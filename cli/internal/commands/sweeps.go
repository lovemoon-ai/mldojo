package commands

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/lovemoon-ai/mldojo/cli/internal/output"
	"github.com/lovemoon-ai/mldojo/recipes"
)

func (a *app) sweepCmd() *cobra.Command {
	c := &cobra.Command{Use: "sweep", Aliases: []string{"sweeps"}, Short: "Hyperparameter search"}
	c.AddCommand(a.sweepCreateCmd(), a.sweepLsCmd(), a.sweepShowCmd(), a.sweepStopCmd())
	return c
}

func (a *app) sweepCreateCmd() *cobra.Command {
	var file string
	c := &cobra.Command{
		Use: "create -f sweep.yaml", Short: "Start a hyperparameter sweep", Args: cobra.NoArgs,
		Example: "  mldojo sweep create -f sweeps/lr.yaml",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if file == "" {
				return usageErr("-f is required")
			}
			b, err := os.ReadFile(file)
			if err != nil {
				return usageErr("%v", err)
			}
			sw, err := recipes.ParseSweep(b)
			if err != nil {
				return usageErr("%v", err)
			}
			// The recipe is inlined here so the server never has to read the
			// submitter's filesystem.
			if sw.RecipeYAML == "" {
				if sw.Recipe == "" {
					return usageErr("sweep needs `recipe: <path>` (or recipe_yaml inline)")
				}
				path := sw.Recipe
				if !filepath.IsAbs(path) {
					path = filepath.Join(filepath.Dir(file), path)
				}
				rb, err := os.ReadFile(path)
				if err != nil {
					return usageErr("recipe %s: %v", sw.Recipe, err)
				}
				sw.RecipeYAML = string(rb)
			}
			if _, err := recipes.Parse([]byte(sw.RecipeYAML)); err != nil {
				return usageErr("recipe: %v", err)
			}
			merged, err := yaml.Marshal(sw)
			if err != nil {
				return err
			}
			var res map[string]any
			if err := a.client().Do(cmd.Context(), "POST", "/sweeps", map[string]string{"yaml": string(merged)}, &res); err != nil {
				return err
			}
			return a.out().Emit(res, func(w ioWriter) {
				fmt.Fprintf(w, "sweep %s/%s started (%s, up to %d runs, %d at a time)\n",
					res["project"], res["name"], sw.Method, sw.Budget.MaxRuns, sw.Budget.MaxParallel)
			})
		},
	}
	c.Flags().StringVarP(&file, "file", "f", "", "sweep definition")
	return c
}

func (a *app) sweepLsCmd() *cobra.Command {
	var project, status string
	c := &cobra.Command{
		Use: "ls", Short: "List sweeps", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			q := url.Values{}
			if project != "" {
				q.Set("project", project)
			}
			if status != "" {
				q.Set("status", status)
			}
			var sweeps []map[string]any
			if err := a.client().Get(cmd.Context(), "/sweeps?"+q.Encode(), &sweeps); err != nil {
				return err
			}
			return a.out().Emit(sweeps, func(w ioWriter) {
				fmt.Fprintln(w, "SWEEP\tSTATUS\tLAUNCHED\tACTIVE")
				for _, s := range sweeps {
					fmt.Fprintf(w, "%v/%v\t%v\t%v\t%v\n", s["project"], s["name"], s["status"], s["launched"], s["active_runs"])
				}
			})
		},
	}
	c.Flags().StringVar(&project, "project", "", "filter by project")
	c.Flags().StringVar(&status, "status", "", "running | done | stopped")
	return c
}

func (a *app) sweepShowCmd() *cobra.Command {
	return &cobra.Command{
		Use: "show <project>/<name>", Short: "Show a sweep and its leaderboard", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, n, err := splitExp(args[0])
			if err != nil {
				return err
			}
			var res struct {
				Name     string `json:"name"`
				Project  string `json:"project"`
				Status   string `json:"status"`
				Launched int    `json:"launched"`
				Metric   string `json:"metric"`
				Goal     string `json:"goal"`
				Results  []struct {
					ID     string         `json:"id"`
					Status string         `json:"status"`
					Params map[string]any `json:"params"`
					Value  *float64       `json:"value"`
				} `json:"results"`
			}
			if err := a.client().Get(cmd.Context(), "/sweeps/"+url.PathEscape(p)+"/"+url.PathEscape(n), &res); err != nil {
				return err
			}
			return a.out().Emit(res, func(w ioWriter) {
				fmt.Fprintf(w, "%s/%s\t%s\tlaunched %d\tbest by %s (%s)\n",
					res.Project, res.Name, res.Status, res.Launched, res.Metric, res.Goal)
				fmt.Fprintln(w, "RUN\tSTATUS\t"+res.Metric+"\tPARAMS")
				for _, r := range res.Results {
					val := "-"
					if r.Value != nil {
						val = fmt.Sprintf("%g", *r.Value)
					}
					var params []string
					for k, v := range r.Params {
						params = append(params, fmt.Sprintf("%s=%v", k, v))
					}
					fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", output.Short(r.ID), r.Status, val, strings.Join(params, " "))
				}
			})
		},
	}
}

func (a *app) sweepStopCmd() *cobra.Command {
	return &cobra.Command{
		Use: "stop <project>/<name>", Short: "Stop launching new runs (running ones continue)",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, n, err := splitExp(args[0])
			if err != nil {
				return err
			}
			var res map[string]any
			if err := a.client().Do(cmd.Context(), "POST",
				"/sweeps/"+url.PathEscape(p)+"/"+url.PathEscape(n)+"/stop", map[string]any{}, &res); err != nil {
				return err
			}
			return a.out().Emit(res, func(w ioWriter) { fmt.Fprintf(w, "stopped %s/%s\n", p, n) })
		},
	}
}
