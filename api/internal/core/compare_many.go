package core

import (
	"context"
	"sort"
	"strings"

	"github.com/lovemoon-ai/mldojo/api/internal/models"
	v1 "github.com/lovemoon-ai/mldojo/proto/mldojo/v1"
)

// MaxCompareRuns bounds a comparison: past this the chart is unreadable and
// the query gets expensive.
const MaxCompareRuns = 20

// CompareRun is one row of a comparison: enough to draw a leaderboard
// (params against final metrics) without a second round trip.
type CompareRun struct {
	ID         string               `json:"id"`
	Name       string               `json:"name"`
	Project    string               `json:"project"`
	Experiment string               `json:"experiment"`
	Status     string               `json:"status"`
	Target     string               `json:"target"`
	Params     map[string]any       `json:"params,omitempty"`
	Latest     map[string]v1.Float  `json:"latest,omitempty"`
	Series     map[string][]v1.Step `json:"series,omitempty"`
}

// CompareManyResult is what /compare/many returns.
type CompareManyResult struct {
	Runs       []CompareRun `json:"runs"`
	MetricKeys []string     `json:"metric_keys"`
	ParamKeys  []string     `json:"param_keys"`
	Sampled    bool         `json:"sampled"`
}

// CompareMany gathers several runs with their params and metric curves, so
// the web app can overlay curves and rank by hyperparameter in one request.
// The A/B Compare endpoint only ever answered about two runs and returned no
// series at all, which is why the UI had no charts.
func (s *Service) CompareMany(ctx context.Context, ids []string, keys []string, maxPoints int) (*CompareManyResult, error) {
	if len(ids) == 0 {
		return nil, models.UserError("give at least one run id")
	}
	if len(ids) > MaxCompareRuns {
		return nil, models.UserError("comparing %d runs at once is too many (max %d)", len(ids), MaxCompareRuns)
	}
	out := &CompareManyResult{Runs: make([]CompareRun, 0, len(ids))}
	metricSet, paramSet := map[string]bool{}, map[string]bool{}
	wanted := strings.Join(keys, ",")

	for _, id := range ids {
		run, err := s.store().GetRun(ctx, id)
		if err != nil {
			return nil, err
		}
		cr := CompareRun{
			ID: run.ID, Name: run.Name, Project: run.Project, Experiment: run.Experiment,
			Status: run.Status, Target: run.Target, Params: run.Metadata.Params,
			Latest: map[string]v1.Float{}, Series: map[string][]v1.Step{},
		}
		for k := range run.Metadata.Params {
			paramSet[k] = true
		}
		latest, err := s.store().LatestMetrics(ctx, run.ID)
		if err != nil {
			return nil, err
		}
		for k, p := range latest {
			cr.Latest[k] = v1.Float(p.Value)
			metricSet[k] = true
		}
		pts, sampled, err := s.store().ListMetrics(ctx, run.ID, wanted, 0, maxPoints)
		if err != nil {
			return nil, err
		}
		out.Sampled = out.Sampled || sampled
		for _, p := range pts {
			cr.Series[p.Key] = append(cr.Series[p.Key], v1.Step{Step: p.Step, Value: v1.Float(p.Value)})
			metricSet[p.Key] = true
		}
		out.Runs = append(out.Runs, cr)
	}
	out.MetricKeys = sortedKeys(metricSet)
	out.ParamKeys = sortedKeys(paramSet)
	return out, nil
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
