package handlers

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/lovemoon-ai/mldojo/api/internal/auth"
	"github.com/lovemoon-ai/mldojo/api/internal/models"
	"github.com/lovemoon-ai/mldojo/recipes"
)

func (s *Server) listModels(w http.ResponseWriter, r *http.Request) error {
	ms, err := s.store().ListModels(r.Context(), r.URL.Query().Get("project"))
	if err != nil {
		return err
	}
	return ok(w, ms)
}

// getModel returns a model with its versions, newest first.
func (s *Server) getModel(w http.ResponseWriter, r *http.Request) error {
	m, err := s.store().GetModel(r.Context(), r.PathValue("p"), r.PathValue("name"))
	if err != nil {
		return err
	}
	versions, err := s.store().ListModelVersions(r.Context(), m.ID)
	if err != nil {
		return err
	}
	used, err := s.store().RunsUsingModel(r.Context(), m.Project+"/"+m.Name)
	if err != nil {
		return err
	}
	return ok(w, map[string]any{"model": m, "versions": versions, "used_by": used})
}

// registerModelVersion promotes an artifact of a run into the registry. The
// run id is the lineage: its code, params and environment are already
// recorded, so "where did this model come from" becomes answerable.
func (s *Server) registerModelVersion(w http.ResponseWriter, r *http.Request) error {
	project, name := r.PathValue("p"), r.PathValue("name")
	if !recipes.ValidName(name) {
		return badRequest("invalid model name %q", name)
	}
	var req struct {
		RunID string `json:"run_id"`
		URI   string `json:"uri"`
		Stage string `json:"stage"`
		Notes string `json:"notes"`
	}
	if err := readJSON(r, &req); err != nil {
		return err
	}
	if req.URI == "" {
		return badRequest("uri is required (the artifact to register; see `mldojo run artifacts ls`)")
	}
	if req.Stage != "" && !models.ValidStage(req.Stage) {
		return badRequest("unknown stage %q (none, staging, production, archived)", req.Stage)
	}
	v := &models.ModelVersion{
		URI: req.URI, Stage: req.Stage, Notes: req.Notes,
		CreatedBy: auth.PrincipalFrom(r.Context()).Actor(),
	}
	if req.RunID != "" {
		run, err := s.store().GetRun(r.Context(), req.RunID)
		if err != nil {
			return err
		}
		v.RunID = &run.ID
		// Carry over what is known about the artifact and how the run scored,
		// so the registry entry stands alone even if the run is deleted.
		arts, err := s.store().ListArtifacts(r.Context(), run.ID)
		if err != nil {
			return err
		}
		for _, a := range arts {
			if a.URI == req.URI {
				v.SHA256, v.SizeBytes = a.SHA256, a.SizeBytes
				break
			}
		}
		if latest, err := s.store().LatestMetrics(r.Context(), run.ID); err == nil && len(latest) > 0 {
			flat := map[string]float64{}
			for k, p := range latest {
				flat[k] = p.Value
			}
			if b, err := json.Marshal(flat); err == nil {
				v.Metrics = b
			}
		}
	}
	m, err := s.store().UpsertModel(r.Context(), project, name, auth.PrincipalFrom(r.Context()).Actor())
	if err != nil {
		return err
	}
	created, err := s.store().AddModelVersion(r.Context(), m.ID, v)
	if err != nil {
		return err
	}
	return writeJSON(w, 201, created)
}

func (s *Server) setModelStage(w http.ResponseWriter, r *http.Request) error {
	m, err := s.store().GetModel(r.Context(), r.PathValue("p"), r.PathValue("name"))
	if err != nil {
		return err
	}
	version, err := strconv.Atoi(r.PathValue("v"))
	if err != nil {
		return badRequest("version must be a number, got %q", r.PathValue("v"))
	}
	var req struct {
		Stage string `json:"stage"`
	}
	if err := readJSON(r, &req); err != nil {
		return err
	}
	updated, err := s.store().SetModelStage(r.Context(), m.ID, version, req.Stage)
	if err != nil {
		return err
	}
	return ok(w, updated)
}

// runModels answers "what did this run produce" -- the other direction of
// the lineage link.
func (s *Server) runModels(w http.ResponseWriter, r *http.Request) error {
	run, err := s.run(r)
	if err != nil {
		return err
	}
	versions, err := s.store().ModelVersionsFromRun(r.Context(), run.ID)
	if err != nil {
		return err
	}
	return ok(w, versions)
}
