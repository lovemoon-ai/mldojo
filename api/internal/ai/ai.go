// Package ai implements the AI-friendly endpoints: pre-joined,
// short, stable JSON answers to the questions agents ask most.
package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/lovemoon-ai/mldojo/api/internal/backends"
	"github.com/lovemoon-ai/mldojo/api/internal/core"
	"github.com/lovemoon-ai/mldojo/api/internal/models"
	v1 "github.com/lovemoon-ai/mldojo/proto/mldojo/v1"
	"gopkg.in/yaml.v3"
)

// aiMaxPoints is the per-key sampling budget for analysis: enough shape to
// spot spikes and plateaus without pulling millions of rows.
const aiMaxPoints = 20000

type AI struct {
	Svc *core.Service
	Hub *backends.Hub
}

func (a *AI) store() *models.Store { return a.Svc.RT.Store }

type Brief struct {
	ID            string              `json:"id"`
	Project       string              `json:"project"`
	Exp           string              `json:"exp"`
	Name          string              `json:"name"`
	Status        string              `json:"status"`
	Progress      *float64            `json:"progress"`
	Step          int64               `json:"step"`
	LatestMetrics map[string]v1.Float `json:"latest_metrics"`
	Anomalies     []string            `json:"anomalies"`
	BestCkpt      *Ckpt               `json:"best_ckpt"`
	// Eval is set when the run recorded episodes. "15/20" is the answer an
	// evaluation is asked for; a success-rate float is not.
	Eval       *v1.EpisodeSummary `json:"eval,omitempty"`
	Target     string             `json:"target"`
	Elapsed    string             `json:"elapsed"`
	ElapsedSec int64              `json:"elapsed_sec"`
	ETA        string             `json:"eta"`
	ExitCode   *int               `json:"exit_code,omitempty"`
	Message    string             `json:"message,omitempty"`
	Summary    string             `json:"summary,omitempty"`
}

type Ckpt struct {
	Step   int64     `json:"step"`
	Metric *v1.Float `json:"metric"`
	URI    string    `json:"uri"`
}

var lossLike = regexp.MustCompile(`(?i)(loss|err|error|mse|mae|perplexity|ppl|nll)`)

// bestLossLike picks the headline loss when none of the well-known names is
// present. Taking the first match in key order is a lottery: a real run
// logs 335 keys, seven of which are losses, and it picked
// "training/router_z_loss" -- a MoE router diagnostic -- to describe the
// job. Prefer the plainest name: an exact "loss" leaf, then the fewest
// qualifiers, then the shortest.
func bestLossLike(keys []string) string {
	var cands []string
	for _, k := range keys {
		if lossLike.MatchString(k) {
			cands = append(cands, k)
		}
	}
	if len(cands) == 0 {
		return ""
	}
	leaf := func(k string) string {
		if i := strings.LastIndexAny(k, "/_"); i >= 0 {
			return strings.ToLower(k[i+1:])
		}
		return strings.ToLower(k)
	}
	rank := func(k string) int {
		if leaf(k) == "loss" {
			return 0
		}
		return 1
	}
	sort.Slice(cands, func(i, j int) bool {
		if ri, rj := rank(cands[i]), rank(cands[j]); ri != rj {
			return ri < rj
		}
		if li, lj := len(cands[i]), len(cands[j]); li != lj {
			return li < lj
		}
		return cands[i] < cands[j]
	})
	return cands[0]
}

// PrimaryMetric picks the metric to judge a run by and whether lower is better.
func PrimaryMetric(keys []string) (string, bool) {
	set := map[string]bool{}
	for _, k := range keys {
		set[k] = true
	}
	// An evaluation outranks any loss. Where both exist the run is a policy
	// being judged on whether it completes the task, and the loss says very
	// little: on the task this rule was written for it sat at 0.06-0.08 from
	// the first checkpoint to the last while the success rate went 40% to 75%.
	if set[v1.MetricSuccessRate] {
		return v1.MetricSuccessRate, false
	}
	prefer := []string{"val/loss", "val_loss", "eval/loss", "eval_loss", "loss",
		"train/loss", "train_loss", "training/loss"}
	for _, p := range prefer {
		if set[p] {
			return p, true
		}
	}
	if k := bestLossLike(keys); k != "" {
		return k, true
	}
	for _, k := range keys {
		l := strings.ToLower(k)
		if strings.Contains(l, "reward") || strings.Contains(l, "acc") || strings.Contains(l, "success") || strings.Contains(l, "return") {
			return k, false
		}
	}
	if len(keys) > 0 {
		sort.Strings(keys)
		return keys[0], true
	}
	return "", true
}

var totalParam = []string{"max_steps", "total_steps", "steps", "num_steps", "train_steps", "iterations", "iters"}
var stepRe = regexp.MustCompile(`(?i)(?:step|iter|it|ckpt|checkpoint|epoch)[_-]?(\d+)`)
var numRe = regexp.MustCompile(`(\d+)`)

func (a *AI) Brief(ctx context.Context, id string, withSummary bool) (*Brief, error) {
	run, err := a.store().GetRun(ctx, id)
	if err != nil {
		return nil, err
	}
	latest, err := a.store().LatestMetrics(ctx, run.ID)
	if err != nil {
		return nil, err
	}
	b := &Brief{ID: run.ID, Project: run.Project, Exp: run.Experiment, Name: run.Name, Status: run.Status, Target: run.Target,
		LatestMetrics: map[string]v1.Float{}, ExitCode: run.ExitCode, Message: run.Metadata.Message, Anomalies: []string{}}
	keys := []string{}
	for k, p := range latest {
		b.LatestMetrics[k] = v1.Float(round(p.Value))
		keys = append(keys, k)
		if p.Step > b.Step {
			b.Step = p.Step
		}
	}
	var elapsed time.Duration
	if run.StartedAt != nil {
		end := time.Now()
		if run.FinishedAt != nil {
			end = *run.FinishedAt
		}
		elapsed = end.Sub(*run.StartedAt)
	}
	b.Elapsed, b.ElapsedSec = elapsed.Round(time.Second).String(), int64(elapsed.Seconds())
	for _, k := range totalParam {
		if v, ok := run.Metadata.Params[k]; ok {
			if total, err := strconv.ParseFloat(fmt.Sprint(v), 64); err == nil && total > 0 && b.Step > 0 {
				p := math.Min(float64(b.Step)/total, 1)
				p = round(p)
				b.Progress = &p
				if p > 0 && p < 1 && !v1.Terminal(run.Status) {
					b.ETA = (time.Duration(float64(elapsed) * (1 - p) / p)).Round(time.Second).String()
				}
			}
			break
		}
	}
	if run.Status == v1.PhaseSucceeded {
		one := 1.0
		b.Progress = &one
	}
	b.Anomalies, _ = a.anomalies(ctx, run)
	// Best checkpoint: the checkpoint closest to the best primary-metric step.
	arts, _ := a.store().ListArtifacts(ctx, run.ID)
	metric, lower := PrimaryMetric(keys)
	var pts []v1.MetricPoint
	if metric != "" {
		pts, _, _ = a.store().ListMetrics(ctx, run.ID, metric, 0, aiMaxPoints)
	}
	for _, art := range arts {
		if art.Kind != "ckpt" {
			continue
		}
		step := ckptStep(art.URI)
		c := &Ckpt{Step: step, URI: art.URI}
		if v, ok := valueNear(pts, step); ok {
			fv := v1.Float(v)
			c.Metric = &fv
		}
		if b.BestCkpt == nil || better(c, b.BestCkpt, lower) {
			b.BestCkpt = c
		}
	}
	if eps, err := a.store().ListEpisodes(ctx, run.ID); err == nil && len(eps) > 0 {
		sum := v1.Summarize(eps)
		b.Eval = &sum
	}
	if withSummary {
		b.Summary = briefSummary(b, metric)
	}
	return b, nil
}

func better(a, b *Ckpt, lower bool) bool {
	if a.Metric != nil && b.Metric != nil && *a.Metric != *b.Metric {
		if lower {
			return *a.Metric < *b.Metric
		}
		return *a.Metric > *b.Metric
	}
	if a.Metric != nil && b.Metric == nil {
		return true
	}
	return a.Step > b.Step
}

func ckptStep(uri string) int64 {
	base := uri[strings.LastIndex(uri, "/")+1:]
	if m := stepRe.FindStringSubmatch(base); m != nil {
		n, _ := strconv.ParseInt(m[1], 10, 64)
		return n
	}
	all := numRe.FindAllString(base, -1)
	if len(all) > 0 {
		n, _ := strconv.ParseInt(all[len(all)-1], 10, 64)
		return n
	}
	return 0
}

func valueNear(pts []v1.MetricPoint, step int64) (float64, bool) {
	best, bestD := 0.0, int64(-1)
	for _, p := range pts {
		d := p.Step - step
		if d < 0 {
			d = -d
		}
		if bestD < 0 || d < bestD {
			best, bestD = p.Value, d
		}
	}
	return round(best), bestD >= 0
}

func round(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return v
	}
	return math.Round(v*1e6) / 1e6
}

func briefSummary(b *Brief, metric string) string {
	var parts []string
	health := "healthy"
	if len(b.Anomalies) > 0 {
		health = "showing anomalies (" + strings.Join(b.Anomalies, ", ") + ")"
	}
	switch b.Status {
	case v1.PhaseRunning:
		parts = append(parts, "Run is "+health)
	case v1.PhaseSucceeded:
		parts = append(parts, "Run succeeded")
	case v1.PhaseFailed:
		msg := "Run failed"
		if b.ExitCode != nil {
			msg += fmt.Sprintf(" with exit code %d", *b.ExitCode)
		}
		if b.Message != "" {
			msg += ": " + b.Message
		}
		parts = append(parts, msg)
	default:
		parts = append(parts, "Run is "+b.Status)
	}
	if b.Eval != nil {
		parts = append(parts, fmt.Sprintf("%d/%d episodes succeeded (%.0f%%)",
			b.Eval.Successes, b.Eval.Total, b.Eval.SuccessRate*100))
	} else if metric != "" {
		if v, ok := b.LatestMetrics[metric]; ok {
			parts = append(parts, fmt.Sprintf("%s %.4g at step %s", metric, float64(v), humanInt(b.Step)))
		}
	}
	if b.Progress != nil {
		parts = append(parts, fmt.Sprintf("%.0f%% done", *b.Progress*100))
	}
	if b.ETA != "" {
		parts = append(parts, "ETA "+b.ETA)
	}
	parts = append(parts, "on "+b.Target+" for "+b.Elapsed)
	return strings.Join(parts, ", ") + "."
}

func humanInt(n int64) string {
	if n >= 1000 && n%100 == 0 {
		return fmt.Sprintf("%gk", float64(n)/1000)
	}
	return strconv.FormatInt(n, 10)
}

// anomalies scans metrics/logs for common problems.
func (a *AI) anomalies(ctx context.Context, run *v1.Run) ([]string, error) {
	out := []string{}
	pts, _, err := a.store().ListMetrics(ctx, run.ID, "", 0, aiMaxPoints)
	if err != nil {
		return out, err
	}
	byKey := map[string][]v1.MetricPoint{}
	for _, p := range pts {
		byKey[p.Key] = append(byKey[p.Key], p)
	}
	keys := make([]string, 0, len(byKey))
	for k := range byKey {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		series := byKey[k]
		name := strings.ReplaceAll(k, "/", "_")
		for _, p := range series {
			if math.IsNaN(p.Value) || math.IsInf(p.Value, 0) {
				out = append(out, fmt.Sprintf("nan_%s@step_%d", name, p.Step))
				break
			}
		}
		if lossLike.MatchString(k) && len(series) >= 20 {
			// Spike: last value > 3x the median of the previous window.
			last := series[len(series)-1].Value
			win := series[max(0, len(series)-51) : len(series)-1]
			vals := make([]float64, 0, len(win))
			for _, p := range win {
				if !math.IsNaN(p.Value) && !math.IsInf(p.Value, 0) {
					vals = append(vals, p.Value)
				}
			}
			sort.Float64s(vals)
			if len(vals) > 0 {
				med := vals[len(vals)/2]
				if med > 0 && last > 3*med {
					out = append(out, fmt.Sprintf("spike_%s@step_%d", name, series[len(series)-1].Step))
				}
			}
			// Divergence: the second half is higher on average than the first.
			h := len(series) / 2
			if mean(series[h:]) > 1.5*mean(series[:h]) && mean(series[:h]) > 0 {
				out = append(out, fmt.Sprintf("diverging_%s", name))
			}
		}
	}
	if run.Status == v1.PhaseRunning {
		lastTS := time.Time{}
		for _, p := range pts {
			if p.TS.After(lastTS) {
				lastTS = p.TS
			}
		}
		if !lastTS.IsZero() && time.Since(lastTS) > 30*time.Minute {
			out = append(out, fmt.Sprintf("stalled_metrics_%dm", int(time.Since(lastTS).Minutes())))
		}
	}
	if run.Status == v1.PhaseFailed {
		tail, _ := a.Svc.RT.Logs.Tail(run.ID, v1.StreamStderr, 8192)
		if len(tail) == 0 {
			tail, _ = a.Svc.RT.Logs.Tail(run.ID, v1.StreamStdout, 8192)
		}
		for _, pat := range []struct{ re, tag string }{
			{`(?i)CUDA out of memory|OutOfMemoryError`, "oom_cuda"},
			{`(?i)Killed|MemoryError`, "oom_host"},
			{`ModuleNotFoundError|ImportError`, "import_error"},
			{`(?i)No such file or directory|FileNotFoundError`, "file_not_found"},
			{`(?i)NCCL`, "nccl_error"},
			{`Traceback \(most recent call last\)`, "python_exception"},
		} {
			if regexp.MustCompile(pat.re).Match(tail) {
				out = append(out, pat.tag)
			}
		}
	}
	return out, nil
}

func mean(ps []v1.MetricPoint) float64 {
	s, n := 0.0, 0
	for _, p := range ps {
		if !math.IsNaN(p.Value) && !math.IsInf(p.Value, 0) {
			s += p.Value
			n++
		}
	}
	if n == 0 {
		return 0
	}
	return s / float64(n)
}

// AnomalyCheck runs a scan and records it as a run event.
func (a *AI) AnomalyCheck(ctx context.Context, id string) (map[string]any, error) {
	run, err := a.store().GetRun(ctx, id)
	if err != nil {
		return nil, err
	}
	an, err := a.anomalies(ctx, run)
	if err != nil {
		return nil, err
	}
	a.store().AddEvent(ctx, run.ID, "anomaly_check", map[string]any{"anomalies": an})
	return map[string]any{"run_id": run.ID, "anomalies": an, "checked_at": time.Now()}, nil
}

type FreeNode struct {
	NodeID    string   `json:"node_id"`
	GPUsFree  int      `json:"gpus_free"` // no MLDojo run and enough free memory
	GPUsTotal int      `json:"gpus_total"`
	GPUsHeld  int      `json:"gpus_held"` // somebody else's process is on them
	GPUModel  string   `json:"gpu_model"`
	MemFreeGB float64  `json:"mem_free"`    // GiB free on the free GPUs
	MaxFreeGB float64  `json:"max_free_gb"` // GiB free on the single emptiest card
	Labels    []string `json:"labels"`
	Target    string   `json:"target"`
	// Holders names what is sitting on the held cards, so "why can I only
	// use 3 of 29 GPUs" has an answer rather than just a number.
	Holders []string `json:"holders,omitempty"`
}

// FreeQuery filters FreeNodes. Without it an agent has to fetch every node
// and filter client-side, and cannot express the one thing it actually
// knows: how much memory its job needs.
type FreeQuery struct {
	GPUs      int      // at least this many free cards
	MinFreeGB float64  // each of them with at least this much free memory
	Labels    []string // all of these labels
}

// idleFreeGB is how much memory a card must have free before it counts as
// free when the caller did not say. Small enough that a card holding only a
// display server still counts, large enough that a real job does not.
const idleFreeGB = 0.9

// FreeNodes lists online nodes with GPUs something could run on.
//
// "Free" means no MLDojo run is on the card and enough of its memory is
// unheld. Utilisation is deliberately not a criterion: a card at 0% with
// 25 GiB held is not available, and a card at 40% with 90 GiB free often
// is. Held cards are reported separately rather than silently dropped.
func (a *AI) FreeNodes(ctx context.Context, q FreeQuery) ([]FreeNode, error) {
	nodes, err := a.store().ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	needGB := q.MinFreeGB
	if needGB <= 0 {
		needGB = idleFreeGB
	}
	out := []FreeNode{}
	for _, n := range nodes {
		if a.Hub.Conn(n.ID) == nil || !hasLabels(n.Labels, q.Labels) {
			continue
		}
		stats := a.Hub.GPU(n.ID)
		f := FreeNode{NodeID: n.ID, Labels: n.Labels, GPUsTotal: len(stats), Target: "node:" + n.ID}
		holders := map[string]bool{}
		for _, g := range stats {
			if f.GPUModel == "" {
				f.GPUModel = g.Model
			}
			freeGB := g.FreeMB() / 1024
			if freeGB > f.MaxFreeGB {
				f.MaxFreeGB = math.Round(freeGB*10) / 10
			}
			// Held means somebody else is on the card, whether or not
			// enough is left for us. Counting it only when the card is full
			// reports "held 0" about a machine with twenty-five foreign
			// processes on it, which is the opposite of useful.
			if fn, _ := g.Foreign(); fn > 0 {
				f.GPUsHeld++
				for _, p := range g.Procs {
					if p.RunID == "" {
						holders[describeHolder(p)] = true
					}
				}
			}
			if len(g.RunIDs) > 0 {
				continue
			}
			if freeGB >= needGB {
				f.GPUsFree++
				f.MemFreeGB += freeGB
			}
		}
		f.MemFreeGB = math.Round(f.MemFreeGB*10) / 10
		for h := range holders {
			f.Holders = append(f.Holders, h)
		}
		sort.Strings(f.Holders)
		if len(stats) == 0 {
			// A CPU-only node is free when nothing of ours is on it, but
			// never satisfies a request for GPUs.
			if n.ActiveRuns == 0 && q.GPUs == 0 {
				out = append(out, f)
			}
			continue
		}
		if f.GPUsFree >= max(q.GPUs, 1) {
			out = append(out, f)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GPUsFree > out[j].GPUsFree })
	return out, nil
}

// describeHolder names a process holding a card as precisely as the node can.
// A pid with no owner is a container's or a leak, and saying so is the point.
func describeHolder(p v1.GPUProc) string {
	name := p.Name
	if name == "" {
		name = "unknown process"
	}
	who := p.User
	if who == "" {
		who = "no local owner"
	}
	return fmt.Sprintf("%s (pid %d, %s, %.0f MiB)", name, p.PID, who, p.MemUsedMB)
}

func hasLabels(have, want []string) bool {
	for _, w := range want {
		found := false
		for _, h := range have {
			if strings.EqualFold(h, w) {
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

// LooseSubmit is POST /ai/runs: a forgiving JSON shape turned into a recipe.
type LooseSubmit struct {
	Project string            `json:"project"`
	Exp     string            `json:"exp"`
	Target  string            `json:"target"`
	Cmd     string            `json:"cmd"`
	Image   string            `json:"image"`
	GPUs    int               `json:"gpus"`
	Seeds   []any             `json:"seeds"`
	Params  map[string]any    `json:"params"`
	Env     map[string]string `json:"env"`
	Repo    string            `json:"repo"`
	Ref     string            `json:"ref"`
	Code    *v1.CodeInfo      `json:"code"`
	Workdir string            `json:"workdir"`
	Outputs map[string]any    `json:"outputs"`
	DryRun  bool              `json:"dry_run"`
}

func (a *AI) Submit(ctx context.Context, l LooseSubmit) (*v1.SubmitResponse, error) {
	if l.Project == "" || l.Target == "" || l.Cmd == "" {
		return nil, backends.Userf("project, target and cmd are required")
	}
	if l.Exp == "" {
		l.Exp = "ai-" + time.Now().Format("20060102-150405")
	}
	recipe := map[string]any{
		"apiVersion": "mldojo/v1", "kind": "Experiment",
		"metadata": map[string]any{"project": l.Project, "name": l.Exp, "tags": []string{"ai"}},
		"run":      map[string]any{"cmd": l.Cmd},
	}
	runB := recipe["run"].(map[string]any)
	params := map[string]any{}
	for k, v := range l.Params {
		params[k] = v
	}
	var matrix []string
	if len(l.Seeds) > 0 {
		params["seed"] = l.Seeds
		matrix = append(matrix, "seed")
		if !strings.Contains(l.Cmd, "${seed}") {
			runB["env"] = map[string]string{"SEED": "${seed}"}
		}
	}
	if len(params) > 0 {
		runB["params"] = params
	}
	if l.Workdir != "" {
		runB["workdir"] = l.Workdir
	}
	if len(l.Env) > 0 {
		env := map[string]string{}
		for k, v := range l.Env {
			env[k] = v
		}
		if e, ok := runB["env"].(map[string]string); ok {
			for k, v := range e {
				env[k] = v
			}
		}
		runB["env"] = env
	}
	if l.Image != "" {
		recipe["env"] = map[string]any{"default": map[string]any{"type": "docker", "image": l.Image}}
	}
	if l.GPUs > 0 {
		recipe["resources"] = map[string]any{"default": map[string]any{"gpus": l.GPUs}}
	}
	if l.Repo != "" {
		recipe["code"] = map[string]any{"source": "git", "repo": l.Repo, "ref": l.Ref}
	}
	if len(l.Outputs) > 0 {
		recipe["outputs"] = l.Outputs
	}
	y, err := yaml.Marshal(recipe)
	if err != nil {
		return nil, err
	}
	return a.Svc.Submit(ctx, v1.SubmitRequest{RecipeYAML: string(y), Target: l.Target, Matrix: matrix, Code: l.Code, DryRun: l.DryRun})
}

// ExpSummary is GET /ai/experiments/{project}/{exp}/summary.
type ExpSummary struct {
	Project   string         `json:"project"`
	Exp       string         `json:"exp"`
	Runs      int            `json:"runs"`
	ByStatus  map[string]int `json:"by_status"`
	Metric    string         `json:"metric"`
	Lower     bool           `json:"lower_is_better"`
	BestRun   *RunScore      `json:"best_run"`
	Ranking   []RunScore     `json:"ranking"`
	NextSteps []string       `json:"next_steps"`
	Summary   string         `json:"summary,omitempty"`
}

type RunScore struct {
	ID     string         `json:"id"`
	Name   string         `json:"name"`
	Status string         `json:"status"`
	Value  *v1.Float      `json:"value"`
	Step   int64          `json:"step"`
	Params map[string]any `json:"params,omitempty"`
}

func (a *AI) ExperimentSummary(ctx context.Context, project, exp string, withSummary bool) (*ExpSummary, error) {
	if _, err := a.store().GetExperiment(ctx, project, exp); err != nil {
		return nil, err
	}
	runs, err := a.store().ListRuns(ctx, models.RunFilter{Project: project, Experiment: exp, Limit: 1000})
	if err != nil {
		return nil, err
	}
	s := &ExpSummary{Project: project, Exp: exp, Runs: len(runs), ByStatus: map[string]int{}, Ranking: []RunScore{}, NextSteps: []string{}}
	latest := map[string]map[string]v1.MetricPoint{}
	keySet := map[string]bool{}
	for _, r := range runs {
		s.ByStatus[r.Status]++
		m, _ := a.store().LatestMetrics(ctx, r.ID)
		latest[r.ID] = m
		for k := range m {
			keySet[k] = true
		}
	}
	keys := make([]string, 0, len(keySet))
	for k := range keySet {
		keys = append(keys, k)
	}
	s.Metric, s.Lower = PrimaryMetric(keys)
	for _, r := range runs {
		rs := RunScore{ID: r.ID, Name: r.Name, Status: r.Status, Params: r.Metadata.Params}
		if p, ok := latest[r.ID][s.Metric]; ok && !math.IsNaN(p.Value) {
			v := v1.Float(round(p.Value))
			rs.Value, rs.Step = &v, p.Step
		}
		s.Ranking = append(s.Ranking, rs)
	}
	sort.SliceStable(s.Ranking, func(i, j int) bool {
		a, b := s.Ranking[i].Value, s.Ranking[j].Value
		if a == nil || b == nil {
			return a != nil
		}
		if s.Lower {
			return *a < *b
		}
		return *a > *b
	})
	if len(s.Ranking) > 0 && s.Ranking[0].Value != nil {
		best := s.Ranking[0]
		s.BestRun = &best
	}
	// Heuristic next steps.
	if n := s.ByStatus[v1.PhaseFailed]; n > 0 {
		s.NextSteps = append(s.NextSteps, fmt.Sprintf("%d run(s) failed: inspect with `mldojo ai brief <run>` / `mldojo run logs <run> --stream stderr`", n))
	}
	if n := s.ByStatus[v1.PhaseRunning] + s.ByStatus[v1.PhaseQueued] + s.ByStatus[v1.PhaseStarting]; n > 0 {
		s.NextSteps = append(s.NextSteps, fmt.Sprintf("%d run(s) still active; re-check later", n))
	}
	if s.BestRun != nil && len(s.Ranking) >= 2 && s.Ranking[1].Value != nil {
		spread := math.Abs(float64(*s.BestRun.Value - *worstScored(s.Ranking)))
		s.NextSteps = append(s.NextSteps, fmt.Sprintf("best %s=%.4g (%s); spread across runs %.4g", s.Metric, float64(*s.BestRun.Value), s.BestRun.Name, spread))
		if _, hasSeed := s.BestRun.Params["seed"]; hasSeed && len(s.Ranking) < 5 {
			s.NextSteps = append(s.NextSteps, "few seeds: add more seeds (--matrix seed) before trusting the ranking")
		}
	}
	if s.Runs == 0 {
		s.NextSteps = append(s.NextSteps, "no runs yet: `mldojo run submit -f recipe.yaml --target <target>`")
	}
	if withSummary {
		parts := []string{fmt.Sprintf("%s/%s has %d runs", project, exp, s.Runs)}
		st := []string{}
		for k, v := range s.ByStatus {
			st = append(st, fmt.Sprintf("%d %s", v, k))
		}
		sort.Strings(st)
		if len(st) > 0 {
			parts = append(parts, "("+strings.Join(st, ", ")+")")
		}
		if s.BestRun != nil {
			parts = append(parts, fmt.Sprintf("; best is %s with %s=%.4g", s.BestRun.Name, s.Metric, float64(*s.BestRun.Value)))
		}
		s.Summary = strings.Join(parts, " ") + "."
	}
	return s, nil
}

// worstScored is the lowest-ranked run that actually has a number.
//
// Runs without one sort last, so the last run is not the worst -- it is a
// run with no result. Reading its value panicked the whole endpoint the
// first time a real experiment had a cancelled run in it, which is every
// experiment anybody has iterated on.
func worstScored(ranking []RunScore) *v1.Float {
	var worst *v1.Float
	for i := range ranking {
		if ranking[i].Value != nil {
			worst = ranking[i].Value
		}
	}
	return worst
}

// MarshalCompact is a helper for tests/CLI.
func MarshalCompact(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
