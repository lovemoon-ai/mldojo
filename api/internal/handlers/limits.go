package handlers

import (
	"net/http"
)

// Concurrency limits. They are admin-only: a limit anyone can raise is not a
// limit, and lowering someone else's is a way to stall their work.

func (s *Server) setNodeLimit(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		MaxRuns *int `json:"max_runs"`
	}
	if err := readJSON(r, &req); err != nil {
		return err
	}
	if req.MaxRuns == nil {
		return badRequest("max_runs is required (0 means unlimited)")
	}
	if *req.MaxRuns < 0 {
		return badRequest("max_runs cannot be negative (0 means unlimited)")
	}
	n, err := s.store().SetNodeMaxRuns(r.Context(), r.PathValue("id"), *req.MaxRuns)
	if err != nil {
		return err
	}
	return ok(w, n)
}

func (s *Server) setProjectLimit(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		MaxConcurrentRuns *int `json:"max_concurrent_runs"`
	}
	if err := readJSON(r, &req); err != nil {
		return err
	}
	if req.MaxConcurrentRuns == nil {
		return badRequest("max_concurrent_runs is required (0 means unlimited)")
	}
	if *req.MaxConcurrentRuns < 0 {
		return badRequest("max_concurrent_runs cannot be negative (0 means unlimited)")
	}
	p, err := s.store().SetProjectMaxRuns(r.Context(), r.PathValue("name"), *req.MaxConcurrentRuns)
	if err != nil {
		return err
	}
	return ok(w, p)
}
