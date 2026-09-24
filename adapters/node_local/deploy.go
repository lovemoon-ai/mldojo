// Package node_local runs commands on the API host and deploys mldojo-agent
// through any Executor (local shell or SSH).
package node_local

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	v1 "github.com/lovemoon-ai/mldojo/proto/mldojo/v1"
	"gopkg.in/yaml.v3"
)

// Executor runs a bash command (with optional stdin) on a node.
type Executor interface {
	Run(ctx context.Context, cmd string, stdin io.Reader) (string, error)
	Describe() string
}

// Local executes on the API host.
type Local struct{}

func (Local) Run(ctx context.Context, cmd string, stdin io.Reader) (string, error) {
	c := exec.CommandContext(ctx, "bash", "-lc", cmd)
	var out bytes.Buffer
	c.Stdout, c.Stderr = &out, &out
	if stdin != nil {
		c.Stdin = stdin
	}
	if err := c.Run(); err != nil {
		s := strings.TrimSpace(out.String())
		if len(s) > 800 {
			s = "..." + s[len(s)-800:]
		}
		return out.String(), fmt.Errorf("%w: %s", err, s)
	}
	return out.String(), nil
}

func (Local) Describe() string { return "local" }

// AgentConfig is written to ~/.mldojo/agent/<node>.yaml on the node.
type AgentConfig struct {
	ServerURL    string    `yaml:"server_url"`
	NodeID       string    `yaml:"node_id"`
	Token        string    `yaml:"token"`
	WorkdirRoot  string    `yaml:"workdir_root"`
	DatasetsRoot string    `yaml:"datasets_cache_root"`
	LogFile      string    `yaml:"log_file"`
	Proxy        *v1.Proxy `yaml:"proxy,omitempty"`
}

type DeployOptions struct {
	NodeID        string
	ServerURL     string
	Token         string
	WorkdirRoot   string // default ~/.mldojo/runs
	DatasetsRoot  string // default ~/.mldojo/datasets
	Proxy         *v1.Proxy
	AgentDist     string // directory with mldojo-agent-<os>-<arch>
	PreferSystemd bool   // use systemd --user even without linger (local node)
}

type DeployResult struct {
	OS, Arch, Method, Home string
	StartOutput            string
	DataHome               DataHome // where ~/.mldojo went, and why
}

var idRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

func agentBinary(dist, goos, goarch string) (string, error) {
	cands := []string{}
	if dist != "" {
		cands = append(cands, filepath.Join(dist, fmt.Sprintf("mldojo-agent-%s-%s", goos, goarch)))
	}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		cands = append(cands, filepath.Join(dir, fmt.Sprintf("mldojo-agent-%s-%s", goos, goarch)),
			filepath.Join(dir, "..", "dist", fmt.Sprintf("mldojo-agent-%s-%s", goos, goarch)))
		if goos == runtime.GOOS && goarch == runtime.GOARCH {
			cands = append(cands, filepath.Join(dir, "mldojo-agent"))
		}
	}
	for _, c := range cands {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c, nil
		}
	}
	return "", fmt.Errorf("no mldojo-agent binary for %s/%s (looked in %s); run `make dist`", goos, goarch, strings.Join(cands, ", "))
}

// Deploy uploads the agent binary + config and (re)starts the agent.
func Deploy(ctx context.Context, ex Executor, o DeployOptions) (*DeployResult, error) {
	if !idRe.MatchString(o.NodeID) {
		return nil, fmt.Errorf("invalid node id %q", o.NodeID)
	}
	out, err := ex.Run(ctx, `uname -sm; echo "HOME=$HOME"`, nil)
	if err != nil {
		return nil, fmt.Errorf("probe %s: %w", ex.Describe(), err)
	}
	res := &DeployResult{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "HOME=") {
			res.Home = strings.TrimPrefix(line, "HOME=")
		} else if f := strings.Fields(line); len(f) == 2 && res.OS == "" {
			res.OS = strings.ToLower(f[0])
			switch f[1] {
			case "x86_64", "amd64":
				res.Arch = "amd64"
			case "aarch64", "arm64":
				res.Arch = "arm64"
			default:
				res.Arch = f[1]
			}
		}
	}
	if res.OS == "" || res.Home == "" {
		return nil, fmt.Errorf("unexpected probe output from %s: %q", ex.Describe(), out)
	}
	bin, err := agentBinary(o.AgentDist, res.OS, res.Arch)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(bin)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	want := hex.EncodeToString(sum[:])
	// Before anything creates ~/.mldojo: once it exists it stays where it is.
	res.DataHome = PlanDataHome(ctx, ex)
	if dir := res.DataHome.Dir; dir != "" {
		if err := placeDataHome(ctx, ex, dir); err != nil {
			res.DataHome = DataHome{Reason: fmt.Sprintf("wanted %s but could not link it (%v); staying in home", dir, err)}
		}
	}
	d := `"$HOME/.mldojo/agent"`
	// Note: never use `rm` in commands sent over SSH: some bastions (e.g. a
	// JumpServer) silently drop any command containing `rm -f` (empty output,
	// exit 0). Effects are verified instead of trusting exit codes.
	shaCmd := `mkdir -p ` + d + ` && chmod 700 ` + d + ` && (sha256sum ` + d + `/mldojo-agent 2>/dev/null || shasum -a 256 ` + d + `/mldojo-agent 2>/dev/null) | cut -d' ' -f1`
	have, _ := ex.Run(ctx, shaCmd, nil)
	if strings.TrimSpace(have) != want {
		if _, err := ex.Run(ctx, `cat > `+d+`/mldojo-agent.new && chmod +x `+d+`/mldojo-agent.new && mv -f `+d+`/mldojo-agent.new `+d+`/mldojo-agent`, bytes.NewReader(data)); err != nil {
			return nil, fmt.Errorf("upload agent: %w", err)
		}
		if have, _ = ex.Run(ctx, shaCmd, nil); strings.TrimSpace(have) != want {
			return nil, fmt.Errorf("upload agent to %s: checksum mismatch after upload (a bastion command filter may have dropped the command)", ex.Describe())
		}
	}
	wr, dr := o.WorkdirRoot, o.DatasetsRoot
	if wr == "" {
		// Per node id: several node ids may point at the same host/user.
		wr = res.Home + "/.mldojo/runs/" + o.NodeID
	}
	if dr == "" {
		dr = res.Home + "/.mldojo/datasets"
	}
	wr, dr = strings.Replace(wr, "~", res.Home, 1), strings.Replace(dr, "~", res.Home, 1)
	cfg := AgentConfig{ServerURL: o.ServerURL, NodeID: o.NodeID, Token: o.Token, WorkdirRoot: wr, DatasetsRoot: dr,
		LogFile: res.Home + "/.mldojo/agent/" + o.NodeID + ".log", Proxy: o.Proxy}
	cb, _ := yaml.Marshal(cfg)
	if _, err := ex.Run(ctx, `umask 077 && cat > `+d+`/`+o.NodeID+`.yaml`, bytes.NewReader(cb)); err != nil {
		return nil, fmt.Errorf("write agent config: %w", err)
	}
	prefer := "0"
	if o.PreferSystemd {
		prefer = "1"
	}
	script := strings.NewReplacer("__ID__", o.NodeID, "__PREFER__", prefer).Replace(startScript)
	out, err = ex.Run(ctx, script, nil)
	if err != nil {
		return nil, fmt.Errorf("start agent: %w", err)
	}
	res.Method = "nohup"
	if strings.Contains(out, "METHOD=systemd") {
		res.Method = "systemd"
	}
	res.StartOutput = strings.TrimSpace(out)
	if !strings.Contains(out, "METHOD=") {
		return res, fmt.Errorf("start script did not complete on %s (output: %q)", ex.Describe(), res.StartOutput)
	}
	return res, nil
}

const startScript = `set -e
D="$HOME/.mldojo/agent"; ID="__ID__"; UNIT="mldojo-agent-$ID"
if command -v systemctl >/dev/null 2>&1 && systemctl --user is-active --quiet "$UNIT" 2>/dev/null; then systemctl --user stop "$UNIT" || true; fi
if [ -f "$D/$ID.pid" ]; then
  old=$(cat "$D/$ID.pid"); kill "$old" 2>/dev/null || true
  for _ in $(seq 40); do kill -0 "$old" 2>/dev/null || break; sleep 0.25; done
  kill -9 "$old" 2>/dev/null || true; unlink "$D/$ID.pid" 2>/dev/null || true
fi
use_systemd=0
if command -v systemctl >/dev/null 2>&1 && systemctl --user show-environment >/dev/null 2>&1; then
  if [ "__PREFER__" = 1 ]; then use_systemd=1; fi
  if command -v loginctl >/dev/null 2>&1 && [ "$(loginctl show-user "$(id -un)" -p Linger --value 2>/dev/null)" = yes ]; then use_systemd=1; fi
fi
if [ "$use_systemd" = 1 ]; then
  mkdir -p "$HOME/.config/systemd/user"
  cat > "$HOME/.config/systemd/user/$UNIT.service" <<EOF
[Unit]
Description=MLDojo agent ($ID)
After=network-online.target

[Service]
ExecStart=%h/.mldojo/agent/mldojo-agent --config %h/.mldojo/agent/$ID.yaml
Restart=always
RestartSec=5
TimeoutStopSec=15
# Training runs are started in their own sessions; keep them alive when the agent restarts.
KillMode=process

[Install]
WantedBy=default.target
EOF
  systemctl --user daemon-reload
  systemctl --user enable "$UNIT" >/dev/null 2>&1 || true
  systemctl --user restart "$UNIT"
  echo METHOD=systemd
else
  nohup setsid "$D/mldojo-agent" --config "$D/$ID.yaml" >> "$D/$ID.out" 2>&1 < /dev/null &
  echo $! > "$D/$ID.pid"
  echo METHOD=nohup
fi
`

// ProbeResult describes a node reached without deploying anything.
type ProbeResult struct {
	OS       string   `json:"os"`
	Arch     string   `json:"arch"`
	Hostname string   `json:"hostname"`
	GPUs     string   `json:"gpus"`
	Python   string   `json:"python"`
	Docker   bool     `json:"docker"`
	Conda    bool     `json:"conda"`
	DataHome DataHome `json:"data_home"` // where a deploy would put ~/.mldojo
}

// Probe runs read-only commands on the node.
func Probe(ctx context.Context, ex Executor) (*ProbeResult, error) {
	out, err := ex.Run(ctx, `echo "U=$(uname -sm)"; echo "H=$(hostname)"; echo "P=$(python3 --version 2>&1 | head -1)";
command -v docker >/dev/null 2>&1 && echo D=1; command -v conda >/dev/null 2>&1 && echo C=1;
nvidia-smi -L 2>/dev/null | sed 's/^/G=/'`, nil)
	if err != nil {
		return nil, err
	}
	p := &ProbeResult{}
	var gpus []string
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch k {
		case "U":
			if f := strings.Fields(v); len(f) == 2 {
				p.OS, p.Arch = strings.ToLower(f[0]), f[1]
			}
		case "H":
			p.Hostname = v
		case "P":
			p.Python = v
		case "D":
			p.Docker = true
		case "C":
			p.Conda = true
		case "G":
			if strings.HasPrefix(v, "GPU ") { // skip nvidia-smi error text
				gpus = append(gpus, v)
			}
		}
	}
	p.GPUs = strings.Join(gpus, "; ")
	p.DataHome = PlanDataHome(ctx, ex)
	return p, nil
}

// Stop stops and removes the agent for a node.
func Stop(ctx context.Context, ex Executor, nodeID string) error {
	if !idRe.MatchString(nodeID) {
		return fmt.Errorf("invalid node id %q", nodeID)
	}
	out, err := ex.Run(ctx, strings.ReplaceAll(`D="$HOME/.mldojo/agent"; ID="__ID__"; UNIT="mldojo-agent-$ID"
if command -v systemctl >/dev/null 2>&1; then systemctl --user disable --now "$UNIT" >/dev/null 2>&1 || true; unlink "$HOME/.config/systemd/user/$UNIT.service" 2>/dev/null || true; systemctl --user daemon-reload >/dev/null 2>&1 || true; fi
if [ -f "$D/$ID.pid" ]; then kill "$(cat "$D/$ID.pid")" 2>/dev/null || true; unlink "$D/$ID.pid" 2>/dev/null || true; fi
unlink "$D/$ID.yaml" 2>/dev/null || true
echo STOPPED`, "__ID__", nodeID), nil)
	if err == nil && !strings.Contains(out, "STOPPED") {
		err = fmt.Errorf("stop script did not complete on %s (output: %q)", ex.Describe(), strings.TrimSpace(out))
	}
	return err
}

// LogTail returns the end of the agent log on the node (for error reports).
func LogTail(ctx context.Context, ex Executor, nodeID string) string {
	out, _ := ex.Run(ctx, fmt.Sprintf(`tail -n 20 "$HOME/.mldojo/agent/%s.log" "$HOME/.mldojo/agent/%s.out" 2>/dev/null`, nodeID, nodeID), nil)
	return strings.TrimSpace(out)
}
