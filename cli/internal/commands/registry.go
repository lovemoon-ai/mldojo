package commands

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/lovemoon-ai/mldojo/cli/internal/output"
)

func (a *app) modelCmd() *cobra.Command {
	c := &cobra.Command{Use: "model", Aliases: []string{"models"}, Short: "Model registry"}
	c.AddCommand(a.modelRegisterCmd(), a.modelLsCmd(), a.modelShowCmd(), a.modelPromoteCmd())
	return c
}

func (a *app) modelRegisterCmd() *cobra.Command {
	var run, uri, stage, notes string
	c := &cobra.Command{
		Use: "register <project>/<name>", Short: "Register a run's artifact as a model version",
		Args: cobra.ExactArgs(1),
		Example: "  mldojo model register demo/policy --run 9f2c --uri node://gpu-a/home/me/out/best.ckpt\n" +
			"  mldojo model register demo/policy --run 9f2c --uri ... --stage production",
		RunE: func(cmd *cobra.Command, args []string) error {
			p, n, err := splitExp(args[0])
			if err != nil {
				return err
			}
			if uri == "" {
				return usageErr("--uri is required (see `mldojo run artifacts ls <run>`)")
			}
			var res map[string]any
			body := map[string]string{"run_id": run, "uri": uri, "stage": stage, "notes": notes}
			if err := a.client().Do(cmd.Context(), "POST",
				"/models/"+url.PathEscape(p)+"/"+url.PathEscape(n)+"/versions", body, &res); err != nil {
				return err
			}
			return a.out().Emit(res, func(w ioWriter) {
				fmt.Fprintf(w, "%s/%s v%v (%v)\n", p, n, res["version"], res["stage"])
			})
		},
	}
	c.Flags().StringVar(&run, "run", "", "run that produced it (records lineage and its metrics)")
	c.Flags().StringVar(&uri, "uri", "", "artifact uri")
	c.Flags().StringVar(&stage, "stage", "", "none | staging | production | archived")
	c.Flags().StringVar(&notes, "notes", "", "free-form note")
	return c
}

func (a *app) modelLsCmd() *cobra.Command {
	var project string
	c := &cobra.Command{
		Use: "ls", Short: "List models", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			q := ""
			if project != "" {
				q = "?project=" + url.QueryEscape(project)
			}
			var ms []map[string]any
			if err := a.client().Get(cmd.Context(), "/models"+q, &ms); err != nil {
				return err
			}
			return a.out().Emit(ms, func(w ioWriter) {
				fmt.Fprintln(w, "MODEL\tVERSIONS\tOWNER")
				for _, m := range ms {
					fmt.Fprintf(w, "%v/%v\t%v\t%v\n", m["project"], m["name"], m["versions"], m["owner"])
				}
			})
		},
	}
	c.Flags().StringVar(&project, "project", "", "filter by project")
	return c
}

func (a *app) modelShowCmd() *cobra.Command {
	return &cobra.Command{
		Use: "show <project>/<name>", Short: "Show a model's versions", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, n, err := splitExp(args[0])
			if err != nil {
				return err
			}
			var res struct {
				Versions []struct {
					Version int                `json:"version"`
					Stage   string             `json:"stage"`
					RunID   *string            `json:"run_id"`
					URI     string             `json:"uri"`
					SHA256  string             `json:"sha256"`
					Metrics map[string]float64 `json:"metrics"`
				} `json:"versions"`
				UsedBy []struct {
					Version    int    `json:"version"`
					RunID      string `json:"run_id"`
					Project    string `json:"project"`
					Experiment string `json:"experiment"`
					Name       string `json:"name"`
					Status     string `json:"status"`
				} `json:"used_by"`
			}
			if err := a.client().Get(cmd.Context(), "/models/"+url.PathEscape(p)+"/"+url.PathEscape(n), &res); err != nil {
				return err
			}
			return a.out().Emit(res, func(w ioWriter) {
				fmt.Fprintln(w, "VERSION\tSTAGE\tRUN\tSHA\tMETRICS\tURI")
				for _, v := range res.Versions {
					run := "-"
					if v.RunID != nil {
						run = output.Short(*v.RunID)
					}
					sha := "-"
					if v.SHA256 != "" {
						sha = v.SHA256[:8]
					}
					var ms []string
					for k, val := range v.Metrics {
						ms = append(ms, fmt.Sprintf("%s=%g", k, val))
					}
					fmt.Fprintf(w, "v%d\t%s\t%s\t%s\t%s\t%s\n", v.Version, v.Stage, run, sha, strings.Join(ms, " "), v.URI)
				}
				// What was done with it. A version with no consumers has
				// never been evaluated, which is worth seeing at a glance.
				if len(res.UsedBy) > 0 {
					fmt.Fprintln(w, "\nUSED BY\tRUN\tEXPERIMENT\tSTATUS")
					for _, u := range res.UsedBy {
						fmt.Fprintf(w, "v%d\t%s\t%s/%s %s\t%s\n", u.Version, output.Short(u.RunID),
							u.Project, u.Experiment, u.Name, u.Status)
					}
				}
			})
		},
	}
}

func (a *app) modelPromoteCmd() *cobra.Command {
	var stage string
	c := &cobra.Command{
		Use:   "promote <project>/<name> <version>",
		Short: "Move a version to a stage (production and staging hold one version each)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, n, err := splitExp(args[0])
			if err != nil {
				return err
			}
			v := strings.TrimPrefix(args[1], "v")
			if _, err := strconv.Atoi(v); err != nil {
				return usageErr("version must be a number, got %q", args[1])
			}
			var res map[string]any
			if err := a.client().Do(cmd.Context(), "POST",
				"/models/"+url.PathEscape(p)+"/"+url.PathEscape(n)+"/versions/"+v+"/stage",
				map[string]string{"stage": stage}, &res); err != nil {
				return err
			}
			return a.out().Emit(res, func(w ioWriter) {
				fmt.Fprintf(w, "%s/%s v%v -> %v\n", p, n, res["version"], res["stage"])
			})
		},
	}
	c.Flags().StringVar(&stage, "stage", "production", "none | staging | production | archived")
	return c
}
