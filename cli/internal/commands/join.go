package commands

import (
	"bufio"
	"bytes"
	"cmp"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/lovemoon-ai/mldojo/adapters/node_local"
	"github.com/lovemoon-ai/mldojo/cli/internal/client"
	"github.com/lovemoon-ai/mldojo/internal/config"
	v1 "github.com/lovemoon-ai/mldojo/proto/mldojo/v1"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// ---- manual nodes: run on the node itself ------------------------------------

// joinTokenEnv carries the token printed by `mldojo node add --manual`; an
// env var (not a flag) so it stays out of shell history and `ps`.
const joinTokenEnv = "MLDOJO_JOIN_TOKEN"

func (a *app) nodeJoinCmd() *cobra.Command {
	var o struct {
		id, server, workdir, datasets, proxy, noProxy string
		tokenStdin                                    bool
	}
	c := &cobra.Command{
		Use:   "join --id <id> --server <url>",
		Short: "Install the agent here for a manual node (run on the node; needs only outbound access to the server)",
		Long: `Installs and starts the agent on this machine for a node registered with
` + "`mldojo node add --id <id> --manual`" + `. The token printed there is read from
$` + joinTokenEnv + ` (or stdin with --token-stdin) and is never echoed.

Run it again without a token to upgrade the agent in place: the server URL,
token and roots are taken from the existing ~/.mldojo/agent/<id>.yaml, and
running jobs are re-adopted.`,
		Example: `  ` + joinTokenEnv + `=<token> mldojo node join --id gpu-x --server https://mldojo.example.com
  mldojo node join --id gpu-x          # upgrade: reuse the installed config`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if o.id == "" {
				return usageErr("--id is required")
			}
			home, err := os.UserHomeDir()
			if err != nil {
				return err
			}
			agentDir := filepath.Join(home, ".mldojo", "agent")
			var prev node_local.AgentConfig
			if b, err := os.ReadFile(filepath.Join(agentDir, o.id+".yaml")); err == nil {
				_ = yaml.Unmarshal(b, &prev)
			}
			tok := strings.TrimSpace(os.Getenv(joinTokenEnv))
			if o.tokenStdin {
				line, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
				if err != nil && err != io.EOF {
					return err
				}
				tok = strings.TrimSpace(line)
			}
			tok = cmp.Or(tok, prev.Token)
			if tok == "" {
				return usageErr("no join token: set %s to the token printed by `mldojo node add --id %s --manual` (or pass --token-stdin)", joinTokenEnv, o.id)
			}
			server := o.server
			if server == "" {
				server = prev.ServerURL
			}
			if server == "" {
				if cfg, _ := config.Load(); cfg != nil {
					server = cmp.Or(a.server, cfg.Server)
				}
			}
			if server == "" {
				return usageErr("--server is required (the server_url printed by `mldojo node add --manual`)")
			}
			server = strings.TrimRight(server, "/")

			dist, err := os.MkdirTemp("", "mldojo-join-")
			if err != nil {
				return err
			}
			defer os.RemoveAll(dist)
			if err := fetchAgent(cmd, server, o.id, tok, dist); err != nil {
				return err
			}

			proxy := prev.Proxy
			if o.proxy != "" {
				proxy = &v1.Proxy{HTTP: o.proxy, HTTPS: o.proxy}
				if o.noProxy != "" {
					proxy.NoProxy = strings.Split(o.noProxy, ",")
				}
			}
			logFile := filepath.Join(agentDir, o.id+".log")
			var logStart int64
			if st, err := os.Stat(logFile); err == nil {
				logStart = st.Size()
			}
			if !a.jsonOut {
				fmt.Fprintf(os.Stderr, "installing agent for node %s (server %s)...\n", o.id, server)
			}
			res, err := node_local.Deploy(cmd.Context(), node_local.Local{}, node_local.DeployOptions{
				NodeID: o.id, ServerURL: server, Token: tok,
				WorkdirRoot: cmp.Or(o.workdir, prev.WorkdirRoot), DatasetsRoot: cmp.Or(o.datasets, prev.DatasetsRoot),
				Proxy: proxy, AgentDist: dist,
			})
			if err != nil {
				return &client.Error{Code: "backend_error", Msg: "install agent: " + err.Error()}
			}
			// The log file may have moved with ~/.mldojo (data home).
			if res.Home != "" {
				logFile = filepath.Join(res.Home, ".mldojo", "agent", o.id+".log")
			}
			connected, tail := waitAgentConnected(logFile, logStart, 45*time.Second)
			out := map[string]any{"node": o.id, "server_url": server, "method": res.Method, "os": res.OS, "arch": res.Arch,
				"data_home": res.DataHome, "connected": connected}
			if err := a.out().Emit(out, func(w ioWriter) {
				if connected {
					fmt.Fprintf(w, "node %s: agent running (%s) and connected to %s\n", o.id, res.Method, server)
				} else {
					fmt.Fprintf(w, "node %s: agent started (%s) but has not connected to %s yet; agent log:\n%s\n", o.id, res.Method, server, tail)
				}
				if res.DataHome.Dir != "" {
					fmt.Fprintf(w, "~/.mldojo -> %s (%s)\n", res.DataHome.Dir, res.DataHome.Reason)
				}
				if res.Method == "nohup" {
					fmt.Fprintln(w, "note: started with nohup (no systemd --user with linger); it will not come back after a reboot -- re-run `mldojo node join --id "+o.id+"` then")
				}
			}); err != nil {
				return err
			}
			if !connected {
				return &client.Error{Code: "unreachable", Msg: fmt.Sprintf("agent for %s did not connect to %s within 45s", o.id, server)}
			}
			return nil
		},
	}
	f := c.Flags()
	f.StringVar(&o.id, "id", "", "node id, as given to mldojo node add --manual")
	f.StringVar(&o.server, "server", "", "server URL the agent dials (default: the installed agent config, then the CLI config)")
	f.BoolVar(&o.tokenStdin, "token-stdin", false, "read the join token from stdin instead of $"+joinTokenEnv)
	f.StringVar(&o.workdir, "workdir-root", "", "run workdirs (default ~/.mldojo/runs/<id>)")
	f.StringVar(&o.datasets, "datasets-root", "", "dataset cache (default ~/.mldojo/datasets)")
	f.StringVar(&o.proxy, "proxy", "", "http(s) proxy for runs on this node")
	f.StringVar(&o.noProxy, "no-proxy", "", "comma separated no_proxy list")
	return c
}

// fetchAgent downloads this machine's agent binary, authenticated with the
// node's agent token, into dist under the name Deploy looks for.
func fetchAgent(cmd *cobra.Command, server, id, tok, dist string) error {
	name := fmt.Sprintf("mldojo-agent-%s-%s", runtime.GOOS, runtime.GOARCH)
	req, err := http.NewRequestWithContext(cmd.Context(), "GET", server+"/api/v1/agent/binary/"+name, nil)
	if err != nil {
		return usageErr("bad server URL %q: %v", server, err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("X-Mldojo-Node", id)
	resp, err := (&http.Client{Timeout: 10 * time.Minute}).Do(req)
	if err != nil {
		return err // network error: exit 3
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == 401:
		return &client.Error{Status: 401, Code: "user_error", Msg: fmt.Sprintf("server rejected the join token for node %s (wrong id or token, or the node was removed)", id)}
	case resp.StatusCode == 404:
		return &client.Error{Status: 404, Code: "user_error", Msg: fmt.Sprintf("server has no %s (too old for `node join`, or not built for this platform)", name)}
	case resp.StatusCode != 200:
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return &client.Error{Status: resp.StatusCode, Code: "backend_error", Msg: fmt.Sprintf("download agent: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))}
	}
	f, err := os.OpenFile(filepath.Join(dist, name), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		return fmt.Errorf("download agent: %w", err)
	}
	return f.Close()
}

// waitAgentConnected watches the agent log (from offset on) for its first
// successful connection.
func waitAgentConnected(logFile string, offset int64, timeout time.Duration) (bool, string) {
	deadline := time.Now().Add(timeout)
	var fresh []byte
	for {
		if b, err := os.ReadFile(logFile); err == nil && int64(len(b)) >= offset {
			fresh = b[offset:]
			if bytes.Contains(fresh, []byte("msg=connected")) {
				return true, ""
			}
		}
		if time.Now().After(deadline) {
			lines := strings.Split(strings.TrimSpace(string(fresh)), "\n")
			if len(lines) > 15 {
				lines = lines[len(lines)-15:]
			}
			return false, strings.Join(lines, "\n")
		}
		time.Sleep(time.Second)
	}
}

func (a *app) nodeLeaveCmd() *cobra.Command {
	var id string
	c := &cobra.Command{
		Use:   "leave --id <id>",
		Short: "Stop and uninstall this machine's agent for a node (run on the node; runs and datasets are kept)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if id == "" {
				return usageErr("--id is required")
			}
			if err := node_local.Stop(cmd.Context(), node_local.Local{}, id); err != nil {
				return err
			}
			return a.out().Emit(map[string]any{"node": id, "stopped": true}, func(w ioWriter) {
				fmt.Fprintf(w, "agent for %s stopped and its config removed (remove the node on the server with `mldojo node rm %s`)\n", id, id)
			})
		},
	}
	c.Flags().StringVar(&id, "id", "", "node id")
	return c
}
