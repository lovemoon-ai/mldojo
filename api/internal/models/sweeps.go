package models

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	v1 "github.com/lovemoon-ai/mldojo/proto/mldojo/v1"
)

// Sweep statuses.
const (
	SweepRunning = "running"
	SweepDone    = "done"
	SweepStopped = "stopped"
)

type Sweep struct {
	ID         string          `json:"id"`
	Project    string          `json:"project"`
	Name       string          `json:"name"`
	Spec       json.RawMessage `json:"spec"`
	RecipeYAML string          `json:"recipe_yaml,omitempty"`
	Target     string          `json:"target"`
	Status     string          `json:"status"`
	Seed       int64           `json:"seed"`
	Launched   int             `json:"launched"`
	Owner      string          `json:"owner,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
	UpdatedAt  time.Time       `json:"updated_at"`
	// Aggregates.
	ActiveRuns int             `json:"active_runs"`
	BestRun    *v1.SweepResult `json:"best_run,omitempty"`
}

const sweepCols = `s.id, p.name, s.name, s.spec, s.recipe_yaml, s.target, s.status, s.seed, s.launched,
	s.owner, s.created_at, s.updated_at,
	(SELECT count(*) FROM runs r WHERE r.metadata->>'sweep_id' = s.id::text
	   AND r.status IN ('queued','starting','running'))`

func scanSweep(row pgx.Row) (*Sweep, error) {
	var s Sweep
	var id uuid.UUID
	err := row.Scan(&id, &s.Project, &s.Name, &s.Spec, &s.RecipeYAML, &s.Target, &s.Status,
		&s.Seed, &s.Launched, &s.Owner, &s.CreatedAt, &s.UpdatedAt, &s.ActiveRuns)
	s.ID = id.String()
	return &s, err
}

func (st *Store) CreateSweep(ctx context.Context, s *Sweep) (*Sweep, error) {
	p, err := st.GetProject(ctx, s.Project)
	if err != nil {
		return nil, err
	}
	id := uuid.New()
	_, err = st.DB.Exec(ctx, `INSERT INTO sweeps(id, project_id, name, spec, recipe_yaml, target, seed, owner)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, id, p.ID, s.Name, []byte(s.Spec), s.RecipeYAML, s.Target, s.Seed, s.Owner)
	if isUnique(err) {
		return nil, Conflict("sweep %s/%s already exists", s.Project, s.Name)
	}
	if err != nil {
		return nil, err
	}
	return st.GetSweepByID(ctx, id.String())
}

func (st *Store) GetSweepByID(ctx context.Context, id string) (*Sweep, error) {
	s, err := scanSweep(st.DB.QueryRow(ctx,
		`SELECT `+sweepCols+` FROM sweeps s JOIN projects p ON p.id=s.project_id WHERE s.id=$1`, id))
	if err != nil {
		return nil, notFoundOr(err, "sweep %q", id)
	}
	return s, nil
}

func (st *Store) GetSweep(ctx context.Context, project, name string) (*Sweep, error) {
	s, err := scanSweep(st.DB.QueryRow(ctx,
		`SELECT `+sweepCols+` FROM sweeps s JOIN projects p ON p.id=s.project_id
		 WHERE p.name=$1 AND s.name=$2`, project, name))
	if err != nil {
		return nil, notFoundOr(err, "sweep %s/%s", project, name)
	}
	return s, nil
}

func (st *Store) ListSweeps(ctx context.Context, project, status string) ([]Sweep, error) {
	q := `SELECT ` + sweepCols + ` FROM sweeps s JOIN projects p ON p.id=s.project_id WHERE 1=1`
	var args []any
	if project != "" {
		args = append(args, project)
		q += ` AND p.name=$1`
	}
	if status != "" {
		args = append(args, status)
		q += ` AND s.status=$` + strconv.Itoa(len(args))
	}
	q += ` ORDER BY s.created_at DESC`
	rows, err := st.DB.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Sweep{}
	for rows.Next() {
		s, err := scanSweep(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *s)
	}
	return out, rows.Err()
}

// RecordSweepLaunch bumps the launch counter. The counter drives sampling, so
// it is incremented in the same statement that reads it.
func (st *Store) RecordSweepLaunch(ctx context.Context, id string, n int) error {
	_, err := st.DB.Exec(ctx, `UPDATE sweeps SET launched = launched + $2, updated_at = now() WHERE id=$1`, id, n)
	return err
}

func (st *Store) SetSweepStatus(ctx context.Context, id, status string) error {
	_, err := st.DB.Exec(ctx, `UPDATE sweeps SET status=$2, updated_at=now() WHERE id=$1`, id, status)
	return err
}

// SweepResults ranks a sweep's runs by its metric.
func (st *Store) SweepResults(ctx context.Context, id, metric string, lowerIsBetter bool, limit int) ([]v1.SweepResult, error) {
	dir := "DESC"
	if lowerIsBetter {
		dir = "ASC"
	}
	if limit <= 0 {
		limit = 50
	}
	rows, err := st.DB.Query(ctx, `
		SELECT r.id, r.name, r.status, r.metadata->'params', m.value, m.step
		FROM runs r
		LEFT JOIN LATERAL (
			SELECT value, step FROM run_metrics
			WHERE run_id = r.id AND key = $2 ORDER BY step DESC LIMIT 1
		) m ON true
		WHERE r.metadata->>'sweep_id' = $1
		ORDER BY (m.value IS NULL), m.value `+dir+`, r.created_at
		LIMIT $3`, id, metric, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []v1.SweepResult{}
	for rows.Next() {
		var r v1.SweepResult
		var value *float64
		var step *int64
		if err := rows.Scan(&r.ID, &r.Name, &r.Status, &r.Params, &value, &step); err != nil {
			return nil, err
		}
		if value != nil {
			f := v1.Float(*value)
			r.Value = &f
		}
		if step != nil {
			r.Step = *step
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
