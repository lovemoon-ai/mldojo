// Package commands implements the mldojo CLI. Every command
// supports --json and returns stable exit codes.
package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"

	"github.com/lovemoon-ai/mldojo/cli/internal/client"
	"github.com/lovemoon-ai/mldojo/cli/internal/output"
	"github.com/lovemoon-ai/mldojo/internal/config"
	"github.com/lovemoon-ai/mldojo/internal/version"
	"github.com/spf13/cobra"
)

type app struct {
	jsonOut bool
	server  string
	token   string
}

func (a *app) client() *client.Client {
	cfg, _ := config.Load()
	server, token := a.server, a.token
	if server == "" && cfg != nil {
		server = cfg.Server
	}
	if token == "" && cfg != nil {
		token = cfg.Token
	}
	return client.New(server, token)
}

func (a *app) out() *output.Printer { return output.New(a.jsonOut) }

// usageErr marks CLI usage mistakes (exit 2).
func usageErr(format string, args ...any) error {
	return &client.Error{Code: "user_error", Msg: fmt.Sprintf(format, args...)}
}

// Execute runs the CLI and returns the process exit code.
func Execute() int {
	a := &app{}
	root := &cobra.Command{
		Use:   "mldojo",
		Short: "MLDojo: organize, run, record and share training runs",
		Long: "MLDojo CLI. Every command supports --json; exit codes: 0 ok, 2 user error, 3 unreachable, 4 backend error, 5 conflict.\n\n" +
			"First time: `mldojo login` (approve the code in a browser).\n" +
			"Full guide (recipes, workflows, examples): <server>/SKILL.md, e.g. " + defaultServer + "/SKILL.md --\n" +
			"open it in a browser (Conductor login), or install it as a Claude Code skill (see its §0).",
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       version.Version,
	}
	root.PersistentFlags().BoolVar(&a.jsonOut, "json", false, "machine-readable JSON output")
	root.PersistentFlags().StringVar(&a.server, "server", "", "API server URL (default: config / MLDOJO_SERVER)")
	root.PersistentFlags().StringVar(&a.token, "token", "", "API token (default: config / MLDOJO_TOKEN)")
	root.AddCommand(
		a.loginCmd(), a.tokenCmd(), a.projectCmd(), a.expCmd(), a.runCmd(), a.compareCmd(), a.sweepCmd(), a.modelCmd(),
		a.nodeCmd(), a.queueCmd(), a.datasetCmd(), a.secretCmd(), a.aiCmd(), a.devCmd(),
		a.usageCmd(), a.gpuCmd(), a.healthCmd(),
	)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err := root.ExecuteContext(ctx)
	if err == nil {
		return 0
	}
	code := client.ExitCode(err)
	var ce *client.Error
	if !errors.As(err, &ce) && (strings.Contains(err.Error(), "unknown command") || strings.Contains(err.Error(), "flag") || strings.Contains(err.Error(), "arg")) {
		code = client.ExitUser
	}
	if errors.Is(err, context.Canceled) {
		code = 130
	}
	if a.jsonOut {
		codeName := "user_error"
		if ce != nil {
			codeName = ce.Code
		}
		fmt.Fprintln(os.Stdout, string(errorJSON(err.Error(), codeName, code)))
	} else {
		fmt.Fprintln(os.Stderr, "error:", err)
	}
	return code
}

func (a *app) healthCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "health",
		Short: "Show API server health",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var h map[string]any
			if err := a.client().Get(cmd.Context(), "/health", &h); err != nil {
				return err
			}
			return a.out().Emit(h, func(w ioWriter) {
				for _, k := range []string{"ok", "version", "db", "secrets", "agents_online"} {
					fmt.Fprintf(w, "%-14s %v\n", k+":", h[k])
				}
				if qp, ok := h["queue_plugins"].(map[string]any); ok {
					names := make([]string, 0, len(qp))
					for n := range qp {
						names = append(names, n)
					}
					sort.Strings(names)
					for _, n := range names {
						fmt.Fprintf(w, "%-14s %v\n", "queue "+n+":", qp[n])
					}
				}
			})
		},
	}
}

// errorJSON renders the --json failure document. It marshals rather than
// formatting with %q: Go quoting escapes control bytes as \x1b, which JSON
// does not accept, and API errors routinely quote a job's coloured stderr.
func errorJSON(msg, code string, exit int) []byte {
	b, err := json.Marshal(map[string]any{"error": msg, "code": code, "exit_code": exit})
	if err != nil {
		b, _ = json.Marshal(map[string]any{"error": "unprintable error", "code": code, "exit_code": exit})
	}
	return b
}
