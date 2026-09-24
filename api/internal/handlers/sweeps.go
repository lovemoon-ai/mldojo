package handlers

import (
	"net/http"
	"strconv"

	"github.com/lovemoon-ai/mldojo/api/internal/models"
	"github.com/lovemoon-ai/mldojo/recipes"
)

func (s *Server) createSweep(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		YAML string `json:"yaml"`
	}
	if err := readJSON(r, &req); err != nil {
		return err
	}
	sw, err := recipes.ParseSweep([]byte(req.YAML))
	if err != nil {
		return badRequest("%v", err)
	}
	created, err := s.Svc.CreateSweep(r.Context(), sw)
	if err != nil {
		return err
	}
	return writeJSON(w, 201, created)
}

func (s *Server) listSweeps(w http.ResponseWriter, r *http.Request) error {
	sweeps, err := s.store().ListSweeps(r.Context(), r.URL.Query().Get("project"), r.URL.Query().Get("status"))
	if err != nil {
		return err
	}
	return ok(w, sweeps)
}

func (s *Server) getSweep(w http.ResponseWriter, r *http.Request) error {
	sw, err := s.store().GetSweep(r.Context(), r.PathValue("p"), r.PathValue("name"))
	if err != nil {
		return err
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	status, err := s.Svc.SweepStatusOf(r.Context(), sw, limit)
	if err != nil {
		return err
	}
	return ok(w, status)
}

// stopSweep halts further launches. Runs already going are left alone: a
// sweep that is not producing anything useful is usually stopped precisely
// so the in-flight results can still be inspected.
func (s *Server) stopSweep(w http.ResponseWriter, r *http.Request) error {
	sw, err := s.store().GetSweep(r.Context(), r.PathValue("p"), r.PathValue("name"))
	if err != nil {
		return err
	}
	if err := mayDelete(r.Context(), "sweep", sw.Owner); err != nil {
		return err
	}
	if err := s.store().SetSweepStatus(r.Context(), sw.ID, models.SweepStopped); err != nil {
		return err
	}
	return ok(w, map[string]any{"stopped": sw.ID})
}
