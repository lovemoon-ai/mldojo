// mldojo-agent runs on every compute node, dials back to the API server and
// executes runs. Usually deployed by `mldojo node add`.
//
//	mldojo-agent --config ~/.mldojo/agent/<node>.yaml
//	mldojo-agent --server http://api:8765 --token T --node-id gpu-a
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/lovemoon-ai/mldojo/agent/internal/exec"
	msync "github.com/lovemoon-ai/mldojo/agent/internal/sync"
	"github.com/lovemoon-ai/mldojo/agent/internal/transport"
	"github.com/lovemoon-ai/mldojo/internal/logging"
	"github.com/lovemoon-ai/mldojo/internal/version"
	"gopkg.in/yaml.v3"
)

type config struct {
	ServerURL    string `yaml:"server_url"`
	NodeID       string `yaml:"node_id"`
	Token        string `yaml:"token"`
	WorkdirRoot  string `yaml:"workdir_root"`
	DatasetsRoot string `yaml:"datasets_cache_root"`
	LogFile      string `yaml:"log_file"`
	// TLS, for an API that terminates it itself with an internal or
	// self-signed certificate, and for mTLS.
	CAFile     string `yaml:"ca_file"`
	ClientCert string `yaml:"client_cert"`
	ClientKey  string `yaml:"client_key"`
}

func expand(p string) string {
	if strings.HasPrefix(p, "~/") {
		h, _ := os.UserHomeDir()
		return filepath.Join(h, p[2:])
	}
	return p
}

func main() {
	var c config
	cfgPath := flag.String("config", "", "agent config file (yaml)")
	flag.StringVar(&c.ServerURL, "server", "", "API server URL")
	flag.StringVar(&c.Token, "token", "", "agent token")
	flag.StringVar(&c.NodeID, "node-id", "", "node id")
	flag.StringVar(&c.WorkdirRoot, "workdir-root", "", "run workdirs (default ~/.mldojo/runs)")
	flag.StringVar(&c.DatasetsRoot, "datasets-root", "", "dataset cache (default ~/.mldojo/datasets)")
	flag.StringVar(&c.LogFile, "log-file", "", "log file (default stderr only)")
	showVersion := flag.Bool("version", false, "print version")
	flag.Parse()
	if *showVersion {
		fmt.Println("mldojo-agent", version.Version)
		return
	}
	if *cfgPath != "" {
		b, err := os.ReadFile(expand(*cfgPath))
		if err != nil {
			fmt.Fprintln(os.Stderr, "mldojo-agent:", err)
			os.Exit(2)
		}
		var fc config
		if err := yaml.Unmarshal(b, &fc); err != nil {
			fmt.Fprintln(os.Stderr, "mldojo-agent: config:", err)
			os.Exit(2)
		}
		// Flags win over the file.
		if c.ServerURL == "" {
			c.ServerURL = fc.ServerURL
		}
		if c.Token == "" {
			c.Token = fc.Token
		}
		if c.NodeID == "" {
			c.NodeID = fc.NodeID
		}
		if c.WorkdirRoot == "" {
			c.WorkdirRoot = fc.WorkdirRoot
		}
		if c.DatasetsRoot == "" {
			c.DatasetsRoot = fc.DatasetsRoot
		}
		if c.LogFile == "" {
			c.LogFile = fc.LogFile
		}
	}
	if c.Token == "" {
		c.Token = os.Getenv("MLDOJO_AGENT_TOKEN")
	}
	if c.ServerURL == "" || c.Token == "" || c.NodeID == "" {
		fmt.Fprintln(os.Stderr, "mldojo-agent: --server, --token and --node-id (or --config) are required")
		os.Exit(2)
	}
	if c.WorkdirRoot == "" {
		c.WorkdirRoot = "~/.mldojo/runs"
	}
	if c.DatasetsRoot == "" {
		c.DatasetsRoot = "~/.mldojo/datasets"
	}
	c.WorkdirRoot, c.DatasetsRoot = expand(c.WorkdirRoot), expand(c.DatasetsRoot)
	os.MkdirAll(c.WorkdirRoot, 0o755)
	os.MkdirAll(c.DatasetsRoot, 0o755)

	var out io.Writer = os.Stderr
	if c.LogFile != "" {
		p := expand(c.LogFile)
		if st, err := os.Stat(p); err == nil && st.Size() > 20<<20 {
			os.Rename(p, p+".1")
		}
		if f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err == nil {
			out = io.MultiWriter(os.Stderr, f)
		}
	}
	logging.Setup(out)

	home, _ := os.UserHomeDir()
	agentDir := filepath.Join(home, ".mldojo", "agent")
	tlsOpts := msync.TLSOptions{
		CAFile:     expand(c.CAFile),
		ClientCert: expand(c.ClientCert),
		ClientKey:  expand(c.ClientKey),
	}
	tlsCfg, err := tlsOpts.TLSConfig()
	if err != nil {
		slog.Error("tls configuration", "err", err)
		os.Exit(1)
	}
	client := &msync.Client{Server: c.ServerURL, Token: c.Token, NodeID: c.NodeID, HTTP: msync.NewHTTP(tlsCfg)}
	a := &transport.Agent{ServerURL: c.ServerURL, Token: c.Token, NodeID: c.NodeID, WorkdirRoot: c.WorkdirRoot,
		DatasetsRoot: c.DatasetsRoot, Client: client, TLS: tlsCfg}
	a.Runs = exec.NewManager(c.WorkdirRoot, agentDir, client, a.Emit)
	a.Runs.Load()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	slog.Info("mldojo-agent starting", "version", version.Version, "node", c.NodeID, "server", c.ServerURL, "workdir_root", c.WorkdirRoot)
	a.Run(ctx)
}
