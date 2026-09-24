package models

import (
	"context"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	v1 "github.com/lovemoon-ai/mldojo/proto/mldojo/v1"
)

const projectCols = `p.id, p.name, p.description, p.owner, p.created_at, p.updated_at,
	p.max_concurrent_runs,
	(SELECT count(*) FROM experiments e WHERE e.project_id=p.id),
	(SELECT count(*) FROM runs r JOIN experiments e ON r.experiment_id=e.id WHERE e.project_id=p.id),
	(SELECT count(*) FROM runs r JOIN experiments e ON r.experiment_id=e.id
	   WHERE e.project_id=p.id AND r.status IN ('queued','starting','running'))`

// ProjectBusyRuns counts the project's runs that are actually occupying
// capacity. See NodeBusyRuns for why queued runs do not count.
func (s *Store) ProjectBusyRuns(ctx context.Context, name string) (int, error) {
	var n int
	err := s.DB.QueryRow(ctx, `SELECT count(*) FROM runs r
		JOIN experiments e ON r.experiment_id=e.id
		JOIN projects p ON e.project_id=p.id
		WHERE p.name=$1 AND r.status IN ('starting','running')`, name).Scan(&n)
	return n, err
}

func scanProject(row pgx.Row) (*v1.Project, error) {
	var p v1.Project
	var id uuid.UUID
	err := row.Scan(&id, &p.Name, &p.Description, &p.Owner, &p.CreatedAt, &p.UpdatedAt,
		&p.MaxConcurrentRuns, &p.ExperimentCount, &p.RunCount, &p.ActiveRuns)
	p.ID = id.String()
	return &p, err
}

func (s *Store) CreateProject(ctx context.Context, name, desc, owner string) (*v1.Project, error) {
	id := uuid.New()
	_, err := s.DB.Exec(ctx, `INSERT INTO projects(id, name, description, owner) VALUES ($1,$2,$3,$4)`, id, name, desc, owner)
	if isUnique(err) {
		return nil, Conflict("project %q already exists", name)
	}
	if err != nil {
		return nil, err
	}
	return s.GetProject(ctx, name)
}

// EnsureProject returns the project, creating it if missing.
func (s *Store) EnsureProject(ctx context.Context, name string) (*v1.Project, error) {
	_, err := s.DB.Exec(ctx, `INSERT INTO projects(id, name) VALUES ($1,$2) ON CONFLICT (name) DO NOTHING`, uuid.New(), name)
	if err != nil {
		return nil, err
	}
	return s.GetProject(ctx, name)
}

func (s *Store) GetProject(ctx context.Context, name string) (*v1.Project, error) {
	p, err := scanProject(s.DB.QueryRow(ctx, `SELECT `+projectCols+` FROM projects p WHERE p.name=$1`, name))
	if err != nil {
		return nil, notFoundOr(err, "project %q", name)
	}
	return p, nil
}

func (s *Store) ListProjects(ctx context.Context) ([]v1.Project, error) {
	rows, err := s.DB.Query(ctx, `SELECT `+projectCols+` FROM projects p ORDER BY p.updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []v1.Project{}
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

func (s *Store) DeleteProject(ctx context.Context, name string, force bool) error {
	p, err := s.GetProject(ctx, name)
	if err != nil {
		return err
	}
	if p.ExperimentCount > 0 && !force {
		return Conflict("project %q has %d experiments; pass force to delete", name, p.ExperimentCount)
	}
	_, err = s.DB.Exec(ctx, `DELETE FROM projects WHERE id=$1`, p.ID)
	return err
}

func (s *Store) TouchProject(ctx context.Context, projectID string) {
	if _, err := s.DB.Exec(ctx, `UPDATE projects SET updated_at=now() WHERE id=$1`, projectID); err != nil {
		slog.Warn("touch project", "project", projectID, "err", err)
	}
}

// Experiments -----------------------------------------------------------

const expCols = `e.id, e.project_id, p.name, e.name, e.description, e.recipe_yaml, e.tags, e.created_at, e.updated_at,
	COALESCE((SELECT jsonb_object_agg(status, n) FROM (SELECT status, count(*) n FROM runs r WHERE r.experiment_id=e.id GROUP BY status) c), '{}'::jsonb)`

func scanExperiment(row pgx.Row) (*v1.Experiment, error) {
	var e v1.Experiment
	var id, pid uuid.UUID
	err := row.Scan(&id, &pid, &e.Project, &e.Name, &e.Description, &e.RecipeYAML, &e.Tags, &e.CreatedAt, &e.UpdatedAt, &e.RunCounts)
	e.ID, e.ProjectID = id.String(), pid.String()
	e.Tags = nonNil(e.Tags)
	if e.RunCounts == nil {
		e.RunCounts = map[string]int{}
	}
	return &e, err
}

func (s *Store) CreateExperiment(ctx context.Context, project, name, desc, recipe string, tags []string) (*v1.Experiment, error) {
	p, err := s.GetProject(ctx, project)
	if err != nil {
		return nil, err
	}
	_, err = s.DB.Exec(ctx, `INSERT INTO experiments(id, project_id, name, description, recipe_yaml, tags) VALUES ($1,$2,$3,$4,$5,$6)`,
		uuid.New(), p.ID, name, desc, recipe, nonNil(tags))
	if isUnique(err) {
		return nil, Conflict("experiment %s/%s already exists", project, name)
	}
	if err != nil {
		return nil, err
	}
	s.TouchProject(ctx, p.ID)
	return s.GetExperiment(ctx, project, name)
}

// UpsertExperiment creates the experiment or refreshes its recipe/tags.
func (s *Store) UpsertExperiment(ctx context.Context, projectID, name, desc, recipe string, tags []string) (*v1.Experiment, error) {
	var id uuid.UUID
	err := s.DB.QueryRow(ctx, `INSERT INTO experiments(id, project_id, name, description, recipe_yaml, tags) VALUES ($1,$2,$3,$4,$5,$6)
		ON CONFLICT (project_id, name) DO UPDATE SET recipe_yaml=excluded.recipe_yaml,
		  tags=excluded.tags,
		  description=CASE WHEN excluded.description <> '' THEN excluded.description ELSE experiments.description END,
		  updated_at=now()
		RETURNING id`, uuid.New(), projectID, name, desc, recipe, nonNil(tags)).Scan(&id)
	if err != nil {
		return nil, err
	}
	s.TouchProject(ctx, projectID)
	return s.GetExperimentByID(ctx, id.String())
}

func (s *Store) GetExperiment(ctx context.Context, project, name string) (*v1.Experiment, error) {
	e, err := scanExperiment(s.DB.QueryRow(ctx, `SELECT `+expCols+` FROM experiments e JOIN projects p ON p.id=e.project_id
		WHERE p.name=$1 AND e.name=$2`, project, name))
	if err != nil {
		return nil, notFoundOr(err, "experiment %s/%s", project, name)
	}
	return e, nil
}

func (s *Store) GetExperimentByID(ctx context.Context, id string) (*v1.Experiment, error) {
	e, err := scanExperiment(s.DB.QueryRow(ctx, `SELECT `+expCols+` FROM experiments e JOIN projects p ON p.id=e.project_id
		WHERE e.id=$1`, id))
	if err != nil {
		return nil, notFoundOr(err, "experiment %s", id)
	}
	return e, nil
}

func (s *Store) ListExperiments(ctx context.Context, project string) ([]v1.Experiment, error) {
	if _, err := s.GetProject(ctx, project); err != nil {
		return nil, err
	}
	rows, err := s.DB.Query(ctx, `SELECT `+expCols+` FROM experiments e JOIN projects p ON p.id=e.project_id
		WHERE p.name=$1 ORDER BY e.updated_at DESC`, project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []v1.Experiment{}
	for rows.Next() {
		e, err := scanExperiment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *e)
	}
	return out, rows.Err()
}

func (s *Store) DeleteExperiment(ctx context.Context, project, name string, force bool) error {
	e, err := s.GetExperiment(ctx, project, name)
	if err != nil {
		return err
	}
	n := 0
	for _, c := range e.RunCounts {
		n += c
	}
	if n > 0 && !force {
		return Conflict("experiment %s/%s has %d runs; pass force to delete", project, name, n)
	}
	_, err = s.DB.Exec(ctx, `DELETE FROM experiments WHERE id=$1`, e.ID)
	return err
}

// SetProjectMaxRuns caps a project's concurrent runs. 0 = unlimited.
func (s *Store) SetProjectMaxRuns(ctx context.Context, name string, max int) (*v1.Project, error) {
	tag, err := s.DB.Exec(ctx, `UPDATE projects SET max_concurrent_runs=$2, updated_at=now() WHERE name=$1`, name, max)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, NotFound("project %q", name)
	}
	return s.GetProject(ctx, name)
}
