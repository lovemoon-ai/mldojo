// Package recipes parses, validates and resolves MLDojo recipes.
package recipes

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	v1 "github.com/lovemoon-ai/mldojo/proto/mldojo/v1"
	"gopkg.in/yaml.v3"
)

const (
	APIVersion = "mldojo/v1"
	KindExp    = "Experiment"
)

type Recipe struct {
	APIVersion string          `yaml:"apiVersion" json:"apiVersion"`
	Kind       string          `yaml:"kind" json:"kind"`
	Metadata   Metadata        `yaml:"metadata" json:"metadata"`
	Code       Code            `yaml:"code" json:"code"`
	Env        EnvBlock        `yaml:"env" json:"env"`
	Datasets   []DatasetRef    `yaml:"datasets" json:"datasets"`
	Models     []ModelRef      `yaml:"models" json:"models"`
	Run        RunBlock        `yaml:"run" json:"run"`
	Resources  ResourcesBlock  `yaml:"resources" json:"resources"`
	Retry      *v1.RetryPolicy `yaml:"retry" json:"retry"`
	Outputs    OutputsBlock    `yaml:"outputs" json:"outputs"`
	Hooks      Hooks           `yaml:"hooks" json:"hooks"`
}

type Metadata struct {
	Project     string   `yaml:"project" json:"project"`
	Name        string   `yaml:"name" json:"name"`
	Description string   `yaml:"description" json:"description"`
	Tags        []string `yaml:"tags" json:"tags"`
}

type Code struct {
	Source string `yaml:"source" json:"source"` // git | local | inline-patch | none
	Repo   string `yaml:"repo" json:"repo"`
	Ref    string `yaml:"ref" json:"ref"`
	Path   string `yaml:"path" json:"path"`   // local dir to bundle (default ".")
	Patch  string `yaml:"patch" json:"patch"` // inline-patch: patch file path
}

type EnvBlock struct {
	Default   map[string]any `yaml:"default" json:"default"`
	Overrides []Override     `yaml:"overrides" json:"overrides"`
}

type ResourcesBlock struct {
	Default   map[string]any `yaml:"default" json:"default"`
	Overrides []Override     `yaml:"overrides" json:"overrides"`
}

type Override struct {
	When Match          `yaml:"when" json:"when"`
	Use  map[string]any `yaml:"use" json:"use"`
}

// Match selects a target. Every non-empty field must match.
type Match struct {
	BackendKind string   `yaml:"backend_kind" json:"backend_kind,omitempty"` // node | queue
	Backend     string   `yaml:"backend" json:"backend,omitempty"`           // queue plugin name for queues, node id for nodes
	BackendID   string   `yaml:"backend_id" json:"backend_id,omitempty"`     // full id
	Target      string   `yaml:"target" json:"target,omitempty"`             // full target string
	Labels      []string `yaml:"labels" json:"labels,omitempty"`             // all required
}

type DatasetRef struct {
	Name    string `yaml:"name" json:"name"`
	Version string `yaml:"version" json:"version"`
	Mount   string `yaml:"mount" json:"mount"`
}

// ModelRef names the model a run consumes instead of pasting its path. The
// resolved location fills the parameter named by As, so the command keeps
// using ${ckpt} -- and the run records which version it read.
type ModelRef struct {
	Name    string `yaml:"name" json:"name"`       // "name" or "project/name"
	Version string `yaml:"version" json:"version"` // a number, or latest (default) | production | staging
	As      string `yaml:"as" json:"as"`           // parameter to fill (default "model")
}

// Project and Model split "project/name"; an unqualified name belongs to the
// recipe's own project.
func (m ModelRef) Project(def string) (string, string) {
	if p, n, ok := strings.Cut(m.Name, "/"); ok {
		return p, n
	}
	return def, m.Name
}

// Param is the parameter this reference fills.
func (m ModelRef) Param() string {
	if m.As != "" {
		return m.As
	}
	return "model"
}

type RunBlock struct {
	Cmd     string            `yaml:"cmd" json:"cmd"`
	Workdir string            `yaml:"workdir" json:"workdir"`
	Setup   string            `yaml:"setup" json:"setup"` // shell run before cmd, inside the env
	Params  map[string]any    `yaml:"params" json:"params"`
	Env     map[string]string `yaml:"env" json:"env"`
	Wandb   string            `yaml:"wandb" json:"wandb"` // "shim" enables the wandb compatibility layer
}

type OutputsBlock struct {
	Logs        string             `yaml:"logs" json:"logs"`
	Checkpoints string             `yaml:"checkpoints" json:"checkpoints"`
	Videos      string             `yaml:"videos" json:"videos"`
	Images      string             `yaml:"images" json:"images"`
	Metrics     []v1.MetricsSource `yaml:"metrics" json:"metrics"`
	// Episodes turns a run into an evaluation: its result becomes a rate
	// over trials rather than a curve over steps.
	Episodes []v1.EpisodesSource `yaml:"episodes" json:"episodes"`
	// Model registers what the run produced. On success the newest
	// checkpoint becomes a version of this model, so the evaluation that
	// follows can name it rather than copy its path.
	Model string `yaml:"model" json:"model"`
}

// Hooks are the Python escape hatch.
type Hooks struct {
	// PreSubmit is a Python script run by the CLI at submit time. It receives
	// the recipe as JSON on stdin and must print the (modified) recipe JSON.
	PreSubmit string `yaml:"pre_submit" json:"pre_submit"`
}

// Parse decodes YAML (strictly: unknown fields are errors) and validates it.
func Parse(data []byte) (*Recipe, error) {
	var r Recipe
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&r); err != nil {
		return nil, fmt.Errorf("recipe: %w", err)
	}
	r.applyDefaults()
	if err := r.Validate(); err != nil {
		return nil, err
	}
	return &r, nil
}

// ParseJSON decodes a JSON recipe (used by hooks and the AI endpoint).
func ParseJSON(data []byte) (*Recipe, error) {
	var r Recipe
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return nil, fmt.Errorf("recipe: %w", err)
	}
	r.applyDefaults()
	if err := r.Validate(); err != nil {
		return nil, err
	}
	return &r, nil
}

// YAML renders the recipe back to YAML.
func (r *Recipe) YAML() string {
	b, _ := yaml.Marshal(r)
	return string(b)
}

func (r *Recipe) applyDefaults() {
	if r.APIVersion == "" {
		r.APIVersion = APIVersion
	}
	if r.Kind == "" {
		r.Kind = KindExp
	}
	if r.Code.Source == "" {
		r.Code.Source = "local"
		if r.Code.Repo != "" {
			r.Code.Source = "git"
		}
		// An absolute workdir says the project already lives on the node,
		// so there is nothing to ship.
		if filepath.IsAbs(r.Run.Workdir) {
			r.Code.Source = "none"
		}
	}
	if r.Run.Workdir == "" {
		r.Run.Workdir = "."
	}
}

// ValidationError aggregates every problem found in a recipe.
type ValidationError struct{ Problems []string }

func (e *ValidationError) Error() string {
	return "invalid recipe:\n  - " + strings.Join(e.Problems, "\n  - ")
}

var nameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)

// ValidName reports whether s is a valid project/experiment/node name.
func ValidName(s string) bool { return nameRe.MatchString(s) }

func (r *Recipe) Validate() error {
	var p []string
	add := func(f string, a ...any) { p = append(p, fmt.Sprintf(f, a...)) }
	if r.APIVersion != APIVersion {
		add("apiVersion must be %q (got %q)", APIVersion, r.APIVersion)
	}
	if r.Kind != KindExp {
		add("kind must be %q (got %q)", KindExp, r.Kind)
	}
	if r.Metadata.Project != "" && !ValidName(r.Metadata.Project) {
		add("metadata.project %q: use letters, digits, '.', '_' or '-'", r.Metadata.Project)
	}
	if r.Metadata.Name != "" && !ValidName(r.Metadata.Name) {
		add("metadata.name %q: use letters, digits, '.', '_' or '-'", r.Metadata.Name)
	}
	switch r.Code.Source {
	case "git", "local":
		if filepath.IsAbs(r.Run.Workdir) {
			add("run.workdir %s is absolute, which only makes sense with code.source none: "+
				"code.source %s ships the code into the run directory, and an absolute workdir would ignore it",
				r.Run.Workdir, r.Code.Source)
		}
	case "none":
		// Nothing is shipped: the command runs against whatever is already
		// on the node. An absolute workdir points at it; a relative one
		// still gets an empty directory of its own.
	case "inline-patch":
		if r.Code.Repo == "" {
			add("code.repo is required for source inline-patch")
		}
	default:
		add("code.source must be git|local|inline-patch|none (got %q)", r.Code.Source)
	}
	if strings.TrimSpace(r.Run.Cmd) == "" {
		add("run.cmd is required")
	}
	if r.Run.Wandb != "" && r.Run.Wandb != "shim" && r.Run.Wandb != "off" {
		add("run.wandb must be shim|off")
	}
	if r.Retry != nil {
		if r.Retry.Max < 0 || r.Retry.Max > 10 {
			add("retry.max must be between 0 and 10 (got %d)", r.Retry.Max)
		}
		if r.Retry.On != "" && r.Retry.On != "infra" && r.Retry.On != "any" {
			add("retry.on must be infra|any (got %q)", r.Retry.On)
		}
	}
	checkEnv := func(where string, m map[string]any) {
		if m == nil {
			return
		}
		if t, ok := m["type"]; ok {
			switch fmt.Sprint(t) {
			case "docker", "conda", "venv", "none":
			default:
				add("%s.type must be docker|conda|venv|none (got %v)", where, t)
			}
			if fmt.Sprint(t) == "docker" {
				if img, _ := m["image"].(string); img == "" && where == "env.default" {
					add("%s.image is required for docker", where)
				}
			}
		}
		for k := range m {
			switch k {
			case "type", "spec", "image", "vars":
			default:
				add("%s: unknown field %q", where, k)
			}
		}
	}
	checkEnv("env.default", r.Env.Default)
	for i, o := range r.Env.Overrides {
		checkEnv(fmt.Sprintf("env.overrides[%d].use", i), o.Use)
		checkMatch(fmt.Sprintf("env.overrides[%d].when", i), o.When, add)
	}
	checkRes := func(where string, m map[string]any) {
		for k, v := range m {
			switch k {
			case "gpu_type":
				if _, ok := v.(string); !ok {
					add("%s.gpu_type must be a string", where)
				}
			case "gpus", "min_mem_gb", "workers", "gpu_per_worker", "cpu_per_worker", "cpu_mem_ratio", "wall_time_min":
				n, ok := toInt(v)
				if !ok || n < 0 {
					add("%s.%s must be a non-negative integer", where, k)
				}
			default:
				add("%s: unknown field %q", where, k)
			}
		}
	}
	checkRes("resources.default", r.Resources.Default)
	for i, o := range r.Resources.Overrides {
		checkRes(fmt.Sprintf("resources.overrides[%d].use", i), o.Use)
		checkMatch(fmt.Sprintf("resources.overrides[%d].when", i), o.When, add)
	}
	seen := map[string]bool{}
	for i, d := range r.Datasets {
		if d.Name == "" {
			add("datasets[%d].name is required", i)
		}
		if d.Mount != "" && !strings.HasPrefix(d.Mount, "/") {
			add("datasets[%d].mount must be an absolute path", i)
		}
		if seen[d.Name] {
			add("datasets[%d]: duplicate dataset %q", i, d.Name)
		}
		seen[d.Name] = true
	}
	for i, m := range r.Models {
		p, n := m.Project(r.Metadata.Project)
		if m.Name == "" || !ValidName(n) || (p != "" && !ValidName(p)) {
			add("models[%d].name must be \"name\" or \"project/name\" (got %q)", i, m.Name)
		}
		switch v := m.Version; v {
		case "", "latest", "production", "staging":
		default:
			if _, err := strconv.Atoi(v); err != nil {
				add("models[%d].version must be a number, latest, production or staging (got %q)", i, v)
			}
		}
		if _, taken := r.Run.Params[m.Param()]; taken {
			add("models[%d] fills ${%s}, which run.params already sets: drop one", i, m.Param())
		}
	}
	if r.Outputs.Model != "" {
		p, n := ModelRef{Name: r.Outputs.Model}.Project(r.Metadata.Project)
		if !ValidName(n) || (p != "" && !ValidName(p)) {
			add("outputs.model must be \"name\" or \"project/name\" (got %q)", r.Outputs.Model)
		}
		if r.Outputs.Checkpoints == "" {
			add("outputs.model needs outputs.checkpoints: the version is registered from the checkpoints collected")
		}
	}
	for i, m := range r.Outputs.Metrics {
		if m.Type != "tensorboard" && m.Type != "jsonl" {
			add("outputs.metrics[%d].type must be tensorboard|jsonl", i)
		}
		if m.Path == "" {
			add("outputs.metrics[%d].path is required", i)
		}
	}
	for k := range r.Run.Params {
		if !paramRe.MatchString(k) {
			add("run.params: invalid parameter name %q", k)
		}
	}
	if len(p) > 0 {
		sort.Strings(p)
		return &ValidationError{Problems: p}
	}
	return nil
}

func checkMatch(where string, m Match, add func(string, ...any)) {
	if m.BackendKind != "" && m.BackendKind != "node" && m.BackendKind != "queue" {
		add("%s.backend_kind must be node|queue", where)
	}
}

var paramRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]*$`)

func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		if n == float64(int(n)) {
			return int(n), true
		}
	case uint64:
		return int(n), true
	}
	return 0, false
}
