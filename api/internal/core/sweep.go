package core

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/lovemoon-ai/mldojo/api/internal/auth"
	"github.com/lovemoon-ai/mldojo/api/internal/models"
	v1 "github.com/lovemoon-ai/mldojo/proto/mldojo/v1"
	"github.com/lovemoon-ai/mldojo/recipes"
)

const sweepInterval = 20 * time.Second

// CreateSweep validates a sweep definition and stores it. The controller
// launches the runs; nothing is submitted here, so a sweep with a budget of
// 200 does not produce 200 runs the moment it is created.
func (s *Service) CreateSweep(ctx context.Context, sw *recipes.Sweep) (*models.Sweep, error) {
	if err := sw.Validate(); err != nil {
		return nil, models.UserError("%v", err)
	}
	if sw.RecipeYAML == "" {
		return nil, models.UserError("sweep needs the recipe inline (recipe_yaml); the CLI reads the file for you")
	}
	if _, err := recipes.Parse([]byte(sw.RecipeYAML)); err != nil {
		return nil, models.UserError("sweep recipe: %v", err)
	}
	if _, err := recipes.ParseTarget(sw.Target); err != nil {
		return nil, models.UserError("%v", err)
	}
	owner := auth.PrincipalFrom(ctx).Actor()
	if _, err := s.store().CreateProject(ctx, sw.Metadata.Project, "", owner); err != nil && !models.IsConflict(err) {
		return nil, err
	}
	spec, err := json.Marshal(sw)
	if err != nil {
		return nil, err
	}
	created, err := s.store().CreateSweep(ctx, &models.Sweep{
		Project: sw.Metadata.Project, Name: sw.Metadata.Name, Spec: spec,
		RecipeYAML: sw.RecipeYAML, Target: sw.Target, Owner: owner,
		Seed: time.Now().UnixNano(),
	})
	if err != nil {
		return nil, err
	}
	// Start it now rather than waiting out a tick, so `sweep create` is
	// followed by runs appearing.
	go s.TickSweeps(context.WithoutCancel(ctx))
	return created, nil
}

// StartSweepController tops sweeps up on a timer.
func (s *Service) StartSweepController(ctx context.Context) {
	go func() {
		t := time.NewTicker(sweepInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.TickSweeps(ctx)
			}
		}
	}()
}

// TickSweeps launches whatever each running sweep is allowed to launch now.
func (s *Service) TickSweeps(ctx context.Context) {
	sweeps, err := s.store().ListSweeps(ctx, "", models.SweepRunning)
	if err != nil {
		slog.Warn("sweeps: list", "err", err)
		return
	}
	for i := range sweeps {
		if err := s.advanceSweep(ctx, &sweeps[i]); err != nil {
			slog.Warn("sweeps: advance", "sweep", sweeps[i].Name, "err", err)
		}
	}
}

func (s *Service) advanceSweep(ctx context.Context, sw *models.Sweep) error {
	var spec recipes.Sweep
	if err := json.Unmarshal(sw.Spec, &spec); err != nil {
		return err
	}
	if sw.Launched >= spec.Budget.MaxRuns {
		if sw.ActiveRuns == 0 {
			return s.store().SetSweepStatus(ctx, sw.ID, models.SweepDone)
		}
		return nil // budget spent; wait for the stragglers
	}
	room := spec.Budget.MaxParallel - sw.ActiveRuns
	if room > spec.Budget.MaxRuns-sw.Launched {
		room = spec.Budget.MaxRuns - sw.Launched
	}
	if room <= 0 {
		return nil
	}

	// The sweep's own owner submits, so ownership and audit attribute the
	// runs to a person rather than to nobody.
	ctx = auth.WithPrincipal(ctx, &auth.Principal{
		Kind: "sweep", ID: sw.ID, Name: sw.Owner, Role: auth.RoleAdmin,
	})

	var grid []map[string]any
	if spec.Method == recipes.MethodGrid {
		grid = spec.GridPoints()
	}
	launched := 0
	for i := range room {
		n := sw.Launched + i
		var point map[string]any
		if spec.Method == recipes.MethodGrid {
			if n >= len(grid) {
				break
			}
			point = grid[n]
		} else {
			// Seeded by the sweep plus the index, so a restarted controller
			// continues the sequence instead of redrawing old points.
			point = spec.Sample(rand.New(rand.NewPCG(uint64(sw.Seed), uint64(n))))
		}
		if err := s.launchSweepRun(ctx, sw, &spec, point, n); err != nil {
			slog.Warn("sweeps: launch", "sweep", sw.Name, "err", err)
			break
		}
		launched++
	}
	if launched > 0 {
		return s.store().RecordSweepLaunch(ctx, sw.ID, launched)
	}
	return nil
}

func (s *Service) launchSweepRun(ctx context.Context, sw *models.Sweep, spec *recipes.Sweep, point map[string]any, n int) error {
	res, err := s.Submit(ctx, v1.SubmitRequest{
		RecipeYAML: sw.RecipeYAML,
		Target:     sw.Target,
		Project:    sw.Project,
		Experiment: sw.Name,
		Params:     point,
		Notes:      fmt.Sprintf("sweep %s #%d", sw.Name, n+1),
	})
	if err != nil {
		return err
	}
	for _, r := range res.Runs {
		if err := s.store().MergeRunMetadata(ctx, r.ID, map[string]any{"sweep_id": sw.ID}); err != nil {
			return err
		}
	}
	return nil
}

// SweepStatus is a sweep with its leaderboard.
type SweepStatus struct {
	*models.Sweep
	Metric  string           `json:"metric"`
	Goal    string           `json:"goal"`
	Results []v1.SweepResult `json:"results"`
}

// SweepStatusOf ranks a sweep's runs by its metric.
func (s *Service) SweepStatusOf(ctx context.Context, sw *models.Sweep, limit int) (*SweepStatus, error) {
	var spec recipes.Sweep
	if err := json.Unmarshal(sw.Spec, &spec); err != nil {
		return nil, err
	}
	results, err := s.store().SweepResults(ctx, sw.ID, spec.Metric.Name, spec.Metric.Goal == "min", limit)
	if err != nil {
		return nil, err
	}
	return &SweepStatus{Sweep: sw, Metric: spec.Metric.Name, Goal: spec.Metric.Goal, Results: results}, nil
}
