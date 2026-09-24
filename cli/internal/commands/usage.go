package commands

import (
	"fmt"
	"net/url"
	"strconv"
	"time"

	"github.com/lovemoon-ai/mldojo/cli/internal/output"
	v1 "github.com/lovemoon-ai/mldojo/proto/mldojo/v1"
	"github.com/spf13/cobra"
)

type usageRow struct {
	Key       string  `json:"key"`
	GPUHours  float64 `json:"gpu_hours"`
	BusyHours float64 `json:"busy_hours"`
	Samples   int     `json:"samples"`
}

type usageResp struct {
	By    string     `json:"by"`
	Since time.Time  `json:"since"`
	Until time.Time  `json:"until"`
	Rows  []usageRow `json:"rows"`
}

// usageCmd answers who spent the cluster's time, and how much of that time
// the cards were actually working. The ingredients were always in the
// database; nothing ever added them up.
func (a *app) usageCmd() *cobra.Command {
	var by, since, until string
	c := &cobra.Command{
		Use:   "usage",
		Short: "GPU hours by project, run, submitter or node",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			q := url.Values{"by": {by}, "since": {since}}
			if until != "" {
				q.Set("until", until)
			}
			var u usageResp
			if err := a.client().Get(cmd.Context(), "/usage?"+q.Encode(), &u); err != nil {
				return err
			}
			return a.out().Emit(u, func(w ioWriter) {
				if len(u.Rows) == 0 {
					fmt.Fprintln(w, "no GPU samples in this window")
					return
				}
				rows := [][]string{}
				for _, r := range u.Rows {
					busy := 0.0
					if r.GPUHours > 0 {
						busy = r.BusyHours / r.GPUHours * 100
					}
					rows = append(rows, []string{r.Key, fmt.Sprintf("%.1f", r.GPUHours),
						fmt.Sprintf("%.1f", r.BusyHours), fmt.Sprintf("%.0f%%", busy)})
				}
				output.Table(w, []string{byHeader(u.By), "GPU-HOURS", "BUSY", "UTIL"}, rows)
			})
		},
	}
	c.Flags().StringVar(&by, "by", "project", "project | run | submitter | node")
	c.Flags().StringVar(&since, "since", "7d", "window start: a duration like 24h, or an RFC3339 time")
	c.Flags().StringVar(&until, "until", "", "window end (default now)")
	return c
}

func byHeader(by string) string {
	switch by {
	case "run":
		return "RUN"
	case "submitter":
		return "SUBMITTER"
	case "node":
		return "NODE"
	}
	return "PROJECT"
}

// gpuCmd groups cluster-wide GPU questions that are not about one node.
func (a *app) gpuCmd() *cobra.Command {
	c := &cobra.Command{Use: "gpu", Short: "Cluster-wide GPU questions"}
	var since string
	var minHours float64
	idle := &cobra.Command{
		Use:   "idle",
		Short: "Cards a run held without using them",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			q := url.Values{"since": {since}, "min_hours": {strconv.FormatFloat(minHours, 'f', -1, 64)}}
			var g []v1.IdleGPU
			if err := a.client().Get(cmd.Context(), "/gpus/idle?"+q.Encode(), &g); err != nil {
				return err
			}
			return a.out().Emit(g, func(w ioWriter) {
				if len(g) == 0 {
					fmt.Fprintln(w, "nothing held idle in this window")
					return
				}
				rows := [][]string{}
				for _, x := range g {
					rows = append(rows, []string{x.NodeID, strconv.Itoa(x.GPUIndex), output.Short(x.RunID),
						output.Or(x.Project, "-"), fmt.Sprintf("%.1fh", x.HeldHours),
						fmt.Sprintf("%.1fh", x.BusyHours), fmt.Sprintf("%.0f%%", x.AvgUtil)})
				}
				output.Table(w, []string{"NODE", "GPU", "RUN", "PROJECT", "HELD", "BUSY", "AVG UTIL"}, rows)
			})
		},
	}
	idle.Flags().StringVar(&since, "since", "24h", "window start: a duration like 24h, or an RFC3339 time")
	idle.Flags().Float64Var(&minHours, "min-hours", 0.5, "only cards held at least this long")
	c.AddCommand(idle)
	return c
}
