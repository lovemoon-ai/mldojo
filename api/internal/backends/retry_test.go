package backends

import (
	"testing"

	v1 "github.com/lovemoon-ai/mldojo/proto/mldojo/v1"
)

// The lingbot run kept checkpoints at global_step_500 .. 2000; resuming has
// to pick the last one, not the first or the alphabetically largest.
func TestCheckpointStep(t *testing.T) {
	cases := map[string]int64{
		"/root/ws/outputs/exp/checkpoints/global_step_2000/hf_ckpt": 2000,
		"/root/ws/outputs/exp/checkpoints/global_step_500/hf_ckpt":  500,
		"outputs/model_step30.ckpt":                                 30,
		"outputs/epoch-7/model.pt":                                  7,
		"outputs/final.ckpt":                                        0,
	}
	for path, want := range cases {
		if got := checkpointStep(path); got != want {
			t.Errorf("checkpointStep(%q) = %d, want %d", path, got, want)
		}
	}
	// 2000 must beat 500 even though "500" sorts after "2000" as a string.
	if checkpointStep("a/global_step_2000/x") <= checkpointStep("a/global_step_500/x") {
		t.Error("step 2000 should outrank step 500")
	}
}

func TestGlobToRegexp(t *testing.T) {
	re := globToRegexp("outputs/*/checkpoints/**")
	for _, p := range []string{
		"/root/ws/comp/lingbot/outputs/run1/checkpoints/global_step_2000/hf_ckpt",
		"outputs/run1/checkpoints/x",
	} {
		if !re.MatchString(p) {
			t.Errorf("%q should match", p)
		}
	}
	// A single * must not cross a directory boundary.
	if globToRegexp("outputs/*.ckpt").MatchString("outputs/a/b.ckpt") {
		t.Error("a single * should not cross /")
	}
	if !globToRegexp("outputs/*.ckpt").MatchString("/abs/path/outputs/model.ckpt") {
		t.Error("the glob names the layout, not the whole absolute path")
	}
}

func TestAttemptName(t *testing.T) {
	if got := attemptName("seed=0", 2); got != "seed=0 (attempt 2)" {
		t.Errorf("got %q", got)
	}
	// Retrying a retry must not stack suffixes.
	if got := attemptName("seed=0 (attempt 2)", 3); got != "seed=0 (attempt 3)" {
		t.Errorf("got %q", got)
	}
}

func TestRetryPolicyDefaults(t *testing.T) {
	var p *v1.RetryPolicy
	if p.Retries() != 0 || p.RetryOnAny() {
		t.Error("no policy means no retries")
	}
	p = &v1.RetryPolicy{Max: 2}
	if p.Retries() != 2 {
		t.Errorf("Retries = %d", p.Retries())
	}
	// The default deliberately excludes a job's own failure: retrying it
	// just runs the same bug again.
	if p.RetryOnAny() {
		t.Error("retry.on should default to infra")
	}
	if !(&v1.RetryPolicy{Max: 1, On: "any"}).RetryOnAny() {
		t.Error("retry.on any should opt in")
	}
}
