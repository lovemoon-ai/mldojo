package recipes

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	v1 "github.com/lovemoon-ai/mldojo/proto/mldojo/v1"
)

// Target is a parsed `<backend_kind>:<backend_id>/<location?>` string.
type Target struct {
	Raw     string   // normalized "node:gpu-a" / "queue:myqueue/gpu-a100"
	Kind    string   // node | pool | queue | external
	ID      string   // node id, or full queue id ("myqueue/gpu-a100")
	Backend string   // node id for nodes, queue plugin name for queues
	Queue   string   // queue name for queues ("project-x")
	Labels  []string // pool targets: every label a node must carry
}

func ParseTarget(s string) (Target, error) {
	s = strings.TrimSpace(s)
	kind, rest, ok := strings.Cut(s, ":")
	if !ok || rest == "" {
		return Target{}, fmt.Errorf("invalid target %q: expected node:<id>, pool:<label>, queue:<backend>/<queue> or external:<host>", s)
	}
	t := Target{Raw: kind + ":" + rest, Kind: kind, ID: rest}
	switch kind {
	case "node":
		if strings.Contains(rest, "/") || !ValidName(rest) {
			return Target{}, fmt.Errorf("invalid node target %q", s)
		}
		t.Backend = rest
	case "pool":
		// pool:<label>[,<label>...] -- any online node carrying all of these
		// labels. Lets a recipe say "a 5090" instead of naming a machine.
		for _, l := range strings.Split(rest, ",") {
			if l = strings.TrimSpace(l); l == "" || !ValidName(l) {
				return Target{}, fmt.Errorf("invalid pool target %q: expected pool:<label>[,<label>]", s)
			}
			t.Labels = append(t.Labels, l)
		}
		t.Backend = rest
	case "external":
		// A process that runs itself and only reports in (mldojo.init()).
		// The id names where it ran, for display only.
		if strings.Contains(rest, "/") || !ValidName(rest) {
			return Target{}, fmt.Errorf("invalid external target %q", s)
		}
		t.Backend = rest
	case "queue":
		b, q, ok := strings.Cut(rest, "/")
		if !ok || b == "" || q == "" {
			return Target{}, fmt.Errorf("invalid queue target %q: expected queue:<backend>/<queue>", s)
		}
		t.Backend, t.Queue = b, q
	default:
		return Target{}, fmt.Errorf("invalid target %q: kind must be node or queue", s)
	}
	return t, nil
}

func (m Match) matches(t Target, labels []string) bool {
	if m.BackendKind != "" && m.BackendKind != t.Kind {
		return false
	}
	if m.Backend != "" && m.Backend != t.Backend {
		return false
	}
	if m.BackendID != "" && m.BackendID != t.ID {
		return false
	}
	if m.Target != "" && m.Target != t.Raw {
		return false
	}
	for _, want := range m.Labels {
		found := false
		for _, l := range labels {
			if l == want {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func merged(def map[string]any, ovs []Override, t Target, labels []string) map[string]any {
	out := map[string]any{}
	for k, v := range def {
		out[k] = v
	}
	for _, o := range ovs {
		if o.When.matches(t, labels) {
			// A type switch replaces the whole env (e.g. conda -> docker).
			if nt, ok := o.Use["type"]; ok && fmt.Sprint(nt) != fmt.Sprint(out["type"]) {
				out = map[string]any{}
			}
			for k, v := range o.Use {
				out[k] = v
			}
		}
	}
	return out
}

func remarshal(in map[string]any, out any) error {
	b, err := json.Marshal(normalize(in))
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

// normalize converts yaml-decoded maps (map[string]any with nested
// map[any]any in old decoders) into JSON-safe values.
func normalize(v any) any {
	switch x := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(x))
		for k, vv := range x {
			m[k] = normalize(vv)
		}
		return m
	case map[any]any:
		m := make(map[string]any, len(x))
		for k, vv := range x {
			m[fmt.Sprint(k)] = normalize(vv)
		}
		return m
	case []any:
		out := make([]any, len(x))
		for i, vv := range x {
			out[i] = normalize(vv)
		}
		return out
	}
	return v
}

// ResolveEnv returns the env spec for a target (defaults + matching overrides).
func (r *Recipe) ResolveEnv(t Target, labels []string) (v1.EnvSpec, error) {
	var e v1.EnvSpec
	if err := remarshal(merged(r.Env.Default, r.Env.Overrides, t, labels), &e); err != nil {
		return e, fmt.Errorf("env: %w", err)
	}
	if e.Type == "" {
		e.Type = "none"
	}
	if e.Type == "docker" && e.Image == "" {
		return e, fmt.Errorf("env: docker requires an image for target %s", t.Raw)
	}
	return e, nil
}

// ResolveResources returns the resources for a target (defaults + overrides).
func (r *Recipe) ResolveResources(t Target, labels []string) (v1.Resources, error) {
	var res v1.Resources
	if err := remarshal(merged(r.Resources.Default, r.Resources.Overrides, t, labels), &res); err != nil {
		return res, fmt.Errorf("resources: %w", err)
	}
	return res, nil
}

// ParamSet is one expanded point of the parameter matrix.
type ParamSet struct {
	Name   string         // "seed=0,lr=0.1" (only the matrix axes)
	Values map[string]any // every parameter, scalar values
}

// Expand builds the parameter sets. Parameters named in matrix are expanded
// as a cartesian product; any other list-valued parameter takes its first
// element. overrides replace recipe values before expansion.
func (r *Recipe) Expand(matrix []string, overrides map[string]any) ([]ParamSet, error) {
	params := map[string]any{}
	for k, v := range r.Run.Params {
		params[k] = normalize(v)
	}
	for k, v := range overrides {
		params[k] = v
	}
	axes := []string{}
	seen := map[string]bool{}
	for _, m := range matrix {
		for _, name := range strings.Split(m, ",") {
			name = strings.TrimSpace(name)
			if name == "" || seen[name] {
				continue
			}
			if name == "all" {
				for k, v := range params {
					if _, isList := v.([]any); isList && !seen[k] {
						axes = append(axes, k)
						seen[k] = true
					}
				}
				continue
			}
			if _, ok := params[name]; !ok {
				return nil, fmt.Errorf("--matrix %s: no such parameter in run.params", name)
			}
			axes = append(axes, name)
			seen[name] = true
		}
	}
	sort.Strings(axes)
	base := map[string]any{}
	for k, v := range params {
		if seen[k] {
			continue
		}
		if l, ok := v.([]any); ok {
			if len(l) == 0 {
				return nil, fmt.Errorf("parameter %s is an empty list", k)
			}
			base[k] = l[0]
		} else {
			base[k] = v
		}
	}
	sets := []ParamSet{{Values: base}}
	for _, ax := range axes {
		vals, ok := params[ax].([]any)
		if !ok {
			vals = []any{params[ax]}
		}
		if len(vals) == 0 {
			return nil, fmt.Errorf("parameter %s is an empty list", ax)
		}
		var next []ParamSet
		for _, s := range sets {
			for _, v := range vals {
				nv := make(map[string]any, len(s.Values)+1)
				for k, x := range s.Values {
					nv[k] = x
				}
				nv[ax] = v
				name := fmt.Sprintf("%s=%v", ax, v)
				if s.Name != "" {
					name = s.Name + "," + name
				}
				next = append(next, ParamSet{Name: name, Values: nv})
			}
		}
		sets = next
	}
	if len(sets) > 1000 {
		return nil, fmt.Errorf("parameter matrix too large (%d runs, max 1000)", len(sets))
	}
	return sets, nil
}

var varRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_.-]*)\}`)

// Substitute replaces ${name} with params/builtins. Unknown variables (e.g.
// ${HOME}) are left untouched for the shell.
func Substitute(s string, vars map[string]any) string {
	return varRe.ReplaceAllStringFunc(s, func(m string) string {
		name := m[2 : len(m)-1]
		name = strings.TrimPrefix(name, "params.")
		if v, ok := vars[name]; ok {
			return fmt.Sprint(v)
		}
		return m
	})
}

// GPUTypeMatches reports whether a GPU model (e.g. "NVIDIA GeForce RTX 5090")
// satisfies a gpu_type expression like "3090|4090|5090|h20".
func GPUTypeMatches(expr, model string) bool {
	expr = strings.TrimSpace(expr)
	if expr == "" || expr == "*" || expr == "any" {
		return true
	}
	m := strings.ToLower(model)
	for _, alt := range strings.Split(expr, "|") {
		alt = strings.ToLower(strings.TrimSpace(alt))
		if alt != "" && strings.Contains(m, alt) {
			return true
		}
	}
	return false
}
