package recipes

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func load(t *testing.T, path string) *Recipe {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	r, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestPlanExampleResolves(t *testing.T) {
	r := load(t, "examples/dp-pusht.yaml")
	node, _ := ParseTarget("node:gpu-a")
	env, err := r.ResolveEnv(node, nil)
	if err != nil || env.Type != "conda" || env.Spec != "environment.yaml" {
		t.Fatalf("node env = %+v, %v", env, err)
	}
	q, err := ParseTarget("queue:myqueue/gpu-a100")
	if err != nil || q.Backend != "myqueue" || q.Queue != "gpu-a100" || q.ID != "myqueue/gpu-a100" {
		t.Fatalf("target = %+v, %v", q, err)
	}
	env, err = r.ResolveEnv(q, nil)
	if err != nil || env.Type != "docker" || env.Spec != "" || !strings.HasPrefix(env.Image, "registry.example.com/") {
		t.Fatalf("queue env = %+v, %v", env, err)
	}
	res, _ := r.ResolveResources(q, nil)
	if res.GPUs != 1 || res.GPUPerWorker != 4 || res.WallTimeMin != 240 || res.Workers != 1 {
		t.Fatalf("queue resources = %+v", res)
	}
	res, _ = r.ResolveResources(node, nil)
	if res.GPUs != 1 || res.GPUPerWorker != 0 || res.MinMemGB != 24 {
		t.Fatalf("node resources = %+v", res)
	}
	sets, err := r.Expand([]string{"seed"}, nil)
	if err != nil || len(sets) != 3 || sets[2].Name != "seed=2" {
		t.Fatalf("expand = %+v, %v", sets, err)
	}
	if got := Substitute(r.Run.Cmd, sets[1].Values); got != "python lerobot/train.py policy=diffusion env=pusht seed=1" {
		t.Fatalf("substitute = %q", got)
	}
	sets, _ = r.Expand(nil, nil)
	if len(sets) != 1 || sets[0].Values["seed"] != 0 {
		t.Fatalf("no-matrix expand = %+v", sets)
	}
}

func TestMatrixProductAndOverrides(t *testing.T) {
	r := load(t, "examples/hello/recipe.yaml")
	sets, err := r.Expand([]string{"seed,steps"}, map[string]any{"steps": []any{5, 10}})
	if err != nil || len(sets) != 4 {
		t.Fatalf("got %d sets, %v", len(sets), err)
	}
	if sets[0].Name != "seed=0,steps=5" {
		t.Fatalf("name = %q", sets[0].Name)
	}
	if _, err := r.Expand([]string{"nope"}, nil); err == nil {
		t.Fatal("expected error for unknown matrix param")
	}
}

func TestValidation(t *testing.T) {
	_, err := Parse([]byte("apiVersion: mldojo/v2\nkind: Experiment\nrun: {}\nenv: {default: {type: podman}}\n"))
	if err == nil {
		t.Fatal("expected validation error")
	}
	msg := err.Error()
	for _, want := range []string{"apiVersion", "run.cmd", "env.default.type"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q missing %q", msg, want)
		}
	}
	if _, err := Parse([]byte("run: {cmd: x}\nbogus: 1\n")); err == nil {
		t.Fatal("unknown top-level field should fail")
	}
}

func TestSubstituteLeavesShellVars(t *testing.T) {
	got := Substitute("echo ${seed} ${HOME} ${params.lr}", map[string]any{"seed": 3, "lr": 0.1})
	if got != "echo 3 ${HOME} 0.1" {
		t.Fatalf("got %q", got)
	}
}

func TestGPUType(t *testing.T) {
	if !GPUTypeMatches("3090|4090|5090|h20", "NVIDIA GeForce RTX 5090") {
		t.Fatal("5090 should match")
	}
	if !GPUTypeMatches("3090|h20", "NVIDIA H20") {
		t.Fatal("h20 should match case-insensitively")
	}
	if GPUTypeMatches("4090", "NVIDIA GeForce RTX 4050 Laptop GPU") {
		t.Fatal("4050 must not match 4090")
	}
}

func TestParseTargetErrors(t *testing.T) {
	for _, s := range []string{"gpu-a", "node:", "queue:myqueue", "cloud:x", "node:a/b"} {
		if _, err := ParseTarget(s); err == nil {
			t.Errorf("ParseTarget(%q) should fail", s)
		}
	}
}

// An absolute run.workdir means "the project is already on the node": its
// code sits next to base weights, virtualenvs and simulator assets that
// cannot be bundled or cloned.
func TestInPlaceRun(t *testing.T) {
	const base = "apiVersion: mldojo/v1\nkind: Experiment\n"

	r, err := Parse([]byte(base + "run: {cmd: bash train.sh, workdir: /root/ws/comp/lingbot}\n"))
	if err != nil {
		t.Fatalf("absolute workdir should be accepted: %v", err)
	}
	if r.Code.Source != "none" {
		t.Errorf("code.source = %q, want none inferred from the absolute workdir", r.Code.Source)
	}

	// Explicit none with a relative workdir is fine too: the command runs
	// against whatever is on the node, in a directory of its own.
	if _, err := Parse([]byte(base + "code: {source: none}\nrun: {cmd: echo hi}\n")); err != nil {
		t.Errorf("code.source none should be accepted: %v", err)
	}

	// Shipping code and then ignoring it is a mistake worth naming.
	_, err = Parse([]byte(base + "code: {source: local}\nrun: {cmd: x, workdir: /abs}\n"))
	if err == nil {
		t.Fatal("absolute workdir with code.source local should fail")
	}
	if !strings.Contains(err.Error(), "code.source none") {
		t.Errorf("error should point at code.source none, got %q", err)
	}

	if _, err := Parse([]byte(base + "code: {source: nowhere}\nrun: {cmd: x}\n")); err == nil {
		t.Fatal("unknown code.source should fail")
	}
}

// Every shipped example must stay valid: they are what people copy.
func TestExamplesParse(t *testing.T) {
	var files []string
	err := filepath.WalkDir("examples", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && (strings.HasSuffix(p, ".yaml") || strings.HasSuffix(p, ".yml")) {
			files = append(files, p)
		}
		return nil
	})
	if err != nil || len(files) == 0 {
		t.Fatalf("no example recipes found: %v", err)
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var probe struct {
			Kind string `yaml:"kind"`
		}
		_ = yaml.Unmarshal(b, &probe)
		if probe.Kind != "" && probe.Kind != KindExp {
			continue // sweep specs and node/queue definitions parse elsewhere
		}
		if _, err := Parse(b); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
}

// A model reference is meant to replace a pasted path, so the failure modes
// have to be caught where they are cheap: at parse, not at dispatch.
func TestModelRefValidation(t *testing.T) {
	base := `apiVersion: mldojo/v1
kind: Experiment
metadata: {project: lingbot, name: eval}
run: {cmd: "bash eval.sh ${ckpt}", params: {%s}}
outputs: {%s}
models:
  - {name: %s, version: %s, as: ckpt}
`
	r, err := Parse([]byte(fmt.Sprintf(base, "", "", "place-empty-cup", "latest")))
	if err != nil {
		t.Fatalf("valid reference rejected: %v", err)
	}
	p, n := r.Models[0].Project(r.Metadata.Project)
	if p != "lingbot" || n != "place-empty-cup" {
		t.Errorf("unqualified name resolved to %s/%s, want lingbot/place-empty-cup", p, n)
	}
	if r.Models[0].Param() != "ckpt" {
		t.Errorf("param = %q, want ckpt", r.Models[0].Param())
	}
	if _, err := Parse([]byte(fmt.Sprintf(base, "", "", "other/policy", "3"))); err != nil {
		t.Errorf("qualified name with a version number rejected: %v", err)
	}
	// The reference fills ${ckpt}; a params entry of the same name would
	// silently win, and the run would record a lineage it did not use.
	if _, err := Parse([]byte(fmt.Sprintf(base, "ckpt: /tmp/x", "", "policy", "latest"))); err == nil {
		t.Error("a reference colliding with run.params must be refused")
	}
	if _, err := Parse([]byte(fmt.Sprintf(base, "", "", "policy", "newest"))); err == nil {
		t.Error("an unknown version alias must be refused")
	}
	// Registering what a run produced needs something to register.
	if _, err := Parse([]byte(fmt.Sprintf(base, "", "model: policy", "policy", "latest"))); err == nil {
		t.Error("outputs.model without outputs.checkpoints must be refused")
	}
	if _, err := Parse([]byte(fmt.Sprintf(base, "", "model: policy, checkpoints: 'out/*.pt'", "policy", "latest"))); err != nil {
		t.Errorf("outputs.model with checkpoints rejected: %v", err)
	}
}
