package recipes

import (
	"fmt"
	"math"
	"math/rand/v2"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Sweep is a hyperparameter search over an existing recipe.
//
// v1 could only expand a literal matrix (`--matrix seed`), a full cartesian
// product submitted all at once with no budget and no notion of which result
// was better. A sweep adds a search space, a sampling method and a budget.
type Sweep struct {
	APIVersion string      `yaml:"apiVersion" json:"apiVersion"`
	Kind       string      `yaml:"kind" json:"kind"`
	Metadata   Metadata    `yaml:"metadata" json:"metadata"`
	Recipe     string      `yaml:"recipe" json:"recipe"` // path to the recipe, resolved by the CLI
	RecipeYAML string      `yaml:"recipe_yaml" json:"recipe_yaml"`
	Target     string      `yaml:"target" json:"target"`
	Method     string      `yaml:"method" json:"method"` // grid | random
	Metric     SweepMetric `yaml:"metric" json:"metric"`
	Budget     SweepBudget `yaml:"budget" json:"budget"`
	Space      yaml.Node   `yaml:"space" json:"-"`
	Dimensions []SweepDim  `yaml:"-" json:"space"`
}

type SweepMetric struct {
	Name string `yaml:"name" json:"name"`
	Goal string `yaml:"goal" json:"goal"` // min | max
}

type SweepBudget struct {
	MaxRuns     int `yaml:"max_runs" json:"max_runs"`
	MaxParallel int `yaml:"max_parallel" json:"max_parallel"`
}

// SweepDim is one axis of the search space. Exactly one form must be set.
type SweepDim struct {
	Name       string    `json:"name"`
	Values     []any     `yaml:"values" json:"values,omitempty"`
	Uniform    []float64 `yaml:"uniform" json:"uniform,omitempty"`
	LogUniform []float64 `yaml:"log_uniform" json:"log_uniform,omitempty"`
	IntUniform []int     `yaml:"int_uniform" json:"int_uniform,omitempty"`
}

const (
	MethodGrid   = "grid"
	MethodRandom = "random"
)

// ParseSweep reads a sweep definition, rejecting unknown fields like recipes
// do -- a typo in a search space is a silent waste of GPU hours otherwise.
func ParseSweep(b []byte) (*Sweep, error) {
	var s Sweep
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("sweep: %w", err)
	}
	if err := s.decodeSpace(); err != nil {
		return nil, err
	}
	if s.Method == "" {
		s.Method = MethodRandom
	}
	if s.Metric.Goal == "" {
		s.Metric.Goal = "min"
	}
	return &s, s.Validate()
}

func (s *Sweep) decodeSpace() error {
	if s.Space.Kind == 0 {
		return nil
	}
	var raw map[string]SweepDim
	if err := s.Space.Decode(&raw); err != nil {
		return fmt.Errorf("sweep: space: %w", err)
	}
	names := make([]string, 0, len(raw))
	for k := range raw {
		names = append(names, k)
	}
	sort.Strings(names) // deterministic grid order
	for _, n := range names {
		d := raw[n]
		d.Name = n
		s.Dimensions = append(s.Dimensions, d)
	}
	return nil
}

func (s *Sweep) Validate() error {
	var problems []string
	if s.APIVersion != "mldojo/v1" {
		problems = append(problems, fmt.Sprintf("apiVersion must be mldojo/v1, got %q", s.APIVersion))
	}
	if s.Kind != "Sweep" {
		problems = append(problems, fmt.Sprintf("kind must be Sweep, got %q", s.Kind))
	}
	if s.Metadata.Project == "" || s.Metadata.Name == "" {
		problems = append(problems, "metadata.project and metadata.name are required")
	}
	if s.Target == "" {
		problems = append(problems, "target is required")
	}
	if s.Method != MethodGrid && s.Method != MethodRandom {
		problems = append(problems, fmt.Sprintf("method must be %q or %q, got %q", MethodGrid, MethodRandom, s.Method))
	}
	if s.Metric.Name == "" {
		problems = append(problems, "metric.name is required, so runs can be ranked")
	}
	if s.Metric.Goal != "min" && s.Metric.Goal != "max" {
		problems = append(problems, fmt.Sprintf("metric.goal must be min or max, got %q", s.Metric.Goal))
	}
	if len(s.Dimensions) == 0 {
		problems = append(problems, "space must have at least one dimension")
	}
	if s.Budget.MaxRuns <= 0 {
		problems = append(problems, "budget.max_runs must be positive, or the sweep never ends")
	}
	if s.Budget.MaxParallel <= 0 {
		problems = append(problems, "budget.max_parallel must be positive")
	}
	for _, d := range s.Dimensions {
		if err := d.validate(); err != nil {
			problems = append(problems, err.Error())
		}
	}
	if s.Method == MethodGrid {
		for _, d := range s.Dimensions {
			if len(d.Values) == 0 {
				problems = append(problems, fmt.Sprintf("space.%s: grid needs `values`, not a continuous range", d.Name))
			}
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return fmt.Errorf("sweep is invalid:\n  - %s", strings.Join(problems, "\n  - "))
	}
	return nil
}

func (d SweepDim) validate() error {
	forms := 0
	for _, set := range []bool{len(d.Values) > 0, len(d.Uniform) > 0, len(d.LogUniform) > 0, len(d.IntUniform) > 0} {
		if set {
			forms++
		}
	}
	if forms == 0 {
		return fmt.Errorf("space.%s: needs one of values, uniform, log_uniform, int_uniform", d.Name)
	}
	if forms > 1 {
		return fmt.Errorf("space.%s: set exactly one of values, uniform, log_uniform, int_uniform", d.Name)
	}
	for _, r := range [][]float64{d.Uniform, d.LogUniform} {
		if len(r) != 0 && len(r) != 2 {
			return fmt.Errorf("space.%s: a range is [min, max]", d.Name)
		}
		if len(r) == 2 && r[0] >= r[1] {
			return fmt.Errorf("space.%s: min must be below max", d.Name)
		}
	}
	if len(d.LogUniform) == 2 && d.LogUniform[0] <= 0 {
		return fmt.Errorf("space.%s: log_uniform needs a positive minimum", d.Name)
	}
	if len(d.IntUniform) != 0 {
		if len(d.IntUniform) != 2 {
			return fmt.Errorf("space.%s: int_uniform is [min, max]", d.Name)
		}
		if d.IntUniform[0] >= d.IntUniform[1] {
			return fmt.Errorf("space.%s: min must be below max", d.Name)
		}
	}
	return nil
}

// Sample draws one parameter set. rnd is passed in so a sweep can be replayed
// from a seed.
func (s *Sweep) Sample(rnd *rand.Rand) map[string]any {
	out := map[string]any{}
	for _, d := range s.Dimensions {
		switch {
		case len(d.Values) > 0:
			out[d.Name] = d.Values[rnd.IntN(len(d.Values))]
		case len(d.Uniform) == 2:
			out[d.Name] = d.Uniform[0] + rnd.Float64()*(d.Uniform[1]-d.Uniform[0])
		case len(d.LogUniform) == 2:
			lo, hi := math.Log(d.LogUniform[0]), math.Log(d.LogUniform[1])
			out[d.Name] = math.Exp(lo + rnd.Float64()*(hi-lo))
		case len(d.IntUniform) == 2:
			out[d.Name] = d.IntUniform[0] + rnd.IntN(d.IntUniform[1]-d.IntUniform[0]+1)
		}
	}
	return out
}

// GridPoints enumerates the full product for a grid sweep, capped at the
// budget. Order is deterministic so resuming a sweep continues where it left
// off rather than repeating work.
func (s *Sweep) GridPoints() []map[string]any {
	out := []map[string]any{{}}
	for _, d := range s.Dimensions {
		var next []map[string]any
		for _, base := range out {
			for _, v := range d.Values {
				m := make(map[string]any, len(base)+1)
				for k, bv := range base {
					m[k] = bv
				}
				m[d.Name] = v
				next = append(next, m)
			}
		}
		out = next
		if len(out) > s.Budget.MaxRuns {
			break
		}
	}
	if len(out) > s.Budget.MaxRuns {
		out = out[:s.Budget.MaxRuns]
	}
	return out
}
