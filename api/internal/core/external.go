package core

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/lovemoon-ai/mldojo/api/internal/auth"
	"github.com/lovemoon-ai/mldojo/api/internal/models"
	v1 "github.com/lovemoon-ai/mldojo/proto/mldojo/v1"
	"github.com/lovemoon-ai/mldojo/recipes"
)

// ExternalRun is what mldojo.init() asks for: somewhere to put the metrics of
// a process that is already running.
type ExternalRun struct {
	Project    string         `json:"project"`
	Experiment string         `json:"experiment"`
	Name       string         `json:"name"`
	Host       string         `json:"host"`
	Config     map[string]any `json:"config"`
	Notes      string         `json:"notes"`
}

// StartExternal records a run the platform is not executing and returns it
// with a token the SDK uses to report metrics.
func (s *Service) StartExternal(ctx context.Context, req ExternalRun) (*v1.Run, string, error) {
	if req.Project == "" {
		req.Project = "external"
	}
	if req.Experiment == "" {
		req.Experiment = "default"
	}
	if !recipes.ValidName(req.Project) || !recipes.ValidName(req.Experiment) {
		return nil, "", models.UserError("invalid project or experiment name")
	}
	host := req.Host
	if host == "" {
		host = "unknown"
	}
	if !recipes.ValidName(host) {
		return nil, "", models.UserError("invalid host %q (letters, digits, '.', '_', '-')", host)
	}
	owner := auth.PrincipalFrom(ctx).Actor()
	p, err := s.store().CreateProject(ctx, req.Project, "", owner)
	if err != nil && !models.IsConflict(err) {
		return nil, "", err
	}
	if p == nil {
		if p, err = s.store().GetProject(ctx, req.Project); err != nil {
			return nil, "", err
		}
	}
	exp, err := s.store().UpsertExperiment(ctx, p.ID, req.Experiment, "", "", nil)
	if err != nil {
		return nil, "", err
	}
	run := &v1.Run{
		ExperimentID: exp.ID,
		Name:         req.Name,
		Target:       "external:" + host,
		BackendKind:  "external",
		BackendID:    host,
		Status:       v1.PhaseRunning,
		Resources:    json.RawMessage(`{}`),
		Env:          json.RawMessage(`{}`),
		Metadata: v1.RunMetadata{
			Params: req.Config, Notes: req.Notes, Submitter: owner,
			LogMode: "external", Message: "started by the SDK",
		},
	}
	if err := s.store().InsertRun(ctx, run); err != nil {
		return nil, "", err
	}
	s.store().AddEvent(ctx, run.ID, "submitted", map[string]any{"source": "sdk", "host": host})
	saved, err := s.store().GetRun(ctx, run.ID)
	if err != nil {
		return nil, "", err
	}
	return saved, s.RT.RunToken(run.ID), nil
}

// FinishExternal closes an external run. The SDK calls it from atexit, so a
// run whose process dies without it is left to the caller to clean up.
func (s *Service) FinishExternal(ctx context.Context, id, status string, exitCode *int) (*v1.Run, error) {
	if !v1.Terminal(status) {
		return nil, models.UserError("finish needs a terminal status, got %q", status)
	}
	run, _, err := s.store().UpdateRunStatus(ctx, id, models.RunUpdate{
		Phase: status, ExitCode: exitCode, Message: fmt.Sprintf("finished by the SDK (%s)", status),
	})
	return run, err
}
