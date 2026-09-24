package models

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Stages a model version can be in.
const (
	StageNone       = "none"
	StageStaging    = "staging"
	StageProduction = "production"
	StageArchived   = "archived"
)

func ValidStage(s string) bool {
	switch s {
	case StageNone, StageStaging, StageProduction, StageArchived:
		return true
	}
	return false
}

type Model struct {
	ID          string        `json:"id"`
	Project     string        `json:"project"`
	Name        string        `json:"name"`
	Description string        `json:"description,omitempty"`
	Owner       string        `json:"owner,omitempty"`
	CreatedAt   time.Time     `json:"created_at"`
	Versions    int           `json:"versions"`
	Latest      *ModelVersion `json:"latest,omitempty"`
}

type ModelVersion struct {
	ID        string          `json:"id"`
	Model     string          `json:"model,omitempty"`
	Version   int             `json:"version"`
	RunID     *string         `json:"run_id,omitempty"`
	URI       string          `json:"uri"`
	SHA256    string          `json:"sha256,omitempty"`
	SizeBytes int64           `json:"size_bytes,omitempty"`
	Stage     string          `json:"stage"`
	Notes     string          `json:"notes,omitempty"`
	Metrics   json.RawMessage `json:"metrics,omitempty"`
	CreatedBy string          `json:"created_by,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
}

// Two spellings of the same column list: RETURNING has no table alias.
const (
	versionCols = `v.id, v.version, v.run_id, v.uri, v.sha256, v.size_bytes, v.stage, v.notes,
		v.metrics, v.created_by, v.created_at`
	versionColsBare = `id, version, run_id, uri, sha256, size_bytes, stage, notes,
		metrics, created_by, created_at`
)

func scanVersion(row pgx.Row) (*ModelVersion, error) {
	var v ModelVersion
	var id uuid.UUID
	var runID *uuid.UUID
	err := row.Scan(&id, &v.Version, &runID, &v.URI, &v.SHA256, &v.SizeBytes, &v.Stage,
		&v.Notes, &v.Metrics, &v.CreatedBy, &v.CreatedAt)
	v.ID = id.String()
	if runID != nil {
		s := runID.String()
		v.RunID = &s
	}
	return &v, err
}

// UpsertModel creates the model if it is new; registering a version should
// not require a separate "create the model first" step.
func (s *Store) UpsertModel(ctx context.Context, project, name, owner string) (*Model, error) {
	p, err := s.GetProject(ctx, project)
	if err != nil {
		return nil, err
	}
	_, err = s.DB.Exec(ctx, `INSERT INTO models(id, project_id, name, owner) VALUES ($1,$2,$3,$4)
		ON CONFLICT (project_id, name) DO NOTHING`, uuid.New(), p.ID, name, owner)
	if err != nil {
		return nil, err
	}
	return s.GetModel(ctx, project, name)
}

func (s *Store) GetModel(ctx context.Context, project, name string) (*Model, error) {
	var m Model
	var id uuid.UUID
	err := s.DB.QueryRow(ctx, `SELECT m.id, p.name, m.name, m.description, m.owner, m.created_at,
		(SELECT count(*) FROM model_versions v WHERE v.model_id=m.id)
		FROM models m JOIN projects p ON p.id=m.project_id WHERE p.name=$1 AND m.name=$2`, project, name).
		Scan(&id, &m.Project, &m.Name, &m.Description, &m.Owner, &m.CreatedAt, &m.Versions)
	if err != nil {
		return nil, notFoundOr(err, "model %s/%s", project, name)
	}
	m.ID = id.String()
	return &m, nil
}

func (s *Store) ListModels(ctx context.Context, project string) ([]Model, error) {
	q := `SELECT m.id, p.name, m.name, m.description, m.owner, m.created_at,
		(SELECT count(*) FROM model_versions v WHERE v.model_id=m.id)
		FROM models m JOIN projects p ON p.id=m.project_id`
	var args []any
	if project != "" {
		args = append(args, project)
		q += ` WHERE p.name=$1`
	}
	q += ` ORDER BY p.name, m.name`
	rows, err := s.DB.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Model{}
	for rows.Next() {
		var m Model
		var id uuid.UUID
		if err := rows.Scan(&id, &m.Project, &m.Name, &m.Description, &m.Owner, &m.CreatedAt, &m.Versions); err != nil {
			return nil, err
		}
		m.ID = id.String()
		out = append(out, m)
	}
	return out, rows.Err()
}

// AddModelVersion appends a version. Numbering is per model and assigned by
// the database, so two concurrent registrations cannot collide.
func (s *Store) AddModelVersion(ctx context.Context, modelID string, v *ModelVersion) (*ModelVersion, error) {
	if len(v.Metrics) == 0 {
		v.Metrics = json.RawMessage(`{}`)
	}
	stage := v.Stage
	if stage == "" {
		stage = StageNone
	}
	row := s.DB.QueryRow(ctx, `INSERT INTO model_versions(id, model_id, version, run_id, uri, sha256, size_bytes, stage, notes, metrics, created_by)
		SELECT $1, $2, COALESCE(max(version), 0) + 1, $3, $4, $5, $6, $7, $8, $9, $10
		FROM model_versions WHERE model_id = $2
		RETURNING `+versionColsBare,
		uuid.New(), modelID, v.RunID, v.URI, v.SHA256, v.SizeBytes, stage, v.Notes, []byte(v.Metrics), v.CreatedBy)
	return scanVersion(row)
}

func (s *Store) ListModelVersions(ctx context.Context, modelID string) ([]ModelVersion, error) {
	rows, err := s.DB.Query(ctx, `SELECT `+versionCols+` FROM model_versions v
		WHERE v.model_id=$1 ORDER BY v.version DESC`, modelID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ModelVersion{}
	for rows.Next() {
		v, err := scanVersion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *v)
	}
	return out, rows.Err()
}

// SetModelStage moves a version. Production and staging are exclusive: the
// previous holder is archived, so "what is in production" has one answer.
func (s *Store) SetModelStage(ctx context.Context, modelID string, version int, stage string) (*ModelVersion, error) {
	if !ValidStage(stage) {
		return nil, UserError("unknown stage %q (none, staging, production, archived)", stage)
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if stage == StageProduction || stage == StageStaging {
		if _, err := tx.Exec(ctx, `UPDATE model_versions SET stage=$3
			WHERE model_id=$1 AND stage=$2 AND version <> $4`, modelID, stage, StageArchived, version); err != nil {
			return nil, err
		}
	}
	row := tx.QueryRow(ctx, `UPDATE model_versions SET stage=$3 WHERE model_id=$1 AND version=$2
		RETURNING `+versionColsBare, modelID, version, stage)
	v, err := scanVersion(row)
	if err != nil {
		return nil, notFoundOr(err, "model version %d", version)
	}
	return v, tx.Commit(ctx)
}

// ModelVersionsFromRun is the lineage lookup: what did this run produce?
func (s *Store) ModelVersionsFromRun(ctx context.Context, runID string) ([]ModelVersion, error) {
	rows, err := s.DB.Query(ctx, `SELECT `+versionCols+`, m.name FROM model_versions v
		JOIN models m ON m.id = v.model_id WHERE v.run_id=$1 ORDER BY v.created_at`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ModelVersion{}
	for rows.Next() {
		var v ModelVersion
		var id uuid.UUID
		var rid *uuid.UUID
		if err := rows.Scan(&id, &v.Version, &rid, &v.URI, &v.SHA256, &v.SizeBytes, &v.Stage,
			&v.Notes, &v.Metrics, &v.CreatedBy, &v.CreatedAt, &v.Model); err != nil {
			return nil, err
		}
		v.ID = id.String()
		if rid != nil {
			s := rid.String()
			v.RunID = &s
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// ModelConsumer is a run that read a model version.
type ModelConsumer struct {
	Version    int       `json:"version"`
	RunID      string    `json:"run_id"`
	Project    string    `json:"project"`
	Experiment string    `json:"experiment"`
	Name       string    `json:"name"`
	Status     string    `json:"status"`
	CreatedAt  time.Time `json:"created_at"`
}

// RunsUsingModel is the lineage direction the registry was missing: not
// "where did this model come from" but "what was done with it". Without it
// a success rate could not be traced back to the checkpoint that produced
// it, which is the question an evaluation exists to answer.
func (s *Store) RunsUsingModel(ctx context.Context, model string) ([]ModelConsumer, error) {
	rows, err := s.DB.Query(ctx, `SELECT (m->>'version')::int, r.id, p.name, e.name, r.name, r.status, r.created_at
		FROM runs r
		JOIN experiments e ON e.id=r.experiment_id
		JOIN projects p ON p.id=e.project_id,
		LATERAL jsonb_array_elements(COALESCE(r.metadata->'models', '[]'::jsonb)) m
		WHERE m->>'name' = $1
		ORDER BY r.created_at`, model)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ModelConsumer{}
	for rows.Next() {
		var c ModelConsumer
		var id uuid.UUID
		if err := rows.Scan(&c.Version, &id, &c.Project, &c.Experiment, &c.Name, &c.Status, &c.CreatedAt); err != nil {
			return nil, err
		}
		c.RunID = id.String()
		out = append(out, c)
	}
	return out, rows.Err()
}
