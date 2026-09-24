package recipes

import (
	"math"
	"math/rand/v2"
	"strings"
	"testing"
)

const goodSweep = `
apiVersion: mldojo/v1
kind: Sweep
metadata: {project: demo, name: lr-search}
recipe: recipes/train.yaml
target: pool:5090
method: random
metric: {name: val/loss, goal: min}
budget: {max_runs: 20, max_parallel: 4}
space:
  lr: {log_uniform: [1.0e-5, 1.0e-2]}
  bs: {values: [16, 32, 64]}
  seed: {int_uniform: [0, 100]}
`

func TestParseSweep(t *testing.T) {
	s, err := ParseSweep([]byte(goodSweep))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(s.Dimensions) != 3 {
		t.Fatalf("got %d dimensions, want 3", len(s.Dimensions))
	}
	// Sorted, so a grid enumerates in a stable order across restarts.
	if s.Dimensions[0].Name != "bs" || s.Dimensions[2].Name != "seed" {
		t.Errorf("dimensions are not sorted: %s, %s, %s",
			s.Dimensions[0].Name, s.Dimensions[1].Name, s.Dimensions[2].Name)
	}
}

func TestParseSweepRejectsBadDefinitions(t *testing.T) {
	cases := []struct{ name, yaml, want string }{
		{"unknown field", strings.Replace(goodSweep, "method: random", "methdo: random", 1), "field methdo"},
		{"no space", "apiVersion: mldojo/v1\nkind: Sweep\nmetadata: {project: p, name: n}\ntarget: pool:x\nmethod: random\nmetric: {name: loss, goal: min}\nbudget: {max_runs: 1, max_parallel: 1}\n", "at least one dimension"},
		{"two forms on one axis", strings.Replace(goodSweep, "bs: {values: [16, 32, 64]}", "bs: {values: [16], uniform: [1, 2]}", 1), "exactly one of"},
		{"backwards range", strings.Replace(goodSweep, "log_uniform: [1.0e-5, 1.0e-2]", "log_uniform: [1.0e-2, 1.0e-5]", 1), "min must be below max"},
		{"log range through zero", strings.Replace(goodSweep, "log_uniform: [1.0e-5, 1.0e-2]", "log_uniform: [0, 1.0e-2]", 1), "positive minimum"},
		{"no budget", strings.Replace(goodSweep, "max_runs: 20", "max_runs: 0", 1), "max_runs must be positive"},
		{"grid over a continuous axis", strings.Replace(goodSweep, "method: random", "method: grid", 1), "grid needs `values`"},
	}
	for _, c := range cases {
		_, err := ParseSweep([]byte(c.yaml))
		if err == nil {
			t.Errorf("%s: accepted an invalid sweep", c.name)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error %q does not mention %q", c.name, err, c.want)
		}
	}
}

func TestSampleStaysInRange(t *testing.T) {
	s, err := ParseSweep([]byte(goodSweep))
	if err != nil {
		t.Fatal(err)
	}
	rnd := rand.New(rand.NewPCG(1, 2))
	for range 200 {
		p := s.Sample(rnd)
		lr, ok := p["lr"].(float64)
		if !ok || lr < 1e-5 || lr > 1e-2 || math.IsNaN(lr) {
			t.Fatalf("lr %v is outside [1e-5, 1e-2]", p["lr"])
		}
		seed, ok := p["seed"].(int)
		if !ok || seed < 0 || seed > 100 {
			t.Fatalf("seed %v is outside [0, 100]", p["seed"])
		}
		if bs := p["bs"]; bs != 16 && bs != 32 && bs != 64 {
			t.Fatalf("bs %v is not one of the listed values", bs)
		}
	}
}

func TestSampleIsReproducibleFromASeed(t *testing.T) {
	s, _ := ParseSweep([]byte(goodSweep))
	a := s.Sample(rand.New(rand.NewPCG(7, 7)))
	b := s.Sample(rand.New(rand.NewPCG(7, 7)))
	if a["lr"] != b["lr"] || a["seed"] != b["seed"] || a["bs"] != b["bs"] {
		t.Errorf("same seed gave different samples: %v vs %v", a, b)
	}
}

func TestGridPointsRespectTheBudget(t *testing.T) {
	y := strings.Replace(goodSweep, "method: random", "method: grid", 1)
	y = strings.Replace(y, "lr: {log_uniform: [1.0e-5, 1.0e-2]}", "lr: {values: [0.1, 0.01]}", 1)
	y = strings.Replace(y, "seed: {int_uniform: [0, 100]}", "seed: {values: [1, 2]}", 1)
	s, err := ParseSweep([]byte(y))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	pts := s.GridPoints()
	if len(pts) != 12 { // 3 bs x 2 lr x 2 seed
		t.Fatalf("got %d grid points, want 12", len(pts))
	}
	// A product larger than the budget must be cut, not submitted whole.
	s.Budget.MaxRuns = 5
	if got := len(s.GridPoints()); got > 5 {
		t.Errorf("got %d points with a budget of 5", got)
	}
}
