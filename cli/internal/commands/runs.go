package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/lovemoon-ai/mldojo/cli/internal/client"
	"github.com/lovemoon-ai/mldojo/cli/internal/output"
	v1 "github.com/lovemoon-ai/mldojo/proto/mldojo/v1"
	"github.com/lovemoon-ai/mldojo/recipes"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

func printRuns(w io.Writer, runs []v1.Run) {
	rows := [][]string{}
	for _, r := range runs {
		exit := "-"
		if r.ExitCode != nil {
			exit = strconv.Itoa(*r.ExitCode)
		}
		rows = append(rows, []string{output.Short(r.ID), r.Project + "/" + r.Experiment, r.Name, r.Target, r.Status, exit,
			output.Ago(&r.CreatedAt), output.Dur(r.StartedAt, r.FinishedAt)})
	}
	output.Table(w, []string{"ID", "EXPERIMENT", "NAME", "TARGET", "STATUS", "EXIT", "CREATED", "DURATION"}, rows)
}

func (a *app) runCmd() *cobra.Command {
	c := &cobra.Command{Use: "run", Aliases: []string{"runs"}, Short: "Submit and inspect runs"}
	c.AddCommand(a.runSubmitCmd(), a.runLsCmd(), a.runShowCmd(), a.runLogsCmd(), a.runStatusCmd(), a.runCancelCmd(),
		a.runRerunCmd(), a.runRmCmd(), a.runArtifactsCmd(), a.runMetricsCmd(), a.runGPUCmd(), a.runEventsCmd(), a.runEpisodesCmd())
	return c
}

// parseParam parses k=v where v is YAML (so "[0,1,2]" is a list, "3" an int).
func parseParam(s string) (string, any, error) {
	k, v, ok := strings.Cut(s, "=")
	if !ok || k == "" {
		return "", nil, usageErr("--param expects key=value, got %q", s)
	}
	var val any
	if err := yaml.Unmarshal([]byte(v), &val); err != nil || val == nil {
		val = v
	}
	return k, val, nil
}

func (a *app) runSubmitCmd() *cobra.Command {
	var file, target, project, exp, notes string
	var gpus int
	var matrix, params []string
	var wait, follow, dryRun bool
	c := &cobra.Command{
		Use:   "submit -f recipe.yaml --target <target>",
		Short: "Submit runs from a recipe (one per parameter-matrix point)",
		Example: `  mldojo run submit -f recipe.yaml --target node:gpu-a
  mldojo run submit -f recipe.yaml --target queue:myqueue/gpu-a100 --matrix seed --wait
  mldojo run submit -f recipe.yaml --target node:local --param steps=100 --follow`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if file == "" {
				return usageErr("-f recipe.yaml is required")
			}
			if target == "" {
				return usageErr("--target is required (node:<id> or queue:<backend>/<queue>; see `mldojo node ls`, `mldojo ai free-nodes`)")
			}
			if _, err := recipes.ParseTarget(target); err != nil {
				return usageErr("%v", err)
			}
			raw, err := os.ReadFile(file)
			if err != nil {
				return usageErr("%v", err)
			}
			r, err := recipes.Parse(raw)
			if err != nil {
				return usageErr("%v", err)
			}
			recipeDir, _ := filepath.Abs(filepath.Dir(file))
			if r.Hooks.PreSubmit != "" {
				if r, err = runPreSubmitHook(r, recipeDir); err != nil {
					return err
				}
				raw = []byte(r.YAML())
			}
			req := v1.SubmitRequest{RecipeYAML: string(raw), Target: target, GPUs: gpus, Matrix: matrix, Project: project,
				Experiment: exp, Notes: notes, DryRun: dryRun, Params: map[string]any{}}
			for _, p := range params {
				k, v, err := parseParam(p)
				if err != nil {
					return err
				}
				req.Params[k] = v
			}
			// Validate the matrix locally before uploading code.
			if _, err := r.Expand(matrix, req.Params); err != nil {
				return usageErr("%v", err)
			}
			cl := a.client()
			logf := func(f string, args ...any) {
				if !a.jsonOut {
					fmt.Fprintf(os.Stderr, f+"\n", args...)
				}
			}
			if !dryRun {
				ci, err := prepareCode(cmd.Context(), cl, r, recipeDir, logf)
				if err != nil {
					return err
				}
				req.Code = ci
			}
			var resp v1.SubmitResponse
			if err := cl.Do(cmd.Context(), "POST", "/runs", req, &resp); err != nil {
				return err
			}
			if !wait && !follow {
				return a.out().Emit(resp, func(w ioWriter) {
					fmt.Fprintf(w, "submitted %d run(s) to %s/%s on %s\n", len(resp.Runs), resp.Experiment.Project, resp.Experiment.Name, target)
					printRuns(w, resp.Runs)
				})
			}
			if !a.jsonOut {
				fmt.Fprintf(os.Stderr, "submitted %d run(s): %s\n", len(resp.Runs), runIDs(resp.Runs))
			}
			if follow && len(resp.Runs) == 1 && !a.jsonOut {
				if err := a.streamLogs(cmd.Context(), cl, resp.Runs[0].ID, "all", true, 0); err != nil {
					return err
				}
			}
			final, err := waitRuns(cmd.Context(), cl, resp.Runs)
			if err != nil {
				return err
			}
			resp.Runs = final
			if err := a.out().Emit(resp, func(w ioWriter) { printRuns(w, final) }); err != nil {
				return err
			}
			for _, r := range final {
				if r.Status != v1.PhaseSucceeded {
					return &client.Error{Code: "backend_error", Msg: fmt.Sprintf("run %s %s", output.Short(r.ID), r.Status)}
				}
			}
			return nil
		},
	}
	f := c.Flags()
	f.StringVarP(&file, "file", "f", "", "recipe file")
	f.StringVar(&target, "target", "", "node:<id> | queue:<backend>/<queue>")
	f.IntVar(&gpus, "gpus", 0, "override resources.gpus")
	f.StringSliceVar(&matrix, "matrix", nil, "expand these list parameters (comma separated or repeated; 'all' = every list)")
	f.StringArrayVar(&params, "param", nil, "override a parameter: key=value (YAML value)")
	f.StringVar(&project, "project", "", "override metadata.project")
	f.StringVar(&exp, "exp", "", "override metadata.name")
	f.StringVar(&notes, "notes", "", "free-form notes stored on the runs")
	f.BoolVar(&wait, "wait", false, "wait until all runs finish (exit 4 if any did not succeed)")
	f.BoolVar(&follow, "follow", false, "stream logs (single run) and wait")
	f.BoolVar(&dryRun, "dry-run", false, "resolve and validate without creating runs")
	return c
}

func runIDs(runs []v1.Run) string {
	ids := make([]string, len(runs))
	for i, r := range runs {
		ids[i] = output.Short(r.ID)
	}
	return strings.Join(ids, " ")
}

func waitRuns(ctx context.Context, cl *client.Client, runs []v1.Run) ([]v1.Run, error) {
	out := make([]v1.Run, len(runs))
	copy(out, runs)
	for {
		done := true
		for i := range out {
			if v1.Terminal(out[i].Status) {
				continue
			}
			var r v1.Run
			if err := cl.Get(ctx, "/runs/"+out[i].ID, &r); err != nil {
				return nil, err
			}
			out[i] = r
			if !v1.Terminal(r.Status) {
				done = false
			}
		}
		if done {
			return out, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func (a *app) runLsCmd() *cobra.Command {
	var project, exp, status, target, tag, search, sortBy, order string
	var limit, offset int
	c := &cobra.Command{
		Use: "ls", Short: "List runs", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			q := url.Values{}
			for k, v := range map[string]string{"project": project, "experiment": exp, "status": status,
				"target": target, "tag": tag, "q": search, "sort": sortBy, "order": order} {
				if v != "" {
					q.Set(k, v)
				}
			}
			q.Set("limit", strconv.Itoa(limit))
			if offset > 0 {
				q.Set("offset", strconv.Itoa(offset))
			}
			var runs []v1.Run
			if err := a.client().Get(cmd.Context(), "/runs?"+q.Encode(), &runs); err != nil {
				return err
			}
			return a.out().Emit(runs, func(w ioWriter) { printRuns(w, runs) })
		},
	}
	c.Flags().StringVar(&project, "project", "", "filter by project")
	c.Flags().StringVar(&exp, "exp", "", "filter by experiment")
	c.Flags().StringVar(&status, "status", "", "filter by status (comma separated)")
	c.Flags().StringVar(&target, "target", "", "filter by target")
	c.Flags().StringVar(&tag, "tag", "", "filter by tag")
	c.Flags().StringVar(&search, "search", "", "substring of the run or experiment name")
	c.Flags().StringVar(&sortBy, "sort", "", "sort by created|started|duration|name|status|target")
	c.Flags().StringVar(&order, "order", "", "asc or desc (default desc)")
	c.Flags().IntVar(&limit, "limit", 50, "max runs")
	c.Flags().IntVar(&offset, "offset", 0, "skip this many, for paging past the newest")
	return c
}

func (a *app) getRun(ctx context.Context, id string) (*v1.Run, error) {
	var r v1.Run
	return &r, a.client().Get(ctx, "/runs/"+url.PathEscape(id), &r)
}

func pretty(raw json.RawMessage) string {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return string(raw)
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func (a *app) runShowCmd() *cobra.Command {
	return &cobra.Command{
		Use: "show <run-id>", Short: "Show a run", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := a.getRun(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return a.out().Emit(r, func(w ioWriter) {
				exit := "-"
				if r.ExitCode != nil {
					exit = strconv.Itoa(*r.ExitCode)
				}
				params, _ := json.Marshal(r.Metadata.Params)
				commit := output.Or(r.CodeCommit, "-")
				if r.Metadata.CodeDirty {
					commit += " (+dirty patch)"
				}
				output.KV(w,
					[2]string{"id", r.ID}, [2]string{"experiment", r.Project + "/" + r.Experiment}, [2]string{"name", r.Name},
					[2]string{"target", r.Target}, [2]string{"status", r.Status}, [2]string{"exit code", exit},
					[2]string{"message", output.Or(r.Metadata.Message, "-")},
					[2]string{"created", r.CreatedAt.Local().Format(time.DateTime)}, [2]string{"duration", output.Dur(r.StartedAt, r.FinishedAt)},
					[2]string{"cmd", r.Metadata.Cmd}, [2]string{"params", string(params)},
					[2]string{"code", commit}, [2]string{"env", pretty(r.Env)}, [2]string{"resources", pretty(r.Resources)},
					[2]string{"handle", pretty(r.BackendHandle)}, [2]string{"logs", output.Or(r.Metadata.LogMode, "realtime")},
				)
				// What this run read, and what it produced. Without the
				// first line an evaluation's number cannot be traced back to
				// a checkpoint.
				for _, m := range r.Metadata.Models {
					output.KV(w, [2]string{"model", fmt.Sprintf("%s@%d -> ${%s} = %s", m.Name, m.Version, m.As, m.URI)})
				}
				if r.Metadata.Outputs != nil && r.Metadata.Outputs.Model != "" {
					output.KV(w, [2]string{"registers", r.Metadata.Outputs.Model})
				}
				if r.Metadata.ExternalURL != "" {
					output.KV(w, [2]string{"external", r.Metadata.ExternalURL})
				}
			})
		},
	}
}

func (a *app) runStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use: "status <run-id>", Short: "Print a run's status", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := a.getRun(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			st := map[string]any{"id": r.ID, "status": r.Status, "exit_code": r.ExitCode, "message": r.Metadata.Message,
				"started_at": r.StartedAt, "finished_at": r.FinishedAt}
			return a.out().Emit(st, func(w ioWriter) {
				line := r.Status
				if r.ExitCode != nil {
					line += fmt.Sprintf(" (exit %d)", *r.ExitCode)
				}
				if r.Metadata.Message != "" {
					line += ": " + r.Metadata.Message
				}
				fmt.Fprintln(w, line)
			})
		},
	}
}

func (a *app) runCancelCmd() *cobra.Command {
	return &cobra.Command{
		Use: "cancel <run-id>...", Short: "Cancel runs", Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var out []v1.Run
			for _, id := range args {
				var r v1.Run
				if err := a.client().Do(cmd.Context(), "POST", "/runs/"+url.PathEscape(id)+"/cancel", map[string]any{}, &r); err != nil {
					return err
				}
				out = append(out, r)
			}
			return a.out().Emit(out, func(w ioWriter) {
				for _, r := range out {
					fmt.Fprintf(w, "%s %s\n", output.Short(r.ID), r.Status)
				}
			})
		},
	}
}

func (a *app) runRerunCmd() *cobra.Command {
	var target string
	var params []string
	c := &cobra.Command{
		Use: "rerun <run-id>", Short: "Re-submit a past run with its recorded code, env and params",
		Args: cobra.ExactArgs(1),
		Example: "  mldojo run rerun 9f2c            # same target, same params\n" +
			"  mldojo run rerun 9f2c --target node:gpu-a\n" +
			"  mldojo run rerun 9f2c --param lr=3e-4",
		RunE: func(cmd *cobra.Command, args []string) error {
			body := map[string]any{}
			if target != "" {
				body["target"] = target
			}
			if len(params) > 0 {
				over := map[string]any{}
				for _, p := range params {
					k, v, err := parseParam(p)
					if err != nil {
						return err
					}
					over[k] = v
				}
				body["params"] = over
			}
			var res v1.SubmitResponse
			if err := a.client().Do(cmd.Context(), "POST", "/runs/"+url.PathEscape(args[0])+"/rerun", body, &res); err != nil {
				return err
			}
			return a.out().Emit(res, func(w ioWriter) {
				for _, r := range res.Runs {
					fmt.Fprintf(w, "%s %s %s\n", output.Short(r.ID), r.Name, r.Target)
				}
			})
		},
	}
	c.Flags().StringVar(&target, "target", "", "run it somewhere else (default: the original target)")
	c.Flags().StringArrayVar(&params, "param", nil, "override a parameter, key=value (repeatable)")
	return c
}

func (a *app) runRmCmd() *cobra.Command {
	var force, purge bool
	c := &cobra.Command{
		Use: "rm <run-id>...", Short: "Delete runs and their logs, metrics and artifacts index",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			q := url.Values{}
			if force {
				q.Set("force", "1")
			}
			if purge {
				q.Set("purge_node", "1")
			}
			suffix := ""
			if len(q) > 0 {
				suffix = "?" + q.Encode()
			}
			var out []map[string]any
			for _, id := range args {
				var res map[string]any
				if err := a.client().Do(cmd.Context(), "DELETE", "/runs/"+url.PathEscape(id)+suffix, nil, &res); err != nil {
					return err
				}
				out = append(out, res)
			}
			return a.out().Emit(out, func(w ioWriter) {
				for _, r := range out {
					line := "deleted " + output.Short(fmt.Sprint(r["deleted"]))
					if n, ok := r["purged_node"]; ok {
						line += fmt.Sprintf(" (workdir removed on %v)", n)
					}
					if e, ok := r["purge_error"]; ok {
						line += fmt.Sprintf(" (node workdir NOT removed: %v)", e)
					}
					fmt.Fprintln(w, line)
				}
			})
		},
	}
	c.Flags().BoolVar(&force, "force", false, "cancel the run first if it is still active")
	c.Flags().BoolVar(&purge, "purge-node", false, "also delete the run's workdir on its node (checkpoints included)")
	return c
}

func (a *app) runLogsCmd() *cobra.Command {
	var follow bool
	var stream string
	var tail int64
	c := &cobra.Command{
		Use: "logs <run-id>", Short: "Print (or follow) run logs", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := a.getRun(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if r.Metadata.LogMode == "near-realtime" && follow && !a.jsonOut {
				fmt.Fprintln(os.Stderr, "(queue job · near-realtime: logs are polled every 5-10s)")
			}
			return a.streamLogs(cmd.Context(), a.client(), r.ID, stream, follow, tail)
		},
	}
	c.Flags().BoolVarP(&follow, "follow", "f", false, "follow until the run ends")
	c.Flags().StringVar(&stream, "stream", "stdout", "stdout | stderr | system | all")
	c.Flags().Int64Var(&tail, "tail", 0, "start from the last N bytes")
	return c
}

func (a *app) streamLogs(ctx context.Context, cl *client.Client, id, stream string, follow bool, tail int64) error {
	q := url.Values{"stream": {stream}, "follow": {map[bool]string{true: "1", false: "0"}[follow]}}
	if tail > 0 {
		q.Set("tail", strconv.FormatInt(tail, 10))
	}
	ws, err := cl.WS(ctx, "/runs/"+url.PathEscape(id)+"/logs/ws", q)
	if err != nil {
		return err
	}
	defer ws.Close()
	go func() { <-ctx.Done(); ws.Close() }()
	enc := json.NewEncoder(os.Stdout)
	for {
		var f v1.Frame
		if err := ws.ReadJSON(&f); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// The server always ends a stream with an explicit eof frame,
			// so getting here means the connection dropped. Reporting
			// success would hand the caller truncated logs that look
			// complete.
			return &client.Error{Code: "unreachable", Msg: fmt.Sprintf("log stream for %s ended early: %v", id, err)}
		}
		switch f.Kind {
		case "log":
			var p v1.LogPayload
			json.Unmarshal(f.Payload, &p)
			if a.jsonOut {
				enc.Encode(p)
				continue
			}
			// Everything goes to stdout (like kubectl logs) so it pipes.
			w := os.Stdout
			if stream == "all" && p.Stream == v1.StreamSystem {
				for _, line := range strings.SplitAfter(p.Data, "\n") {
					if line != "" {
						fmt.Fprint(w, "[mldojo] "+line)
					}
				}
				continue
			}
			fmt.Fprint(w, p.Data)
		case "eof":
			return nil
		case "error":
			return &client.Error{Code: "backend_error", Msg: string(f.Payload)}
		}
	}
}

func (a *app) runArtifactsCmd() *cobra.Command {
	c := &cobra.Command{Use: "artifacts", Short: "List or fetch run artifacts"}
	ls := &cobra.Command{
		Use: "ls <run-id>", Short: "List artifacts (rescans the node)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var arts []v1.Artifact
			if err := a.client().Do(cmd.Context(), "POST", "/runs/"+url.PathEscape(args[0])+"/artifacts/refresh", map[string]any{}, &arts); err != nil {
				return err
			}
			return a.out().Emit(arts, func(w ioWriter) {
				rows := [][]string{}
				for _, x := range arts {
					rows = append(rows, []string{x.Kind, output.Bytes(x.SizeBytes), x.URI})
				}
				output.Table(w, []string{"KIND", "SIZE", "URI"}, rows)
			})
		},
	}
	var dest string
	get := &cobra.Command{
		Use: "get <run-id> <uri>", Short: "Download an artifact", Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			resp, err := a.client().Raw(cmd.Context(), "GET", "/runs/"+url.PathEscape(args[0])+"/artifacts/raw?uri="+url.QueryEscape(args[1]), nil, "")
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			target := dest
			if st, err := os.Stat(dest); err == nil && st.IsDir() {
				target = filepath.Join(dest, filepath.Base(args[1]))
			}
			f, err := os.Create(target)
			if err != nil {
				return err
			}
			n, err := io.Copy(f, resp.Body)
			f.Close()
			if err != nil {
				return err
			}
			return a.out().Emit(map[string]any{"path": target, "bytes": n}, func(w ioWriter) {
				fmt.Fprintf(w, "saved %s (%s)\n", target, output.Bytes(n))
			})
		},
	}
	get.Flags().StringVar(&dest, "dest", ".", "destination file or directory")
	c.AddCommand(ls, get)
	return c
}

func (a *app) runMetricsCmd() *cobra.Command {
	var key, since string
	c := &cobra.Command{
		Use: "metrics <run-id>", Short: "Print run metrics", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			q := url.Values{}
			if key != "" {
				q.Set("key", key)
			}
			if since != "" {
				q.Set("since_step", strings.TrimPrefix(since, "step:"))
			}
			var res struct {
				Keys   []string         `json:"keys"`
				Points []v1.MetricPoint `json:"points"`
			}
			if err := a.client().Get(cmd.Context(), "/runs/"+url.PathEscape(args[0])+"/metrics?"+q.Encode(), &res); err != nil {
				return err
			}
			return a.out().Emit(res, func(w ioWriter) {
				if key != "" {
					rows := [][]string{}
					for _, p := range res.Points {
						rows = append(rows, []string{p.Key, strconv.FormatInt(p.Step, 10), fmt.Sprintf("%.6g", p.Value)})
					}
					output.Table(w, []string{"KEY", "STEP", "VALUE"}, rows)
					return
				}
				type agg struct {
					n          int
					last       v1.MetricPoint
					minV, maxV float64
				}
				m := map[string]*agg{}
				for _, p := range res.Points {
					g := m[p.Key]
					if g == nil {
						g = &agg{minV: p.Value, maxV: p.Value}
						m[p.Key] = g
					}
					g.n++
					if p.Step >= g.last.Step {
						g.last = p
					}
					g.minV, g.maxV = min(g.minV, p.Value), max(g.maxV, p.Value)
				}
				keys := make([]string, 0, len(m))
				for k := range m {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				rows := [][]string{}
				for _, k := range keys {
					g := m[k]
					rows = append(rows, []string{k, strconv.Itoa(g.n), strconv.FormatInt(g.last.Step, 10), fmt.Sprintf("%.6g", g.last.Value),
						fmt.Sprintf("%.6g", g.minV), fmt.Sprintf("%.6g", g.maxV)})
				}
				output.Table(w, []string{"KEY", "POINTS", "LAST STEP", "LAST", "MIN", "MAX"}, rows)
			})
		},
	}
	c.Flags().StringVar(&key, "key", "", "only these keys (comma separated)")
	c.Flags().StringVar(&since, "since", "", "step:N")
	return c
}

func (a *app) compareCmd() *cobra.Command {
	c := a.compareRunsCmd()
	c.AddCommand(a.compareEpisodesCmd())
	return c
}

func (a *app) compareRunsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "compare <run-a> <run-b> [run-c ...]",
		Short: "Compare runs: code and env for two, params and metrics for any number",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			// The A/B endpoint is the only source of the code and env diff,
			// so it stays the path for exactly two runs.
			if len(args) > 2 {
				return a.compareMany(cmd.Context(), args)
			}
			var c map[string]any
			var raw json.RawMessage
			q := url.Values{"a": {args[0]}, "b": {args[1]}}
			if err := a.client().Get(cmd.Context(), "/compare?"+q.Encode(), &raw); err != nil {
				return err
			}
			json.Unmarshal(raw, &c)
			var typed struct {
				A, B v1.Run
				Code struct {
					SameCommit bool   `json:"same_commit"`
					CommitA    string `json:"commit_a"`
					CommitB    string `json:"commit_b"`
					DirtyA     bool   `json:"dirty_a"`
					DirtyB     bool   `json:"dirty_b"`
					SamePatch  bool   `json:"same_patch"`
				}
				Metrics []struct {
					Key         string
					A, B, Delta *v1.Float
					StepA       *int64 `json:"step_a"`
					StepB       *int64 `json:"step_b"`
				}
				Env []struct {
					Path string
					A, B any
				}
			}
			json.Unmarshal(raw, &typed)
			return a.out().Emit(c, func(w ioWriter) {
				fmt.Fprintf(w, "A: %s %s (%s)\nB: %s %s (%s)\n\n", output.Short(typed.A.ID), typed.A.Name, typed.A.Status,
					output.Short(typed.B.ID), typed.B.Name, typed.B.Status)
				fmt.Fprintf(w, "code: same_commit=%v same_patch=%v  A=%s dirty=%v  B=%s dirty=%v\n\n", typed.Code.SameCommit, typed.Code.SamePatch,
					output.Short(typed.Code.CommitA), typed.Code.DirtyA, output.Short(typed.Code.CommitB), typed.Code.DirtyB)
				f := func(p *v1.Float) string {
					if p == nil {
						return "-"
					}
					return fmt.Sprintf("%.6g", float64(*p))
				}
				rows := [][]string{}
				for _, m := range typed.Metrics {
					rows = append(rows, []string{m.Key, f(m.A), f(m.B), f(m.Delta)})
				}
				output.Table(w, []string{"METRIC", "A", "B", "DELTA"}, rows)
				fmt.Fprintln(w)
				rows = [][]string{}
				for _, e := range typed.Env {
					rows = append(rows, []string{e.Path, fmt.Sprint(e.A), fmt.Sprint(e.B)})
				}
				output.Table(w, []string{"SETTING", "A", "B"}, rows)
			})
		},
	}
}

// compareMany prints the params-and-metrics table for any number of runs.
func (a *app) compareMany(ctx context.Context, ids []string) error {
	var res struct {
		Runs []struct {
			ID, Name, Status, Target string
			Params                   map[string]any
			Latest                   map[string]v1.Float
		} `json:"runs"`
		MetricKeys []string `json:"metric_keys"`
		ParamKeys  []string `json:"param_keys"`
	}
	q := url.Values{"runs": {strings.Join(ids, ",")}, "max_points": {"1"}}
	if err := a.client().Get(ctx, "/compare/many?"+q.Encode(), &res); err != nil {
		return err
	}
	return a.out().Emit(res, func(w ioWriter) {
		header := append([]string{"RUN", "STATUS"}, res.ParamKeys...)
		header = append(header, res.MetricKeys...)
		fmt.Fprintln(w, strings.Join(header, "\t"))
		for _, r := range res.Runs {
			name := r.Name
			if name == "" {
				name = output.Short(r.ID)
			}
			row := []string{name, r.Status}
			for _, k := range res.ParamKeys {
				row = append(row, fmt.Sprint(orDash(r.Params[k])))
			}
			for _, k := range res.MetricKeys {
				v, ok := r.Latest[k]
				if !ok {
					row = append(row, "-")
					continue
				}
				row = append(row, fmt.Sprint(v))
			}
			fmt.Fprintln(w, strings.Join(row, "\t"))
		}
	})
}

func orDash(v any) any {
	if v == nil {
		return "-"
	}
	return v
}

type runGPUResp struct {
	RunID   string       `json:"run_id"`
	Live    []v1.GPUStat `json:"live"`
	History []gpuSeries  `json:"history"`
}

// runGPUCmd answers "is my job actually using the card". For a finished run
// the live view is misleading -- those indices may belong to someone else by
// now -- so the server returns the samples taken while it ran.
func (a *app) runGPUCmd() *cobra.Command {
	var maxPts int
	c := &cobra.Command{
		Use: "gpu <run-id>", Short: "GPU usage of a run, live and over its lifetime",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			q := url.Values{"max_points": {strconv.Itoa(maxPts)}}
			var r runGPUResp
			if err := a.client().Get(cmd.Context(),
				"/runs/"+url.PathEscape(args[0])+"/gpu?"+q.Encode(), &r); err != nil {
				return err
			}
			return a.out().Emit(r, func(w ioWriter) {
				if len(r.Live) > 0 {
					printGPU(w, r.Live)
					fmt.Fprintln(w)
				}
				printGPUHistory(w, r.History)
			})
		},
	}
	c.Flags().IntVar(&maxPts, "max-points", 500, "points per card")
	return c
}

// runEventsCmd exposes the run's timeline -- placement, spawn, anomalies,
// failures. The web has had it all along; an agent debugging a run had to
// read the logs and guess.
func (a *app) runEventsCmd() *cobra.Command {
	return &cobra.Command{
		Use: "events <run-id>", Short: "State transitions and anomalies recorded for a run",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var evs []v1.RunEvent
			if err := a.client().Get(cmd.Context(), "/runs/"+url.PathEscape(args[0])+"/events", &evs); err != nil {
				return err
			}
			return a.out().Emit(evs, func(w ioWriter) {
				if len(evs) == 0 {
					fmt.Fprintln(w, "no events")
					return
				}
				rows := [][]string{}
				for _, e := range evs {
					payload := strings.TrimSpace(string(e.Payload))
					if payload == "null" || payload == "{}" {
						payload = ""
					}
					if len(payload) > 100 {
						payload = payload[:100] + "…"
					}
					rows = append(rows, []string{e.TS.Local().Format("01-02 15:04:05"), e.Kind, payload})
				}
				output.Table(w, []string{"TIME", "KIND", "DETAIL"}, rows)
			})
		},
	}
}
