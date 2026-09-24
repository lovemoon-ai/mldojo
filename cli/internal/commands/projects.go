package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/lovemoon-ai/mldojo/cli/internal/client"
	"github.com/lovemoon-ai/mldojo/cli/internal/output"
	"github.com/lovemoon-ai/mldojo/internal/config"
	v1 "github.com/lovemoon-ai/mldojo/proto/mldojo/v1"
	"github.com/lovemoon-ai/mldojo/recipes"
	"github.com/spf13/cobra"
)

type ioWriter = io.Writer

func (a *app) loginCmd() *cobra.Command {
	var server, token string
	c := &cobra.Command{
		Use:   "login",
		Short: "Sign in and save the server URL and token to ~/.mldojo/config.yaml",
		Long: "Sign in and save the server URL and token to ~/.mldojo/config.yaml. Without --token, the token is read from\n" +
			"MLDOJO_TOKEN / the local config / `mldojo-api token issue` on the server host; failing those, it prints a\n" +
			"link and a code to approve in a browser (Conductor login), so no token is ever typed or shown.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			f, err := config.LoadRaw()
			if err != nil {
				return err
			}
			if server == "" {
				server = a.server
			}
			if server == "" {
				server = os.Getenv("MLDOJO_SERVER")
			}
			if server == "" {
				server = f.Server
			}
			if server == "" {
				server = defaultServer
			}
			if token == "" {
				token = a.token
			}
			if token == "" {
				token = os.Getenv("MLDOJO_TOKEN")
			}
			explicit := token != ""
			if token == "" {
				token = f.Token
			}
			if token == "" {
				if out, err := apiBinary("token", "issue"); err == nil {
					token = strings.TrimSpace(out)
				}
			}
			var projects []v1.Project
			err = errNoToken
			if token != "" {
				err = client.New(server, token).Get(cmd.Context(), "/projects", &projects)
			}
			var ce *client.Error
			if !explicit && (err == errNoToken || errors.As(err, &ce) && ce.Status == 401) {
				// Nothing usable saved: sign in through the browser instead.
				if token, err = deviceLogin(cmd.Context(), server); err == nil {
					err = client.New(server, token).Get(cmd.Context(), "/projects", &projects)
				}
			}
			if err != nil {
				return err
			}
			f.Server, f.Token = strings.TrimRight(server, "/"), token
			if err := config.Save(f); err != nil {
				return err
			}
			return a.out().Emit(map[string]any{"ok": true, "server": f.Server, "config": config.Path()}, func(w ioWriter) {
				fmt.Fprintf(w, "logged in to %s (%d projects); saved to %s\n", f.Server, len(projects), config.Path())
			})
		},
	}
	c.Flags().StringVar(&server, "server", "", "API server URL, e.g. http://192.0.2.10:8765 (default: saved config, else "+defaultServer+")")
	c.Flags().StringVar(&token, "token", "", "API token")
	return c
}

// defaultServer is what `mldojo login` connects to without --server. A site
// bakes its own in at build time: MLDOJO_DEFAULT_SERVER in deploy/site.env
// (see the Makefile).
var defaultServer = "http://localhost:8765"

var errNoToken = errors.New("no token")

// deviceLogin gets a token by having the user approve a short code in a
// browser signed in with SSO, so no token is pasted into a terminal.
func deviceLogin(ctx context.Context, server string) (string, error) {
	cl := client.New(server, "")
	host, _ := os.Hostname()
	var start struct {
		DeviceCode      string `json:"device_code"`
		UserCode        string `json:"user_code"`
		VerificationURL string `json:"verification_url"`
		ExpiresIn       int    `json:"expires_in"`
		Interval        int    `json:"interval"`
	}
	if err := cl.Do(ctx, "POST", "/auth/device/start", map[string]string{"hostname": host}, &start); err != nil {
		return "", err
	}
	prompt := "Open this link in a browser, sign in with Conductor, check the code and approve:\n\n  %s\n\n  Code: %s\n\nWaiting for approval (valid for %d minutes)...\n"
	if chineseLocale() {
		prompt = "在浏览器打开下面的链接，用 Conductor 登录后核对校验码并批准：\n\n  %s\n\n  校验码：%s\n\n等待批准（%d 分钟内有效）...\n"
	}
	fmt.Fprintf(os.Stderr, prompt, start.VerificationURL, start.UserCode, start.ExpiresIn/60)
	interval := time.Duration(max(start.Interval, 1)) * time.Second
	for deadline := time.Now().Add(time.Duration(start.ExpiresIn) * time.Second); time.Now().Before(deadline); {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(interval):
		}
		var poll struct{ Status, Token string }
		if err := cl.Do(ctx, "POST", "/auth/device/poll", map[string]string{"device_code": start.DeviceCode}, &poll); err != nil {
			return "", err
		}
		if poll.Status == "approved" && poll.Token != "" {
			return poll.Token, nil
		}
	}
	return "", usageErr("login request expired; run `mldojo login` again")
}

// apiBinary runs mldojo-api (next to this binary or on PATH).
func apiBinary(args ...string) (string, error) {
	bin := "mldojo-api"
	if exe, err := os.Executable(); err == nil {
		if p := filepath.Join(filepath.Dir(exe), "mldojo-api"); fileExists(p) {
			bin = p
		}
	}
	out, err := exec.Command(bin, args...).Output()
	return string(out), err
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func (a *app) tokenCmd() *cobra.Command {
	c := &cobra.Command{Use: "token", Short: "API token helpers"}
	c.AddCommand(&cobra.Command{
		Use:   "issue",
		Short: "Print the API token (runs `mldojo-api token issue` on the server host)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out, err := apiBinary("token", "issue")
			if err != nil {
				return usageErr("run this on the API server host (mldojo-api not found or failed: %v)", err)
			}
			tok := strings.TrimSpace(out)
			return a.out().Emit(map[string]string{"token": tok}, func(w ioWriter) { fmt.Fprintln(w, tok) })
		},
	})
	return c
}

func (a *app) projectLimitCmd() *cobra.Command {
	var max int
	c := &cobra.Command{
		Use: "limit <name>", Short: "Cap a project's concurrent runs (0 = unlimited)",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var p v1.Project
			if err := a.client().Do(cmd.Context(), "PATCH", "/projects/"+url.PathEscape(args[0])+"/limit",
				map[string]int{"max_concurrent_runs": max}, &p); err != nil {
				return err
			}
			return a.out().Emit(p, func(w ioWriter) {
				if p.MaxConcurrentRuns == 0 {
					fmt.Fprintf(w, "%s: unlimited concurrent runs\n", p.Name)
					return
				}
				fmt.Fprintf(w, "%s: at most %d concurrent runs\n", p.Name, p.MaxConcurrentRuns)
			})
		},
	}
	c.Flags().IntVar(&max, "max-runs", 0, "concurrent run limit, 0 for unlimited")
	return c
}

func (a *app) projectCmd() *cobra.Command {
	c := &cobra.Command{Use: "project", Aliases: []string{"projects"}, Short: "Manage projects"}
	var desc string
	create := &cobra.Command{
		Use: "create <name>", Short: "Create a project", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var p v1.Project
			if err := a.client().Do(cmd.Context(), "POST", "/projects", map[string]string{"name": args[0], "description": desc}, &p); err != nil {
				return err
			}
			return a.out().Emit(p, func(w ioWriter) { fmt.Fprintf(w, "created project %s\n", p.Name) })
		},
	}
	create.Flags().StringVar(&desc, "description", "", "description")
	ls := &cobra.Command{
		Use: "ls", Short: "List projects", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var ps []v1.Project
			if err := a.client().Get(cmd.Context(), "/projects", &ps); err != nil {
				return err
			}
			return a.out().Emit(ps, func(w ioWriter) {
				rows := [][]string{}
				for _, p := range ps {
					rows = append(rows, []string{p.Name, fmt.Sprint(p.ExperimentCount), fmt.Sprint(p.RunCount), output.Ago(&p.UpdatedAt), p.Description})
				}
				output.Table(w, []string{"NAME", "EXPERIMENTS", "RUNS", "UPDATED", "DESCRIPTION"}, rows)
			})
		},
	}
	var force bool
	rm := &cobra.Command{
		Use: "rm <name>", Short: "Delete a project", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			q := ""
			if force {
				q = "?force=1"
			}
			if err := a.client().Do(cmd.Context(), "DELETE", "/projects/"+url.PathEscape(args[0])+q, nil, nil); err != nil {
				return err
			}
			return a.out().Emit(map[string]bool{"ok": true}, func(w ioWriter) { fmt.Fprintf(w, "deleted project %s\n", args[0]) })
		},
	}
	rm.Flags().BoolVar(&force, "force", false, "also delete its experiments and runs")
	c.AddCommand(create, ls, rm, a.projectLimitCmd())
	return c
}

func splitExp(s string) (string, string, error) {
	p, e, ok := strings.Cut(s, "/")
	if !ok || p == "" || e == "" {
		return "", "", usageErr("expected <project>/<experiment>, got %q", s)
	}
	return p, e, nil
}

func (a *app) expCmd() *cobra.Command {
	c := &cobra.Command{Use: "exp", Aliases: []string{"experiment", "experiments"}, Short: "Manage experiments"}
	var file, name, desc string
	create := &cobra.Command{
		Use: "create <project> -f recipe.yaml", Short: "Create an experiment from a recipe", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			body := map[string]any{"name": name, "description": desc}
			if file != "" {
				b, err := os.ReadFile(file)
				if err != nil {
					return usageErr("%v", err)
				}
				if _, err := recipes.Parse(b); err != nil {
					return usageErr("%v", err)
				}
				body["recipe_yaml"] = string(b)
			}
			var e v1.Experiment
			if err := a.client().Do(cmd.Context(), "POST", "/projects/"+url.PathEscape(args[0])+"/experiments", body, &e); err != nil {
				return err
			}
			return a.out().Emit(e, func(w ioWriter) { fmt.Fprintf(w, "created experiment %s/%s\n", e.Project, e.Name) })
		},
	}
	create.Flags().StringVarP(&file, "file", "f", "", "recipe file")
	create.Flags().StringVar(&name, "name", "", "experiment name (default: recipe metadata.name)")
	create.Flags().StringVar(&desc, "description", "", "description")
	ls := &cobra.Command{
		Use: "ls <project>", Short: "List experiments", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var es []v1.Experiment
			if err := a.client().Get(cmd.Context(), "/projects/"+url.PathEscape(args[0])+"/experiments", &es); err != nil {
				return err
			}
			return a.out().Emit(es, func(w ioWriter) {
				rows := [][]string{}
				for _, e := range es {
					rows = append(rows, []string{e.Name, runCounts(e.RunCounts), strings.Join(e.Tags, ","), output.Ago(&e.UpdatedAt)})
				}
				output.Table(w, []string{"NAME", "RUNS", "TAGS", "UPDATED"}, rows)
			})
		},
	}
	show := &cobra.Command{
		Use: "show <project>/<exp>", Short: "Show an experiment and its runs", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, e, err := splitExp(args[0])
			if err != nil {
				return err
			}
			var exp v1.Experiment
			if err := a.client().Get(cmd.Context(), "/projects/"+url.PathEscape(p)+"/experiments/"+url.PathEscape(e), &exp); err != nil {
				return err
			}
			var runs []v1.Run
			q := url.Values{"project": {p}, "experiment": {e}}
			if err := a.client().Get(cmd.Context(), "/runs?"+q.Encode(), &runs); err != nil {
				return err
			}
			return a.out().Emit(map[string]any{"experiment": exp, "runs": runs}, func(w ioWriter) {
				output.KV(w, [2]string{"experiment", exp.Project + "/" + exp.Name}, [2]string{"tags", strings.Join(exp.Tags, ",")},
					[2]string{"runs", runCounts(exp.RunCounts)}, [2]string{"updated", output.Ago(&exp.UpdatedAt)})
				fmt.Fprintln(w)
				printRuns(w, runs)
			})
		},
	}
	var force bool
	rm := &cobra.Command{
		Use: "rm <project>/<exp>", Short: "Delete an experiment", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, e, err := splitExp(args[0])
			if err != nil {
				return err
			}
			q := ""
			if force {
				q = "?force=1"
			}
			if err := a.client().Do(cmd.Context(), "DELETE", "/projects/"+url.PathEscape(p)+"/experiments/"+url.PathEscape(e)+q, nil, nil); err != nil {
				return err
			}
			return a.out().Emit(map[string]bool{"ok": true}, func(w ioWriter) { fmt.Fprintf(w, "deleted %s\n", args[0]) })
		},
	}
	rm.Flags().BoolVar(&force, "force", false, "also delete its runs")
	c.AddCommand(create, ls, show, rm)
	return c
}

func runCounts(m map[string]int) string {
	if len(m) == 0 {
		return "0"
	}
	var parts []string
	for _, k := range []string{"running", "queued", "starting", "succeeded", "failed", "cancelled"} {
		if n := m[k]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, k))
		}
	}
	return strings.Join(parts, ", ")
}

// chineseLocale reports whether the user's locale (LC_ALL, LC_MESSAGES, LANG,
// first one set wins) is Chinese; interactive prompts then use Chinese.
func chineseLocale() bool {
	for _, k := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		if v := os.Getenv(k); v != "" {
			return strings.HasPrefix(strings.ToLower(v), "zh")
		}
	}
	return false
}
