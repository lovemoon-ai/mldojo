package ai

import (
	"testing"

	"os"
	"strings"

	v1 "github.com/lovemoon-ai/mldojo/proto/mldojo/v1"
)

// The real run logs 335 keys, seven of them losses. Taking the first match
// in key order picked training/router_z_loss -- a MoE router diagnostic --
// as the number describing the job.
func TestPrimaryMetricPicksThePlainestLoss(t *testing.T) {
	keys := []string{
		"align/future_video_mse_loss", "moe_zloss/avg_raw", "training/router_z_loss",
		"training/depth_loss", "training/vla_loss", "training/loss", "training/sequence_wise_loss",
		"steptime", "training/lr",
	}
	got, lower := PrimaryMetric(keys)
	if got != "training/loss" || !lower {
		t.Errorf("PrimaryMetric = (%q, %v), want (training/loss, true)", got, lower)
	}
	// Without the plain one, the shortest qualified loss wins over a router
	// diagnostic.
	got, _ = PrimaryMetric([]string{"training/router_z_loss", "align/future_video_mse_loss", "training/vla_loss"})
	if got != "training/vla_loss" {
		t.Errorf("PrimaryMetric = %q, want training/vla_loss", got)
	}
}

// The same question asked of the file itself rather than a list somebody
// typed out. testdata/real is the repo's real-artifact corpus; the rule is
// that anything reading a job's output is tested against it.
func TestPrimaryMetricOverTheRealCorpus(t *testing.T) {
	raw, err := os.ReadFile("../../../testdata/real/tfevents-keys.txt")
	if err != nil {
		t.Fatal(err)
	}
	keys := strings.Fields(string(raw))
	if len(keys) < 40 {
		t.Fatalf("corpus gave %d keys, expected the real run's 46", len(keys))
	}
	got, lower := PrimaryMetric(keys)
	if got != "training/loss" || !lower {
		t.Errorf("PrimaryMetric = (%q, %v), want (training/loss, true)", got, lower)
	}
	// An evaluation outranks any loss: the loss sat at 0.06-0.08 from the
	// first checkpoint to the last while the success rate went 40% to 75%.
	got, lower = PrimaryMetric(append(keys, v1.MetricSuccessRate))
	if got != v1.MetricSuccessRate || lower {
		t.Errorf("PrimaryMetric with an evaluation = (%q, %v), want (%s, false)", got, lower, v1.MetricSuccessRate)
	}
}

// The real lingbot evaluation had eight runs: four that finished and four
// cancelled. Cancelled runs have no value and sort last, and reading the
// last run's value as "the worst" panicked GET /ai/experiments/.../summary
// -- the endpoint the experiment matrix reads.
func TestSpreadIgnoresRunsWithNoResult(t *testing.T) {
	v := func(f float64) *v1.Float { x := v1.Float(f); return &x }
	ranking := []RunScore{
		{Name: "horizon=50,setting=clean", Value: v(0.80)},
		{Name: "horizon=16,setting=clean", Value: v(0.70)},
		{Name: "horizon=50,setting=randomized", Value: v(0.25)},
		{Name: "horizon=16,setting=randomized", Value: v(0)},
		{Name: "cancelled-1", Status: "cancelled"},
		{Name: "cancelled-2", Status: "cancelled"},
	}
	w := worstScored(ranking)
	if w == nil || *w != 0 {
		t.Fatalf("worstScored = %v, want the 0%% run, not the cancelled one", w)
	}
	// Every run cancelled: there is no number to spread over, and the caller
	// only reaches this with a best run, so nil must still not be produced
	// out of thin air.
	if got := worstScored(ranking[4:]); got != nil {
		t.Errorf("worstScored with no results = %v, want nil", got)
	}
}
