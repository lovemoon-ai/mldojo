package handlers

import (
	"context"
	"encoding/base64"
	"io"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lovemoon-ai/mldojo/adapters/queue_sidecar"
	"github.com/lovemoon-ai/mldojo/api/internal/ai"
	"github.com/lovemoon-ai/mldojo/api/internal/auth"
	"github.com/lovemoon-ai/mldojo/api/internal/backends"
	"github.com/lovemoon-ai/mldojo/api/internal/core"
	"github.com/lovemoon-ai/mldojo/api/internal/logstore"
	"github.com/lovemoon-ai/mldojo/api/internal/models"
	"github.com/lovemoon-ai/mldojo/api/internal/secrets"
	"github.com/lovemoon-ai/mldojo/datasets"
	v1 "github.com/lovemoon-ai/mldojo/proto/mldojo/v1"
	"github.com/lovemoon-ai/mldojo/recipes"
)

func (s *Server) store() *models.Store { return s.RT.Store }

func force(r *http.Request) bool {
	f := r.URL.Query().Get("force")
	return f == "1" || f == "true"
}

// ---- projects / experiments -------------------------------------------------

func (s *Server) listProjects(w http.ResponseWriter, r *http.Request) error {
	ps, err := s.store().ListProjects(r.Context())
	if err != nil {
		return err
	}
	return ok(w, ps)
}

func (s *Server) createProject(w http.ResponseWriter, r *http.Request) error {
	var req struct{ Name, Description, Owner string }
	if err := readJSON(r, &req); err != nil {
		return err
	}
	if !recipes.ValidName(req.Name) {
		return badRequest("invalid project name %q (letters, digits, '.', '_', '-')", req.Name)
	}
	// Ownership is who you are. Only an admin may create a project on behalf
	// of someone else; otherwise the field is just a claim by the caller.
	owner := auth.PrincipalFrom(r.Context()).Actor()
	if req.Owner != "" && auth.PrincipalFrom(r.Context()).IsAdmin() {
		owner = req.Owner
	}
	p, err := s.store().CreateProject(r.Context(), req.Name, req.Description, owner)
	if err != nil {
		return err
	}
	return writeJSON(w, 201, p)
}

func (s *Server) getProject(w http.ResponseWriter, r *http.Request) error {
	p, err := s.store().GetProject(r.Context(), r.PathValue("name"))
	if err != nil {
		return err
	}
	return ok(w, p)
}

func (s *Server) deleteProject(w http.ResponseWriter, r *http.Request) error {
	p, err := s.store().GetProject(r.Context(), r.PathValue("name"))
	if err != nil {
		return err
	}
	if err := mayDelete(r.Context(), "project", p.Owner); err != nil {
		return err
	}
	if err := s.store().DeleteProject(r.Context(), r.PathValue("name"), force(r)); err != nil {
		return err
	}
	return ok(w, map[string]bool{"ok": true})
}

func (s *Server) listExperiments(w http.ResponseWriter, r *http.Request) error {
	es, err := s.store().ListExperiments(r.Context(), r.PathValue("p"))
	if err != nil {
		return err
	}
	return ok(w, es)
}

func (s *Server) createExperiment(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		Name        string   `json:"name"`
		Description string   `json:"description"`
		RecipeYAML  string   `json:"recipe_yaml"`
		Tags        []string `json:"tags"`
	}
	if err := readJSON(r, &req); err != nil {
		return err
	}
	if req.RecipeYAML != "" {
		rc, err := recipes.Parse([]byte(req.RecipeYAML))
		if err != nil {
			return badRequest("%v", err)
		}
		if req.Name == "" {
			req.Name = rc.Metadata.Name
		}
		if len(req.Tags) == 0 {
			req.Tags = rc.Metadata.Tags
		}
		if req.Description == "" {
			req.Description = rc.Metadata.Description
		}
	}
	if !recipes.ValidName(req.Name) {
		return badRequest("invalid experiment name %q", req.Name)
	}
	e, err := s.store().CreateExperiment(r.Context(), r.PathValue("p"), req.Name, req.Description, req.RecipeYAML, req.Tags)
	if err != nil {
		return err
	}
	return writeJSON(w, 201, e)
}

func (s *Server) getExperiment(w http.ResponseWriter, r *http.Request) error {
	e, err := s.store().GetExperiment(r.Context(), r.PathValue("p"), r.PathValue("e"))
	if err != nil {
		return err
	}
	return ok(w, e)
}

func (s *Server) deleteExperiment(w http.ResponseWriter, r *http.Request) error {
	// Experiments have no owner of their own; they belong to their project.
	p, err := s.store().GetProject(r.Context(), r.PathValue("p"))
	if err != nil {
		return err
	}
	if err := mayDelete(r.Context(), "experiment", p.Owner); err != nil {
		return err
	}
	if err := s.store().DeleteExperiment(r.Context(), r.PathValue("p"), r.PathValue("e"), force(r)); err != nil {
		return err
	}
	return ok(w, map[string]bool{"ok": true})
}

// ---- runs -------------------------------------------------------------------

func (s *Server) listRuns(w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))
	f := models.RunFilter{
		Project: q.Get("project"), Experiment: q.Get("experiment"),
		Status: q.Get("status"), Target: q.Get("target"),
		BackendKind: q.Get("backend_kind"), BackendID: q.Get("backend"),
		Tag: q.Get("tag"), Search: q.Get("q"),
		Sort: q.Get("sort"), Asc: q.Get("order") == "asc",
		Limit: limit, Offset: offset,
	}
	runs, err := s.store().ListRuns(r.Context(), f)
	if err != nil {
		return err
	}
	// The total goes in a header so the response stays a plain array for
	// every existing client; the web app uses it to page.
	if total, err := s.store().CountRuns(r.Context(), f); err == nil {
		w.Header().Set("X-Total-Count", strconv.Itoa(total))
	}
	return ok(w, runs)
}

func (s *Server) submitRun(w http.ResponseWriter, r *http.Request) error {
	var req v1.SubmitRequest
	if err := readJSON(r, &req); err != nil {
		return err
	}
	resp, err := s.Svc.Submit(r.Context(), req)
	if err != nil {
		return err
	}
	return writeJSON(w, 201, resp)
}

func (s *Server) run(r *http.Request) (*v1.Run, error) {
	return s.store().GetRun(r.Context(), r.PathValue("id"))
}

func (s *Server) getRun(w http.ResponseWriter, r *http.Request) error {
	run, err := s.run(r)
	if err != nil {
		return err
	}
	return ok(w, run)
}

func (s *Server) deleteRun(w http.ResponseWriter, r *http.Request) error {
	force := r.URL.Query().Get("force") == "1" || r.URL.Query().Get("force") == "true"
	purge := r.URL.Query().Get("purge_node") == "1" || r.URL.Query().Get("purge_node") == "true"
	// Read the run before deleting it: afterwards there is nothing left to
	// say which node was holding its outputs.
	var run *v1.Run
	if purge {
		var err error
		if run, err = s.store().GetRun(r.Context(), r.PathValue("id")); err != nil {
			return err
		}
	}
	id, err := s.Svc.Delete(r.Context(), r.PathValue("id"), force)
	if err != nil {
		return err
	}
	res := map[string]any{"deleted": id}
	if purge && run != nil {
		if err := s.Node.PurgeRunDir(r.Context(), run); err != nil {
			// The row is already gone; report the node half honestly
			// rather than failing the whole call.
			res["purge_error"] = err.Error()
		} else {
			res["purged_node"] = run.BackendID
		}
	}
	return ok(w, res)
}

// rerunRun re-submits a past run, optionally on a different target or with
// different params.
// compareMany backs the multi-run view: params, final metrics and the curves
// themselves, in one request.
func (s *Server) compareMany(w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()
	ids := splitCSV(q.Get("runs"))
	keys := splitCSV(q.Get("keys"))
	maxPts, _ := strconv.Atoi(q.Get("max_points"))
	res, err := s.Svc.CompareMany(r.Context(), ids, keys, maxPts)
	if err != nil {
		return err
	}
	return ok(w, res)
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func (s *Server) rerunRun(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		Target string         `json:"target"`
		Params map[string]any `json:"params"`
	}
	if err := readJSON(r, &req); err != nil {
		return err
	}
	res, err := s.Svc.Rerun(r.Context(), r.PathValue("id"), req.Target, req.Params)
	if err != nil {
		return err
	}
	return writeJSON(w, 201, res)
}

func (s *Server) cancelRun(w http.ResponseWriter, r *http.Request) error {
	run, err := s.Svc.Cancel(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	return ok(w, run)
}

func (s *Server) runEvents(w http.ResponseWriter, r *http.Request) error {
	run, err := s.run(r)
	if err != nil {
		return err
	}
	ev, err := s.store().ListEvents(r.Context(), run.ID)
	if err != nil {
		return err
	}
	return ok(w, ev)
}

func validStream(st string) bool {
	return st == v1.StreamStdout || st == v1.StreamStderr || st == v1.StreamSystem
}

func (s *Server) runLogs(w http.ResponseWriter, r *http.Request) error {
	run, err := s.run(r)
	if err != nil {
		return err
	}
	q := r.URL.Query()
	stream := q.Get("stream")
	if stream == "" {
		stream = v1.StreamStdout
	}
	if !validStream(stream) {
		return badRequest("stream must be stdout|stderr|system")
	}
	off, _ := strconv.ParseInt(q.Get("offset"), 10, 64)
	limit, _ := strconv.ParseInt(q.Get("limit"), 10, 64)
	if limit <= 0 || limit > 64<<20 {
		limit = 4 << 20
	}
	if tail, _ := strconv.ParseInt(q.Get("tail"), 10, 64); tail > 0 {
		off = s.RT.Logs.Size(run.ID, stream) - tail
	}
	data, size, err := s.RT.Logs.ReadAt(run.ID, stream, off, limit)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Log-Size", strconv.FormatInt(size, 10))
	w.Write(data)
	return nil
}

const (
	// totalMetricBudget bounds an unfiltered metrics response. ~40k points
	// is a couple of megabytes of JSON, which a browser parses without
	// noticing.
	totalMetricBudget = 40000
	// minPointsPerKey keeps a curve readable even on a run with hundreds of
	// keys; the caller asks for one by name when it wants the full thing.
	minPointsPerKey = 120
)

func (s *Server) runMetrics(w http.ResponseWriter, r *http.Request) error {
	run, err := s.run(r)
	if err != nil {
		return err
	}
	since, _ := strconv.ParseInt(strings.TrimPrefix(r.URL.Query().Get("since_step"), "step:"), 10, 64)
	maxPts, _ := strconv.Atoi(r.URL.Query().Get("max_points"))
	keys, err := s.store().MetricKeys(r.Context(), run.ID)
	if err != nil {
		return err
	}
	// A caller that only needs to know what was logged should not have to
	// download it. A real run logs 335 keys; the key list is a few KB and
	// the data behind it is tens of megabytes.
	if r.URL.Query().Get("keys_only") == "1" {
		return ok(w, map[string]any{"keys": keys, "points": []v1.MetricPoint{}, "sampled": false})
	}
	key := r.URL.Query().Get("key")
	if maxPts <= 0 && key == "" {
		// The per-key budget says nothing about the size of the response
		// when a run has hundreds of keys: 335 x 2000 points is ~75 MB, and
		// asking for all of them was enough to hang a browser. Spread a
		// total budget across the keys instead, and let a caller that wants
		// a specific curve ask for it by name.
		maxPts = totalMetricBudget / max(len(keys), 1)
		if maxPts < minPointsPerKey {
			maxPts = minPointsPerKey
		}
	}
	pts, sampled, err := s.store().ListMetrics(r.Context(), run.ID, key, since, maxPts)
	if err != nil {
		return err
	}
	return ok(w, map[string]any{"keys": keys, "points": pts, "sampled": sampled})
}

// startExternal backs mldojo.init(): a run for a process already running.
func (s *Server) startExternal(w http.ResponseWriter, r *http.Request) error {
	var req core.ExternalRun
	if err := readJSON(r, &req); err != nil {
		return err
	}
	run, token, err := s.Svc.StartExternal(r.Context(), req)
	if err != nil {
		return err
	}
	return writeJSON(w, 201, map[string]any{"run": run, "token": token})
}

// finishExternal closes a run the SDK started. Like metric ingest it accepts
// the per-run token, so a training script needs no API credentials beyond
// what init() handed it.
func (s *Server) finishExternal(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("id")
	if !s.tokenOK(r) && !s.RT.CheckRunToken(id, bearer(r)) {
		return httpErr{401, "unauthorized", "invalid token"}
	}
	var req struct {
		Status   string `json:"status"`
		ExitCode *int   `json:"exit_code"`
	}
	if err := readJSON(r, &req); err != nil {
		return err
	}
	if req.Status == "" {
		req.Status = v1.PhaseSucceeded
	}
	run, err := s.Svc.FinishExternal(r.Context(), id, req.Status, req.ExitCode)
	if err != nil {
		return err
	}
	return ok(w, run)
}

// putRunConfig records hyperparameters reported by the SDK, for projects that
// define them in code (argparse, hydra) rather than in a recipe.
func (s *Server) putRunConfig(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("id")
	if !s.tokenOK(r) && !s.RT.CheckRunToken(id, bearer(r)) {
		return httpErr{401, "unauthorized", "invalid token"}
	}
	var cfg map[string]any
	if err := readJSON(r, &cfg); err != nil {
		return err
	}
	run, err := s.store().GetRun(r.Context(), id)
	if err != nil {
		return err
	}
	merged := map[string]any{}
	for k, v := range run.Metadata.Params {
		merged[k] = v
	}
	for k, v := range cfg {
		merged[k] = v
	}
	if err := s.store().MergeRunMetadata(r.Context(), run.ID, map[string]any{"params": merged}); err != nil {
		return err
	}
	return ok(w, map[string]any{"params": merged})
}

// ingestMetrics accepts the API token or the per-run token (SDK / wandb shim).
func (s *Server) ingestMetrics(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("id")
	if !s.tokenOK(r) && !s.RT.CheckRunToken(id, bearer(r)) {
		return httpErr{401, "unauthorized", "invalid token"}
	}
	run, err := s.store().GetRun(r.Context(), id)
	if err != nil {
		return err
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		return err
	}
	var pts []v1.MetricPoint
	trim := strings.TrimSpace(string(body))
	if strings.HasPrefix(trim, "[") {
		err = jsonUnmarshal(body, &pts)
	} else {
		var wrapper struct {
			Points []v1.MetricPoint `json:"points"`
		}
		err = jsonUnmarshal(body, &wrapper)
		pts = wrapper.Points
	}
	if err != nil {
		return badRequest("invalid metrics body: %v", err)
	}
	for _, p := range pts {
		if p.Key == "" {
			return badRequest("metric key is required")
		}
	}
	if err := s.store().UpsertMetrics(r.Context(), run.ID, pts); err != nil {
		return err
	}
	s.RT.Logs.Broker.Publish(logstore.MetricTopic(run.ID), pts)
	return ok(w, map[string]any{"ok": true, "count": len(pts)})
}

func (s *Server) listArtifacts(w http.ResponseWriter, r *http.Request) error {
	run, err := s.run(r)
	if err != nil {
		return err
	}
	arts, err := s.store().ListArtifacts(r.Context(), run.ID)
	if err != nil {
		return err
	}
	return ok(w, arts)
}

func (s *Server) refreshArtifacts(w http.ResponseWriter, r *http.Request) error {
	run, err := s.run(r)
	if err != nil {
		return err
	}
	be, err := s.Svc.Backends.ForRun(run)
	if err != nil {
		return err
	}
	arts, err := be.ArtifactsList(r.Context(), run)
	if err != nil {
		return err
	}
	return ok(w, arts)
}

func (s *Server) rawArtifact(w http.ResponseWriter, r *http.Request) error {
	run, err := s.run(r)
	if err != nil {
		return err
	}
	uri := r.URL.Query().Get("uri")
	if uri == "" {
		return badRequest("uri is required")
	}
	be, err := s.Svc.Backends.ForRun(run)
	if err != nil {
		return err
	}
	p, err := be.ArtifactsFetch(r.Context(), run, uri)
	if err != nil {
		return err
	}
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	st, _ := f.Stat()
	name := path.Base(uri)
	if ct := mime.TypeByExtension(filepath.Ext(name)); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	if r.URL.Query().Get("download") == "1" {
		w.Header().Set("Content-Disposition", `attachment; filename="`+strings.ReplaceAll(name, `"`, "")+`"`)
	}
	http.ServeContent(w, r, name, st.ModTime(), f)
	return nil
}

func (s *Server) runCode(w http.ResponseWriter, r *http.Request) error {
	run, err := s.run(r)
	if err != nil {
		return err
	}
	ci, err := s.Svc.Code(r.Context(), run)
	if err != nil {
		return err
	}
	return ok(w, ci)
}

// runGPU reports the cards a run is on. For a finished run the live stats
// are worse than useless -- the indices it once held may belong to somebody
// else by now -- so it answers from the samples taken while it ran.
func (s *Server) runGPU(w http.ResponseWriter, r *http.Request) error {
	run, err := s.run(r)
	if err != nil {
		return err
	}
	if v1.Terminal(run.Status) || r.URL.Query().Get("history") == "1" {
		h, err := s.store().GPUHistory(r.Context(), models.GPUHistoryQuery{
			RunID: run.ID, Since: startOf(run), Until: endOf(run), MaxPoints: queryInt(r, "max_points", 500),
		})
		if err != nil {
			return err
		}
		return ok(w, map[string]any{"run_id": run.ID, "history": h, "live": []v1.GPUStat{}})
	}
	be, err := s.Svc.Backends.ForRun(run)
	if err != nil {
		return err
	}
	g, err := be.GPUStats(r.Context(), run)
	if err != nil {
		return err
	}
	if g == nil {
		g = []v1.GPUStat{}
	}
	h, _ := s.store().GPUHistory(r.Context(), models.GPUHistoryQuery{
		RunID: run.ID, Since: startOf(run), MaxPoints: queryInt(r, "max_points", 500),
	})
	return ok(w, map[string]any{"run_id": run.ID, "live": g, "history": h})
}

func startOf(run *v1.Run) time.Time {
	if run.StartedAt != nil {
		return run.StartedAt.Add(-time.Minute)
	}
	return run.CreatedAt
}

func endOf(run *v1.Run) time.Time {
	if run.FinishedAt != nil {
		return run.FinishedAt.Add(time.Minute)
	}
	return time.Now()
}

func queryInt(r *http.Request, key string, def int) int {
	if v := r.URL.Query().Get(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// nodeGPUHistory answers "what was this machine doing yesterday", which
// until now nothing could: the readings lived in a map and were deleted the
// moment the agent disconnected.
func (s *Server) nodeGPUHistory(w http.ResponseWriter, r *http.Request) error {
	if _, err := s.store().GetNode(r.Context(), r.PathValue("id")); err != nil {
		return err
	}
	since, until, err := window(r)
	if err != nil {
		return err
	}
	h, err := s.store().GPUHistory(r.Context(), models.GPUHistoryQuery{
		NodeID: r.PathValue("id"), Since: since, Until: until, MaxPoints: queryInt(r, "max_points", 500),
	})
	if err != nil {
		return err
	}
	return ok(w, h)
}

// nodeDisk reports what MLDojo is holding on a node, per run.
func (s *Server) nodeDisk(w http.ResponseWriter, r *http.Request) error {
	if _, err := s.store().GetNode(r.Context(), r.PathValue("id")); err != nil {
		return err
	}
	du, err := s.Node.DiskUsage(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	// Name the runs: a list of uuids and sizes is not an answer to "what is
	// filling this disk".
	ids := make([]string, 0, len(du.Entries))
	for _, e := range du.Entries {
		ids = append(ids, e.RunID)
	}
	names := map[string]any{}
	for _, id := range ids {
		if run, err := s.store().GetRun(r.Context(), id); err == nil {
			names[id] = map[string]any{"project": run.Project, "experiment": run.Experiment,
				"name": run.Name, "status": run.Status}
		}
	}
	return ok(w, map[string]any{"node": r.PathValue("id"), "usage": du, "runs": names})
}

// usage totals GPU hours, and how much of that time the cards were working.
func (s *Server) usage(w http.ResponseWriter, r *http.Request) error {
	by := r.URL.Query().Get("by")
	if by == "" {
		by = "project"
	}
	since, until, err := window(r)
	if err != nil {
		return err
	}
	u, err := s.store().Usage(r.Context(), by, since, until)
	if err != nil {
		return err
	}
	if u == nil {
		u = []models.UsageRow{}
	}
	return ok(w, map[string]any{"by": by, "since": since, "until": until, "rows": u})
}

// idleGPUs lists cards a run held without using them.
func (s *Server) idleGPUs(w http.ResponseWriter, r *http.Request) error {
	since, _, err := window(r)
	if err != nil {
		return err
	}
	minHours := 0.5
	if v := r.URL.Query().Get("min_hours"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f < 0 {
			return badRequest("min_hours must be a non-negative number")
		}
		minHours = f
	}
	g, err := s.store().IdleGPUs(r.Context(), since, minHours)
	if err != nil {
		return err
	}
	return ok(w, g)
}

// window parses ?since=&until=, accepting either RFC3339 or a duration ago
// ("24h"), because both are natural and neither is guessable.
func window(r *http.Request) (since, until time.Time, err error) {
	parse := func(key string) (time.Time, error) {
		v := r.URL.Query().Get(key)
		if v == "" {
			return time.Time{}, nil
		}
		if d, err := time.ParseDuration(v); err == nil {
			return time.Now().Add(-d), nil
		}
		// Days are what people type for a window, and the CLI's own default
		// is "7d" -- which Go durations do not accept, so `mldojo usage`
		// with no flags at all used to fail.
		if n, ok := strings.CutSuffix(v, "d"); ok {
			if days, err := strconv.ParseFloat(n, 64); err == nil && days >= 0 {
				return time.Now().Add(-time.Duration(days * 24 * float64(time.Hour))), nil
			}
		}
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return time.Time{}, badRequest("%s must be RFC3339 or a duration like 24h or 7d", key)
		}
		return t, nil
	}
	if since, err = parse("since"); err != nil {
		return
	}
	until, err = parse("until")
	return
}

// runEpisodes returns a run's per-trial results. For a policy this is the
// result: the rate is a summary of it, not the other way round.
func (s *Server) runEpisodes(w http.ResponseWriter, r *http.Request) error {
	run, err := s.run(r)
	if err != nil {
		return err
	}
	eps, err := s.store().ListEpisodes(r.Context(), run.ID)
	if err != nil {
		return err
	}
	// Summarise before filtering: the summary describes the evaluation, not
	// the view of it. "0/15 succeeded" is a confusing way to say "here are
	// the 15 failures out of 20".
	sum := v1.Summarize(eps)
	if only := r.URL.Query().Get("result"); only != "" {
		want := only == "success"
		var kept []v1.Episode
		for _, e := range eps {
			if e.Success == want {
				kept = append(kept, e)
			}
		}
		eps = kept
	}
	if eps == nil {
		eps = []v1.Episode{}
	}
	return ok(w, map[string]any{"run_id": run.ID, "summary": sum, "episodes": eps})
}

// compareEpisodes lines two evaluations up trial by trial. Two runs of the
// same checkpoint on the same seeds scored 6/20 and 5/20; the rate cannot
// tell you which trials flipped, and that is the only interesting part.
func (s *Server) compareEpisodes(w http.ResponseWriter, r *http.Request) error {
	a, b := r.URL.Query().Get("a"), r.URL.Query().Get("b")
	if a == "" || b == "" {
		return badRequest("a and b are required")
	}
	ra, err := s.store().GetRun(r.Context(), a)
	if err != nil {
		return err
	}
	rb, err := s.store().GetRun(r.Context(), b)
	if err != nil {
		return err
	}
	rows, err := s.store().CompareEpisodes(r.Context(), ra.ID, rb.ID)
	if err != nil {
		return err
	}
	flipped := 0
	for _, c := range rows {
		if c.A != nil && c.B != nil && *c.A != *c.B {
			flipped++
		}
	}
	return ok(w, map[string]any{"a": ra.ID, "b": rb.ID, "flipped": flipped, "episodes": rows})
}

func (s *Server) compare(w http.ResponseWriter, r *http.Request) error {
	a, b := r.URL.Query().Get("a"), r.URL.Query().Get("b")
	if a == "" || b == "" {
		return badRequest("a and b are required")
	}
	c, err := s.Svc.Compare(r.Context(), a, b)
	if err != nil {
		return err
	}
	return ok(w, c)
}

// ---- nodes ------------------------------------------------------------------

func (s *Server) withGPU(n *v1.Node) {
	n.GPUStats = s.Node.Hub.GPU(n.ID)
	n.Disks = s.Node.Hub.Disks(n.ID)
	if s.Node.Hub.Conn(n.ID) == nil && n.AgentStatus == "online" {
		n.AgentStatus = "offline"
	}
	// Never expose credentials material (they are refs anyway, but be strict).
	if n.Connection.Password != "" && !secrets.IsRef(n.Connection.Password) {
		n.Connection.Password = "***"
	}
}

func (s *Server) listNodes(w http.ResponseWriter, r *http.Request) error {
	ns, err := s.store().ListNodes(r.Context())
	if err != nil {
		return err
	}
	for i := range ns {
		s.withGPU(&ns[i])
	}
	return ok(w, ns)
}

func (s *Server) addNode(w http.ResponseWriter, r *http.Request) error {
	var req backends.NodeAddRequest
	if err := readJSON(r, &req); err != nil {
		return err
	}
	if req.DryRun {
		res, err := s.Node.ProbeNode(r.Context(), req)
		if err != nil {
			return err
		}
		return ok(w, res)
	}
	n, err := s.Node.AddNode(r.Context(), req)
	if err != nil {
		return err
	}
	s.withGPU(n)
	return writeJSON(w, 201, n)
}

func (s *Server) getNode(w http.ResponseWriter, r *http.Request) error {
	n, err := s.store().GetNode(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	s.withGPU(n)
	return ok(w, n)
}

func (s *Server) deleteNode(w http.ResponseWriter, r *http.Request) error {
	stop := r.URL.Query().Get("stop_agent") != "0"
	if err := s.Node.RemoveNode(r.Context(), r.PathValue("id"), stop, force(r)); err != nil {
		return err
	}
	return ok(w, map[string]bool{"ok": true})
}

func (s *Server) testNode(w http.ResponseWriter, r *http.Request) error {
	res, err := s.Node.TestNode(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	return ok(w, res)
}

func (s *Server) upgradeNode(w http.ResponseWriter, r *http.Request) error {
	n, err := s.Node.UpgradeNode(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	s.withGPU(n)
	return ok(w, n)
}

func (s *Server) nodeGPU(w http.ResponseWriter, r *http.Request) error {
	if _, err := s.store().GetNode(r.Context(), r.PathValue("id")); err != nil {
		return err
	}
	g := s.Node.Hub.GPU(r.PathValue("id"))
	if g == nil {
		g = []v1.GPUStat{}
	}
	return ok(w, g)
}

// ---- queues -----------------------------------------------------------------

var queueIDRe = regexp.MustCompile(`^[a-z0-9_-]+/[A-Za-z0-9._-]+$`)

func (s *Server) listQueues(w http.ResponseWriter, r *http.Request) error {
	qs, err := s.store().ListQueues(r.Context())
	if err != nil {
		return err
	}
	return ok(w, qs)
}

type queueResource struct {
	Plugin  string `json:"plugin"`
	QueueID string `json:"queue_id,omitempty"` // set when registered as <plugin>/<name>
	queue_sidecar.QueueResource
}

// queueResources reports live scheduler capacity from every queue plugin that supports it.
func (s *Server) queueResources(w http.ResponseWriter, r *http.Request) error {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	registered := map[string]bool{}
	if qs, err := s.store().ListQueues(ctx); err == nil {
		for _, q := range qs {
			registered[q.ID] = true
		}
	}
	out := struct {
		Queues   []queueResource   `json:"queues"`
		Errors   map[string]string `json:"errors,omitempty"`
		Warnings []string          `json:"warnings,omitempty"`
	}{Queues: []queueResource{}, Errors: map[string]string{}}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for name, be := range s.Queues {
		wg.Add(1)
		go func() {
			defer wg.Done()
			qs, warns, err := be.Resources(ctx)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				out.Errors[name] = err.Error()
			}
			for _, wn := range warns {
				out.Warnings = append(out.Warnings, name+": "+wn)
			}
			for _, q := range qs {
				qr := queueResource{Plugin: name, QueueResource: q}
				if id := name + "/" + q.Name; registered[id] {
					qr.QueueID = id
				}
				out.Queues = append(out.Queues, qr)
			}
		}()
	}
	wg.Wait()
	return ok(w, out)
}

func (s *Server) addQueue(w http.ResponseWriter, r *http.Request) error {
	var q v1.Queue
	if err := readJSON(r, &q); err != nil {
		return err
	}
	if !queueIDRe.MatchString(q.ID) {
		return badRequest("queue id must look like <plugin>/<queue>, e.g. myqueue/gpu-a100")
	}
	be, _, _ := strings.Cut(q.ID, "/")
	if q.Backend == "" {
		q.Backend = be
	}
	if q.Backend != be {
		return badRequest("queue backend %q does not match the id prefix %q", q.Backend, be)
	}
	if _, ok := s.Queues[q.Backend]; !ok {
		return badRequest("unknown queue plugin %q (configure it under api.queue_plugins)", q.Backend)
	}
	for _, ref := range []string{q.Client.Credentials, q.Client.JobPassword} {
		if ref != "" && !secrets.IsRef(ref) {
			return badRequest("credentials and job_password must be secret:// references")
		}
	}
	if q.DisplayName == "" {
		q.DisplayName = q.ID
	}
	if err := s.store().InsertQueue(r.Context(), &q); err != nil {
		return err
	}
	out, err := s.store().GetQueue(r.Context(), q.ID)
	if err != nil {
		return err
	}
	return writeJSON(w, 201, out)
}

func (s *Server) getQueue(w http.ResponseWriter, r *http.Request) error {
	q, err := s.store().GetQueue(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	return ok(w, q)
}

func (s *Server) deleteQueue(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("id")
	q, err := s.store().GetQueue(r.Context(), id)
	if err != nil {
		return err
	}
	if q.ActiveRuns > 0 && !force(r) {
		return models.Conflict("queue %s has %d active runs", id, q.ActiveRuns)
	}
	if err := s.store().DeleteQueue(r.Context(), id); err != nil {
		return err
	}
	return ok(w, map[string]bool{"ok": true})
}

// ---- datasets -----------------------------------------------------------------

func (s *Server) listDatasets(w http.ResponseWriter, r *http.Request) error {
	ds, err := s.store().ListDatasets(r.Context())
	if err != nil {
		return err
	}
	return ok(w, ds)
}

func (s *Server) registerDataset(w http.ResponseWriter, r *http.Request) error {
	var d v1.Dataset
	if err := readJSON(r, &d); err != nil {
		return err
	}
	if !recipes.ValidName(d.Name) {
		return badRequest("invalid dataset name %q", d.Name)
	}
	if d.Version == "" {
		d.Version = "v1"
	}
	if !recipes.ValidName(d.Version) {
		return badRequest("invalid dataset version %q", d.Version)
	}
	if d.Mount != "" && !strings.HasPrefix(d.Mount, "/") {
		return badRequest("mount must be an absolute path")
	}
	if len(d.Locations) == 0 {
		return badRequest("at least one --location is required")
	}
	auth := 0
	for _, l := range d.Locations {
		switch l.Kind {
		case "node_path":
			if l.Node == "" || l.Path == "" {
				return badRequest("node_path locations need node and path")
			}
			if _, err := s.store().GetNode(r.Context(), l.Node); err != nil {
				return badRequest("location node %q: %v", l.Node, err)
			}
		case "bucket":
			if l.Bucket == "" || l.Path == "" {
				return badRequest("bucket locations need bucket and path")
			}
			if l.Credentials != "" && !secrets.IsRef(l.Credentials) {
				return badRequest("bucket credentials must be a secret:// reference")
			}
		default:
			return badRequest("location kind must be node_path or bucket")
		}
		if l.Authoritative {
			auth++
		}
	}
	if auth > 1 {
		return badRequest("only one location can be authoritative")
	}
	out, err := s.store().CreateDataset(r.Context(), &d)
	if err != nil {
		return err
	}
	return writeJSON(w, 201, out)
}

func (s *Server) getDataset(w http.ResponseWriter, r *http.Request) error {
	name, ver := datasets.SplitRef(r.PathValue("ref"))
	d, err := s.store().GetDataset(r.Context(), name, ver)
	if err != nil {
		return err
	}
	return ok(w, d)
}

func (s *Server) deleteDataset(w http.ResponseWriter, r *http.Request) error {
	name, ver := datasets.SplitRef(r.PathValue("ref"))
	if err := s.store().DeleteDataset(r.Context(), name, ver); err != nil {
		return err
	}
	return ok(w, map[string]bool{"ok": true})
}

func (s *Server) pushDataset(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		Node string `json:"node"`
	}
	if err := readJSON(r, &req); err != nil {
		return err
	}
	req.Node = strings.TrimPrefix(req.Node, "node:")
	if req.Node == "" {
		return badRequest("node is required")
	}
	name, ver := datasets.SplitRef(r.PathValue("ref"))
	p, err := s.Node.PushDataset(r.Context(), name, ver, req.Node)
	if err != nil {
		return err
	}
	return ok(w, map[string]string{"node": req.Node, "path": p})
}

// ---- secrets ------------------------------------------------------------------

func (s *Server) listSecrets(w http.ResponseWriter, r *http.Request) error {
	l, err := s.RT.Secrets.List(r.Context())
	if err != nil {
		return err
	}
	return ok(w, l)
}

func (s *Server) secretStatus(w http.ResponseWriter, r *http.Request) error {
	return ok(w, s.RT.Secrets.Status())
}

func (s *Server) unlockSecrets(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		Key string `json:"key"`
	}
	if err := readJSON(r, &req); err != nil {
		return err
	}
	if err := s.RT.Secrets.Unlock(r.Context(), req.Key); err != nil {
		return badRequest("%v", err)
	}
	return ok(w, s.RT.Secrets.Status())
}

func (s *Server) setSecret(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		ValueB64    string `json:"value_b64"`
		Description string `json:"description"`
	}
	if err := readJSON(r, &req); err != nil {
		return err
	}
	val, err := base64.StdEncoding.DecodeString(req.ValueB64)
	if err != nil {
		return badRequest("value_b64 is not valid base64")
	}
	if len(val) == 0 {
		return badRequest("empty secret value")
	}
	m, err := s.RT.Secrets.Set(r.Context(), r.PathValue("ns"), r.PathValue("name"), val, req.Description)
	if err != nil {
		if err == secrets.ErrLocked {
			return badRequest("%v", err)
		}
		return badRequest("%v", err)
	}
	return writeJSON(w, 201, m)
}

func (s *Server) deleteSecret(w http.ResponseWriter, r *http.Request) error {
	if err := s.RT.Secrets.Delete(r.Context(), r.PathValue("ns"), r.PathValue("name")); err != nil {
		return err
	}
	return ok(w, map[string]bool{"ok": true})
}

// ---- blobs --------------------------------------------------------------------

func (s *Server) putBlob(w http.ResponseWriter, r *http.Request) error {
	sha, n, err := s.RT.Logs.PutBlob(r.Body, 4<<30)
	if err != nil {
		return badRequest("%v", err)
	}
	return writeJSON(w, 201, v1.BlobRef{URI: "blob://" + sha, SHA256: sha, Size: n})
}

func (s *Server) serveBlob(w http.ResponseWriter, r *http.Request, sha string) {
	f, err := s.RT.Logs.OpenBlob(sha)
	if err != nil {
		writeErr(w, models.NotFound("blob %s", sha))
		return
	}
	defer f.Close()
	st, _ := f.Stat()
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, sha, st.ModTime(), f)
}

func (s *Server) getBlob(w http.ResponseWriter, r *http.Request) error {
	s.serveBlob(w, r, r.PathValue("sha"))
	return nil
}

func (s *Server) agentBlob(w http.ResponseWriter, r *http.Request, node string) {
	s.serveBlob(w, r, r.PathValue("sha"))
}

// ---- AI -----------------------------------------------------------------------

func withSummary(r *http.Request) bool {
	v := strings.ToLower(r.Header.Get("X-Include-Summary"))
	return v == "true" || v == "1" || r.URL.Query().Get("summary") == "1"
}

func (s *Server) aiBrief(w http.ResponseWriter, r *http.Request) error {
	b, err := s.AI.Brief(r.Context(), r.PathValue("id"), withSummary(r))
	if err != nil {
		return err
	}
	return ok(w, b)
}

func (s *Server) aiFreeNodes(w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()
	fq := ai.FreeQuery{}
	if v := q.Get("gpus"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return badRequest("gpus must be a non-negative integer")
		}
		fq.GPUs = n
	}
	if v := q.Get("min_free_gb"); v != "" {
		g, err := strconv.ParseFloat(v, 64)
		if err != nil || g < 0 {
			return badRequest("min_free_gb must be a non-negative number")
		}
		fq.MinFreeGB = g
	}
	if v := q.Get("labels"); v != "" {
		for _, l := range strings.Split(v, ",") {
			if l = strings.TrimSpace(l); l != "" {
				fq.Labels = append(fq.Labels, l)
			}
		}
	}
	f, err := s.AI.FreeNodes(r.Context(), fq)
	if err != nil {
		return err
	}
	return ok(w, f)
}

func (s *Server) aiSubmit(w http.ResponseWriter, r *http.Request) error {
	var req ai.LooseSubmit
	if err := readJSON(r, &req); err != nil {
		return err
	}
	resp, err := s.AI.Submit(r.Context(), req)
	if err != nil {
		return err
	}
	return writeJSON(w, 201, resp)
}

func (s *Server) aiExpSummary(w http.ResponseWriter, r *http.Request) error {
	sum, err := s.AI.ExperimentSummary(r.Context(), r.PathValue("p"), r.PathValue("e"), withSummary(r))
	if err != nil {
		return err
	}
	return ok(w, sum)
}

func (s *Server) aiAnomaly(w http.ResponseWriter, r *http.Request) error {
	res, err := s.AI.AnomalyCheck(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	return ok(w, res)
}

func (s *Server) listAudit(w http.ResponseWriter, r *http.Request) error {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	entries, err := s.store().ListAudit(r.Context(), models.AuditFilter{
		Actor:  r.URL.Query().Get("actor"),
		Action: r.URL.Query().Get("action"),
		Target: r.URL.Query().Get("target"),
		Limit:  limit,
	})
	if err != nil {
		return err
	}
	return ok(w, entries)
}

func (s *Server) listUsers(w http.ResponseWriter, r *http.Request) error {
	users, err := s.store().ListUsers(r.Context())
	if err != nil {
		return err
	}
	return ok(w, users)
}

func (s *Server) listTokens(w http.ResponseWriter, r *http.Request) error {
	ts, err := s.store().ListAPITokens(r.Context())
	if err != nil {
		return err
	}
	return ok(w, ts)
}

// createToken returns the plaintext once; only its hash is stored.
func (s *Server) createToken(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		Name     string `json:"name"`
		Role     string `json:"role"`
		ExpireIn string `json:"expire_in"` // Go duration, e.g. "720h"
	}
	if err := readJSON(r, &req); err != nil {
		return err
	}
	if req.Name == "" {
		return badRequest("a token needs a name, so it can be told apart later")
	}
	role := req.Role
	if role == "" {
		role = auth.RoleAdmin
	}
	if role != auth.RoleAdmin && role != auth.RoleMember {
		return badRequest("role must be %q or %q", auth.RoleAdmin, auth.RoleMember)
	}
	var ttl time.Duration
	if req.ExpireIn != "" {
		d, err := time.ParseDuration(req.ExpireIn)
		if err != nil || d <= 0 {
			return badRequest("expire_in must be a positive Go duration (e.g. 720h), got %q", req.ExpireIn)
		}
		ttl = d
	}
	plain, t, err := s.store().CreateAPIToken(r.Context(), req.Name, role,
		auth.PrincipalFrom(r.Context()).Actor(), ttl)
	if err != nil {
		return err
	}
	return writeJSON(w, 201, map[string]any{"token": plain, "meta": t,
		"note": "this is the only time the token is shown"})
}

func (s *Server) revokeToken(w http.ResponseWriter, r *http.Request) error {
	id, err := s.store().RevokeAPIToken(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	return ok(w, map[string]any{"revoked": id})
}
