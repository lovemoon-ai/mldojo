package commands

import (
	"fmt"
	"net/url"
	"strconv"

	"github.com/lovemoon-ai/mldojo/cli/internal/output"
	v1 "github.com/lovemoon-ai/mldojo/proto/mldojo/v1"
	"github.com/spf13/cobra"
)

type episodesResp struct {
	RunID    string            `json:"run_id"`
	Summary  v1.EpisodeSummary `json:"summary"`
	Episodes []v1.Episode      `json:"episodes"`
}

// runEpisodesCmd shows an evaluation trial by trial. For a policy this is
// the result; the success rate is a summary of it.
func (a *app) runEpisodesCmd() *cobra.Command {
	var result string
	c := &cobra.Command{
		Use: "episodes <run-id>", Short: "Per-episode results of an evaluation run",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			q := url.Values{}
			if result != "" {
				q.Set("result", result)
			}
			var resp episodesResp
			p := "/runs/" + url.PathEscape(args[0]) + "/episodes"
			if len(q) > 0 {
				p += "?" + q.Encode()
			}
			if err := a.client().Get(cmd.Context(), p, &resp); err != nil {
				return err
			}
			return a.out().Emit(resp, func(w ioWriter) {
				s := resp.Summary
				fmt.Fprintf(w, "%d/%d succeeded (%.0f%%), %d with video\n\n",
					s.Successes, s.Total, s.SuccessRate*100, s.WithVideo)
				if len(resp.Episodes) == 0 {
					fmt.Fprintln(w, "no episodes recorded (set outputs.episodes in the recipe)")
					return
				}
				rows := [][]string{}
				for _, e := range resp.Episodes {
					seed := "-"
					if e.Seed != nil {
						seed = strconv.FormatInt(*e.Seed, 10)
					}
					res := "fail"
					if e.Success {
						res = "ok"
					}
					dur := "-"
					if e.DurationMS > 0 {
						dur = fmt.Sprintf("%.1fs", float64(e.DurationMS)/1000)
					}
					rows = append(rows, []string{strconv.Itoa(e.Index), seed, res,
						output.Or(strconv.Itoa(e.Steps), "-"), dur, output.Or(e.VideoURI, "-")})
				}
				output.Table(w, []string{"EP", "SEED", "RESULT", "STEPS", "TIME", "VIDEO"}, rows)
			})
		},
	}
	c.Flags().StringVar(&result, "result", "", "only success | failure")
	return c
}

type episodeDiff struct {
	Seed  int64 `json:"seed"`
	Index int   `json:"index"`
	A     *bool `json:"a"`
	B     *bool `json:"b"`
}

type episodeDiffResp struct {
	A        string        `json:"a"`
	B        string        `json:"b"`
	Flipped  int           `json:"flipped"`
	Episodes []episodeDiff `json:"episodes"`
}

// compareEpisodesCmd answers the question a success rate cannot: between two
// evaluations of the same checkpoint, which trials changed their mind.
func (a *app) compareEpisodesCmd() *cobra.Command {
	var flippedOnly bool
	c := &cobra.Command{
		Use: "episodes <run-a> <run-b>", Short: "Compare two evaluations trial by trial, joined on seed",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			q := url.Values{"a": {args[0]}, "b": {args[1]}}
			var resp episodeDiffResp
			if err := a.client().Get(cmd.Context(), "/compare/episodes?"+q.Encode(), &resp); err != nil {
				return err
			}
			return a.out().Emit(resp, func(w ioWriter) {
				fmt.Fprintf(w, "%d of %d trials flipped\n\n", resp.Flipped, len(resp.Episodes))
				rows := [][]string{}
				for _, e := range resp.Episodes {
					flip := e.A != nil && e.B != nil && *e.A != *e.B
					if flippedOnly && !flip {
						continue
					}
					mark := ""
					if flip {
						mark = "flipped"
					}
					seed := "-"
					if e.Seed >= 0 {
						seed = strconv.FormatInt(e.Seed, 10)
					}
					rows = append(rows, []string{seed, strconv.Itoa(e.Index), boolStr(e.A), boolStr(e.B), mark})
				}
				output.Table(w, []string{"SEED", "EP", "A", "B", ""}, rows)
			})
		},
	}
	c.Flags().BoolVar(&flippedOnly, "flipped", false, "only the trials whose result changed")
	return c
}

func boolStr(b *bool) string {
	if b == nil {
		return "-"
	}
	if *b {
		return "ok"
	}
	return "fail"
}
