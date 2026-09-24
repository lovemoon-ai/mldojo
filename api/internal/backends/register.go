package backends

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/lovemoon-ai/mldojo/api/internal/models"
	v1 "github.com/lovemoon-ai/mldojo/proto/mldojo/v1"
)

// registerModel closes the lineage loop. A training run that names
// outputs.model registers its newest checkpoint as a version of that model
// the moment it succeeds, so the evaluation that follows can reference it
// rather than copy a path out of a terminal.
//
// The registry was built for this and went unused, because `model register`
// was one more step after a run that had already finished. Nothing that has
// to be remembered afterwards gets done.
func (rt *Runtime) registerModel(ctx context.Context, run *v1.Run) {
	o := run.Metadata.Outputs
	if o == nil || o.Model == "" {
		return
	}
	project, name := run.Project, o.Model
	if p, n, ok := strings.Cut(o.Model, "/"); ok {
		project, name = p, n
	}
	// A retried or re-reported run must not register twice.
	if prev, err := rt.Store.ModelVersionsFromRun(ctx, run.ID); err == nil && len(prev) > 0 {
		return
	}
	arts, err := rt.Store.ListArtifacts(ctx, run.ID)
	if err != nil {
		slog.Warn("auto-register: list artifacts", "run", run.ID, "err", err)
		return
	}
	v, step := newestCheckpoint(arts)
	if v == nil {
		rt.SysLog(run.ID, "outputs.model %s: no checkpoints were collected, nothing to register", o.Model)
		return
	}
	if latest, err := rt.Store.LatestMetrics(ctx, run.ID); err == nil && len(latest) > 0 {
		flat := map[string]float64{}
		for k, p := range latest {
			flat[k] = p.Value
		}
		if b, err := json.Marshal(flat); err == nil {
			v.Metrics = b
		}
	}
	v.RunID, v.CreatedBy = &run.ID, run.Metadata.Submitter
	v.Notes = "registered automatically on success"
	if step > 0 {
		v.Notes += fmt.Sprintf(" at step %d", step)
	}
	m, err := rt.Store.UpsertModel(ctx, project, name, run.Metadata.Submitter)
	if err != nil {
		slog.Warn("auto-register: upsert model", "run", run.ID, "model", o.Model, "err", err)
		return
	}
	created, err := rt.Store.AddModelVersion(ctx, m.ID, v)
	if err != nil {
		slog.Warn("auto-register: add version", "run", run.ID, "model", o.Model, "err", err)
		return
	}
	rt.Store.AddEvent(ctx, run.ID, "model_registered",
		map[string]any{"model": project + "/" + name, "version": created.Version, "uri": created.URI})
	rt.SysLog(run.ID, "registered %s/%s@%d -> %s", project, name, created.Version, created.URI)
}

// newestCheckpoint picks the checkpoint to register and returns its step.
//
// A checkpoint is rarely one file: the lingbot run wrote 32 artifacts across
// four steps, eight shards each, and what an evaluation wants handed to it is
// the directory. So artifacts are grouped by the step in their path, the
// highest step wins, and a group of several files collapses to the directory
// they share.
func newestCheckpoint(arts []v1.Artifact) (*models.ModelVersion, int64) {
	best, bestStep := []v1.Artifact{}, int64(-1)
	for _, a := range arts {
		if a.Kind != "ckpt" {
			continue
		}
		p := a.URI
		if _, rel, ok := ParseNodeURI(a.URI); ok {
			p = rel
		}
		step := checkpointStep(p)
		switch {
		case step > bestStep:
			best, bestStep = []v1.Artifact{a}, step
		case step == bestStep:
			best = append(best, a)
		}
	}
	if len(best) == 0 {
		return nil, 0
	}
	v := &models.ModelVersion{URI: best[0].URI, SHA256: best[0].SHA256, SizeBytes: best[0].SizeBytes}
	if len(best) > 1 {
		v.URI, v.SHA256 = commonDir(best), ""
		v.SizeBytes = 0
		for _, a := range best {
			v.SizeBytes += a.SizeBytes
		}
	}
	return v, bestStep
}

// commonDir is the deepest directory holding every artifact. It splits the
// raw URI rather than using path.Dir, which would fold node://n/x into
// node:/n.
func commonDir(arts []v1.Artifact) string {
	parts := strings.Split(dirOf(arts[0].URI), "/")
	for _, a := range arts[1:] {
		other := strings.Split(dirOf(a.URI), "/")
		n := min(len(parts), len(other))
		for i := 0; i < n; i++ {
			if parts[i] != other[i] {
				n = i
				break
			}
		}
		parts = parts[:n]
	}
	return strings.Join(parts, "/")
}

func dirOf(uri string) string {
	if i := strings.LastIndex(uri, "/"); i > 0 {
		return uri[:i]
	}
	return uri
}
