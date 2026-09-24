// Package core orchestrates submissions and cross-cutting run operations on
// top of the backends (submit, cancel, compare, code provenance).
package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"

	"github.com/lovemoon-ai/mldojo/api/internal/auth"
	"github.com/lovemoon-ai/mldojo/api/internal/backends"
	"github.com/lovemoon-ai/mldojo/api/internal/models"
	v1 "github.com/lovemoon-ai/mldojo/proto/mldojo/v1"
	"github.com/lovemoon-ai/mldojo/recipes"
)

type Service struct {
	RT       *backends.Runtime
	Backends *backends.Registry
}

func (s *Service) store() *models.Store { return s.RT.Store }

// Submit parses the recipe, resolves it for the target, expands the matrix
// and creates one run per parameter set.
func (s *Service) Submit(ctx context.Context, req v1.SubmitRequest) (*v1.SubmitResponse, error) {
	r, err := recipes.Parse([]byte(req.RecipeYAML))
	if err != nil {
		return nil, backends.Userf("%v", err)
	}
	if req.Project != "" {
		r.Metadata.Project = req.Project
	}
	if req.Experiment != "" {
		r.Metadata.Name = req.Experiment
	}
	if r.Metadata.Project == "" || r.Metadata.Name == "" {
		return nil, backends.Userf("recipe needs metadata.project and metadata.name (or pass --project/--exp)")
	}
	if !recipes.ValidName(r.Metadata.Project) || !recipes.ValidName(r.Metadata.Name) {
		return nil, backends.Userf("invalid project/experiment name")
	}
	if strings.TrimSpace(req.Target) == "" {
		return nil, backends.Userf("--target is required (e.g. node:local, queue:<plugin>/<queue>)")
	}
	t, err := recipes.ParseTarget(req.Target)
	if err != nil {
		return nil, backends.Userf("%v", err)
	}
	be, err := s.Backends.For(t)
	if err != nil {
		return nil, err
	}
	labels := s.targetLabels(ctx, t)
	env, err := r.ResolveEnv(t, labels)
	if err != nil {
		return nil, backends.Userf("%v", err)
	}
	res, err := r.ResolveResources(t, labels)
	if err != nil {
		return nil, backends.Userf("%v", err)
	}
	if req.GPUs > 0 {
		res.GPUs = req.GPUs
		if t.Kind == "queue" {
			res.GPUPerWorker = req.GPUs
		}
	}
	// Models are resolved before expansion: a reference fills a parameter,
	// so the matrix and ${...} see it like any other.
	rms, err := s.resolveModels(ctx, r, t)
	if err != nil {
		return nil, err
	}
	// --param would win over the resolved path while the run still recorded
	// the version it named: a lineage that lies is worse than none.
	for _, m := range rms {
		if _, ok := req.Params[m.As]; ok {
			return nil, backends.Userf("--param %s would replace the path models: resolved for %s@%d, and the run would still claim to have read it; change models[].version instead",
				m.As, m.Name, m.Version)
		}
	}
	sets, err := r.Expand(req.Matrix, req.Params)
	if err != nil {
		return nil, backends.Userf("%v", err)
	}
	// Datasets must be registered.
	var rds []v1.RunDataset
	for _, d := range r.Datasets {
		ds, err := s.store().GetDataset(ctx, d.Name, d.Version)
		if models.IsNotFound(err) {
			return nil, backends.Userf("dataset %s@%s is not registered (mldojo dataset register ...)", d.Name, d.Version)
		}
		if err != nil {
			return nil, err
		}
		mount := d.Mount
		if mount == "" {
			mount = ds.Mount
		}
		rds = append(rds, v1.RunDataset{Name: ds.Name, Version: ds.Version, Mount: mount})
	}
	code := req.Code
	if code == nil {
		code = &v1.CodeInfo{Source: r.Code.Source, Repo: r.Code.Repo, Ref: r.Code.Ref}
	}
	if code.BundleURI == "" && code.Repo == "" {
		if r.Code.Source == "git" {
			return nil, backends.Userf("code.source git needs code.repo (or submit from a git checkout with the CLI)")
		}
		// Command-only run (e.g. AI/API submissions of scripts that already
		// live on the node); the agent starts in an empty workdir.
		code.Source = "none"
	}
	envJSON, _ := json.Marshal(env)
	resJSON, _ := json.Marshal(res)

	// Validate against the backend before creating anything.
	probe := &v1.Run{Metadata: v1.RunMetadata{CodeBundleURI: code.BundleURI}}
	if req.DryRun && code.Source == "none" {
		probe.Metadata.CodeBundleURI = "blob://dry-run" // the CLI bundles only for real submits
	}
	if err := be.Validate(ctx, &backends.SubmitSpec{Run: probe, Target: t, Env: env, Resources: res}); err != nil {
		return nil, err
	}

	if req.DryRun {
		out := &v1.SubmitResponse{Experiment: v1.Experiment{Project: r.Metadata.Project, Name: r.Metadata.Name, RecipeYAML: req.RecipeYAML, Tags: r.Metadata.Tags}}
		for _, ps := range sets {
			out.Runs = append(out.Runs, v1.Run{Name: ps.Name, Target: t.Raw, BackendKind: t.Kind, BackendID: t.ID,
				Resources: resJSON, Env: envJSON, Status: "dry-run", Project: r.Metadata.Project, Experiment: r.Metadata.Name,
				Metadata: v1.RunMetadata{Params: ps.Values, Cmd: recipes.Substitute(r.Run.Cmd, vars(ps.Values, r)),
					Workdir: recipes.Substitute(r.Run.Workdir, vars(ps.Values, r))}})
		}
		return out, nil
	}

	p, err := s.store().EnsureProject(ctx, r.Metadata.Project)
	if err != nil {
		return nil, err
	}
	exp, err := s.store().UpsertExperiment(ctx, p.ID, r.Metadata.Name, r.Metadata.Description, req.RecipeYAML, r.Metadata.Tags)
	if err != nil {
		return nil, err
	}
	out := &v1.SubmitResponse{Experiment: *exp}
	var specs []*backends.SubmitSpec
	for _, ps := range sets {
		name := ps.Name
		if name == "" {
			name = "run"
		}
		// A pool run is a node run whose node is decided later, by the
		// scheduler, so it is stored with the node backend and no id.
		kind, backendID := t.Kind, t.ID
		if kind == "pool" {
			kind, backendID = "node", ""
		}
		run := &v1.Run{
			ExperimentID: exp.ID, Name: name, Target: t.Raw, BackendKind: kind, BackendID: backendID,
			Resources: resJSON, Env: envJSON, CodeCommit: code.Commit, CodePatchURI: code.PatchURI, Status: v1.PhaseQueued,
			Metadata: v1.RunMetadata{
				Tags: r.Metadata.Tags, Params: ps.Values, Cmd: recipes.Substitute(r.Run.Cmd, vars(ps.Values, r)),
				Workdir: recipes.Substitute(r.Run.Workdir, vars(ps.Values, r)),
				Setup:   recipes.Substitute(r.Run.Setup, vars(ps.Values, r)), Wandb: r.Run.Wandb,
				Notes: req.Notes, CodeSource: code.Source, CodeRepo: code.Repo, CodeRef: code.Ref,
				CodeBundleURI: code.BundleURI, CodeDirty: code.Dirty, Datasets: rds, Models: rms,
				Outputs:  substOutputs(r.Outputs, vars(ps.Values, r)),
				ExtraEnv: substMap(r.Run.Env, vars(ps.Values, r)), LogMode: "realtime",
				Retry: r.Retry, Attempt: 1,
				Submitter: auth.PrincipalFrom(ctx).Actor(), // "api-token" for token auth
			},
		}
		if t.Kind == "queue" {
			run.Metadata.LogMode = "near-realtime"
		}
		if err := s.store().InsertRun(ctx, run); err != nil {
			return nil, err
		}
		s.store().AddEvent(ctx, run.ID, "submitted", map[string]any{"target": t.Raw, "params": ps.Values})
		s.RT.SysLog(run.ID, "submitted to %s: %s", t.Raw, run.Metadata.Cmd)
		full, err := s.store().GetRun(ctx, run.ID)
		if err != nil {
			return nil, err
		}
		out.Runs = append(out.Runs, *full)
		specs = append(specs, &backends.SubmitSpec{Run: full, Target: t, Env: env, Resources: res})
	}
	for _, sp := range specs {
		if err := be.Submit(ctx, sp); err != nil {
			s.RT.Fail(ctx, sp.Run.ID, err)
		}
	}
	if e, err := s.store().GetExperimentByID(ctx, exp.ID); err == nil {
		out.Experiment = *e
	}
	slog.Info("submitted", "project", r.Metadata.Project, "exp", r.Metadata.Name, "target", t.Raw, "runs", len(out.Runs))
	return out, nil
}

// substOutputs resolves parameters in the output paths. A matrix writes one
// directory per point, so without this every run in the matrix would collect
// the same files -- or, more often, none.
func substOutputs(o recipes.OutputsBlock, v map[string]any) *v1.Outputs {
	out := &v1.Outputs{
		Logs:        recipes.Substitute(o.Logs, v),
		Checkpoints: recipes.Substitute(o.Checkpoints, v),
		Videos:      recipes.Substitute(o.Videos, v),
		Images:      recipes.Substitute(o.Images, v),
		Model:       o.Model,
	}
	for _, m := range o.Metrics {
		out.Metrics = append(out.Metrics, v1.MetricsSource{Type: m.Type, Path: recipes.Substitute(m.Path, v)})
	}
	for _, e := range o.Episodes {
		out.Episodes = append(out.Episodes, v1.EpisodesSource{Type: e.Type, Path: recipes.Substitute(e.Path, v)})
	}
	return out
}

func vars(params map[string]any, r *recipes.Recipe) map[string]any {
	v := map[string]any{"project": r.Metadata.Project, "experiment": r.Metadata.Name}
	for k, x := range params {
		v[k] = x
	}
	return v
}

func substMap(m map[string]string, v map[string]any) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, x := range m {
		out[k] = recipes.Substitute(x, v)
	}
	return out
}

func (s *Service) targetLabels(ctx context.Context, t recipes.Target) []string {
	switch t.Kind {
	case "node":
		if n, err := s.store().GetNode(ctx, t.ID); err == nil {
			return n.Labels
		}
	case "queue":
		if q, err := s.store().GetQueue(ctx, t.ID); err == nil {
			return q.Labels
		}
	}
	return nil
}

// Cancel cancels a run through its backend.
func (s *Service) Cancel(ctx context.Context, id string) (*v1.Run, error) {
	run, err := s.store().GetRun(ctx, id)
	if err != nil {
		return nil, err
	}
	be, err := s.Backends.ForRun(run)
	if err != nil {
		return nil, err
	}
	if err := be.Cancel(ctx, run); err != nil {
		return nil, err
	}
	return s.store().GetRun(ctx, run.ID)
}

// Delete removes a run and its files. A run that is still active is refused
// unless force is set, in which case it is cancelled first -- deleting the
// row out from under a running agent would leave the process orphaned.
func (s *Service) Delete(ctx context.Context, id string, force bool) (string, error) {
	run, err := s.store().GetRun(ctx, id)
	if err != nil {
		return "", err
	}
	if p := auth.PrincipalFrom(ctx); !p.Owns(run.Metadata.Submitter) {
		return "", models.Forbidden("this run was submitted by %s", run.Metadata.Submitter)
	}
	if !v1.Terminal(run.Status) {
		if !force {
			return "", models.Conflict("run %s is %s: cancel it first, or delete with force", run.ID, run.Status)
		}
		if _, err := s.Cancel(ctx, run.ID); err != nil {
			return "", err
		}
	}
	deleted, err := s.store().DeleteRun(ctx, run.ID)
	if err != nil {
		return "", err
	}
	if err := s.RT.Logs.RemoveRun(deleted); err != nil {
		slog.Warn("delete run files", "run", deleted, "err", err)
	}
	return deleted, nil
}

// CodeInfo is GET /runs/{id}/code.
type CodeInfo struct {
	Source    string `json:"source"`
	Repo      string `json:"repo"`
	Ref       string `json:"ref"`
	Commit    string `json:"commit"`
	Dirty     bool   `json:"dirty"`
	Patch     string `json:"patch"`
	PatchURI  string `json:"patch_uri"`
	BundleURI string `json:"bundle_uri"`
}

func (s *Service) Code(ctx context.Context, run *v1.Run) (*CodeInfo, error) {
	ci := &CodeInfo{Source: run.Metadata.CodeSource, Repo: run.Metadata.CodeRepo, Ref: run.Metadata.CodeRef,
		Commit: run.CodeCommit, Dirty: run.Metadata.CodeDirty, PatchURI: run.CodePatchURI, BundleURI: run.Metadata.CodeBundleURI}
	if run.CodePatchURI != "" {
		f, err := s.RT.Logs.OpenBlob(run.CodePatchURI)
		if err == nil {
			b, _ := io.ReadAll(io.LimitReader(f, 4<<20))
			f.Close()
			ci.Patch = string(b)
		}
	}
	return ci, nil
}

// Compare is GET /compare?a=&b=.
type Compare struct {
	A       v1.Run        `json:"a"`
	B       v1.Run        `json:"b"`
	Code    CodeCompare   `json:"code"`
	Metrics []MetricDelta `json:"metrics"`
	Env     []EnvDelta    `json:"env"`
}

type CodeCompare struct {
	SameCommit bool   `json:"same_commit"`
	CommitA    string `json:"commit_a"`
	CommitB    string `json:"commit_b"`
	DirtyA     bool   `json:"dirty_a"`
	DirtyB     bool   `json:"dirty_b"`
	SamePatch  bool   `json:"same_patch"`
	PatchA     string `json:"patch_a,omitempty"`
	PatchB     string `json:"patch_b,omitempty"`
}

type MetricDelta struct {
	Key   string    `json:"key"`
	A     *v1.Float `json:"a"`
	B     *v1.Float `json:"b"`
	StepA *int64    `json:"step_a"`
	StepB *int64    `json:"step_b"`
	Delta *v1.Float `json:"delta"`
}

type EnvDelta struct {
	Path string `json:"path"`
	A    any    `json:"a"`
	B    any    `json:"b"`
}

func (s *Service) Compare(ctx context.Context, a, b string) (*Compare, error) {
	ra, err := s.store().GetRun(ctx, a)
	if err != nil {
		return nil, err
	}
	rb, err := s.store().GetRun(ctx, b)
	if err != nil {
		return nil, err
	}
	out := &Compare{A: *ra, B: *rb}
	out.Code = CodeCompare{CommitA: ra.CodeCommit, CommitB: rb.CodeCommit, DirtyA: ra.Metadata.CodeDirty, DirtyB: rb.Metadata.CodeDirty,
		SameCommit: ra.CodeCommit == rb.CodeCommit, SamePatch: ra.CodePatchURI == rb.CodePatchURI}
	if !out.Code.SamePatch {
		ca, _ := s.Code(ctx, ra)
		cb, _ := s.Code(ctx, rb)
		out.Code.PatchA, out.Code.PatchB = ca.Patch, cb.Patch
	}
	ma, err := s.store().LatestMetrics(ctx, ra.ID)
	if err != nil {
		return nil, err
	}
	mb, err := s.store().LatestMetrics(ctx, rb.ID)
	if err != nil {
		return nil, err
	}
	keys := map[string]bool{}
	for k := range ma {
		keys[k] = true
	}
	for k := range mb {
		keys[k] = true
	}
	for k := range keys {
		d := MetricDelta{Key: k}
		if p, ok := ma[k]; ok {
			v, st := v1.Float(p.Value), p.Step
			d.A, d.StepA = &v, &st
		}
		if p, ok := mb[k]; ok {
			v, st := v1.Float(p.Value), p.Step
			d.B, d.StepB = &v, &st
		}
		if d.A != nil && d.B != nil {
			x := *d.B - *d.A
			d.Delta = &x
		}
		out.Metrics = append(out.Metrics, d)
	}
	sort.Slice(out.Metrics, func(i, j int) bool { return out.Metrics[i].Key < out.Metrics[j].Key })
	fa, fb := flatRun(ra), flatRun(rb)
	paths := map[string]bool{}
	for k := range fa {
		paths[k] = true
	}
	for k := range fb {
		paths[k] = true
	}
	for p := range paths {
		if fmt.Sprint(fa[p]) != fmt.Sprint(fb[p]) {
			out.Env = append(out.Env, EnvDelta{Path: p, A: fa[p], B: fb[p]})
		}
	}
	sort.Slice(out.Env, func(i, j int) bool { return out.Env[i].Path < out.Env[j].Path })
	if out.Metrics == nil {
		out.Metrics = []MetricDelta{}
	}
	if out.Env == nil {
		out.Env = []EnvDelta{}
	}
	return out, nil
}

func flatRun(r *v1.Run) map[string]any {
	m := map[string]any{"target": r.Target, "cmd": r.Metadata.Cmd, "workdir": r.Metadata.Workdir}
	var env, res any
	json.Unmarshal(r.Env, &env)
	json.Unmarshal(r.Resources, &res)
	flatten("env", env, m)
	flatten("resources", res, m)
	flatten("params", map[string]any(r.Metadata.Params), m)
	flatten("extra_env", toAny(r.Metadata.ExtraEnv), m)
	return m
}

func toAny(m map[string]string) map[string]any {
	out := map[string]any{}
	for k, v := range m {
		out[k] = v
	}
	return out
}

func flatten(prefix string, v any, out map[string]any) {
	switch x := v.(type) {
	case map[string]any:
		for k, vv := range x {
			flatten(prefix+"."+k, vv, out)
		}
	case nil:
	default:
		out[prefix] = x
	}
}

// ErrStatus maps an error to (HTTP status, code).
func ErrStatus(err error) (int, string) {
	var ue *backends.UserError
	var un *backends.UnreachableError
	switch {
	case errors.As(err, &ue):
		return 400, "user_error"
	case errors.As(err, &un):
		return 503, "unreachable"
	case models.IsNotFound(err):
		return 404, "not_found"
	case models.IsConflict(err):
		return 409, "conflict"
	case models.IsForbidden(err):
		return 403, "forbidden"
	case models.IsUserError(err):
		return 400, "user_error"
	case errors.Is(err, context.DeadlineExceeded):
		return 504, "unreachable"
	}
	msg := err.Error()
	if strings.Contains(msg, "secrets are locked") || strings.Contains(msg, "invalid secret") {
		return 400, "user_error"
	}
	if strings.HasPrefix(msg, "agent ") || strings.Contains(msg, "queue plugin") {
		return 502, "backend_error"
	}
	return 500, "internal"
}
