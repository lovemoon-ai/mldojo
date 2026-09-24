package models

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	v1 "github.com/lovemoon-ai/mldojo/proto/mldojo/v1"
)

const runCols = `r.id, r.experiment_id, p.name, e.name, r.name, r.target, r.backend_kind, r.backend_id, r.backend_handle,
	r.resources, r.env, r.code_commit, r.code_patch_uri, r.status, r.exit_code, r.started_at, r.finished_at,
	r.created_at, r.metadata`

const runFrom = ` FROM runs r JOIN experiments e ON e.id=r.experiment_id JOIN projects p ON p.id=e.project_id `

func scanRun(row pgx.Row) (*v1.Run, error) {
	var r v1.Run
	var id, eid uuid.UUID
	var handle, res, env, meta []byte
	err := row.Scan(&id, &eid, &r.Project, &r.Experiment, &r.Name, &r.Target, &r.BackendKind, &r.BackendID, &handle,
		&res, &env, &r.CodeCommit, &r.CodePatchURI, &r.Status, &r.ExitCode, &r.StartedAt, &r.FinishedAt,
		&r.CreatedAt, &meta)
	if err != nil {
		return nil, err
	}
	r.ID, r.ExperimentID = id.String(), eid.String()
	r.BackendHandle, r.Resources, r.Env = handle, res, env
	if err := json.Unmarshal(meta, &r.Metadata); err != nil {
		return nil, err
	}
	return &r, nil
}

func (s *Store) InsertRun(ctx context.Context, r *v1.Run) error {
	if r.ID == "" {
		r.ID = uuid.NewString()
	}
	if len(r.BackendHandle) == 0 {
		r.BackendHandle = json.RawMessage(`{}`)
	}
	_, err := s.DB.Exec(ctx, `INSERT INTO runs(id, experiment_id, name, target, backend_kind, backend_id, backend_handle,
		resources, env, code_commit, code_patch_uri, status, metadata) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
		r.ID, r.ExperimentID, r.Name, r.Target, r.BackendKind, r.BackendID, []byte(r.BackendHandle),
		[]byte(r.Resources), []byte(r.Env), r.CodeCommit, r.CodePatchURI, r.Status, mustJSON(r.Metadata))
	return err
}

// GetRun accepts a full id or a unique prefix (like git short hashes).
func (s *Store) GetRun(ctx context.Context, id string) (*v1.Run, error) {
	id = strings.TrimSpace(id)
	if _, err := uuid.Parse(id); err == nil {
		r, err := scanRun(s.DB.QueryRow(ctx, `SELECT `+runCols+runFrom+`WHERE r.id=$1`, id))
		if err != nil {
			return nil, notFoundOr(err, "run %s", id)
		}
		return r, nil
	}
	if len(id) < 4 {
		return nil, NotFound("run %q (use at least 4 characters of the id)", id)
	}
	rows, err := s.DB.Query(ctx, `SELECT `+runCols+runFrom+`WHERE r.id::text LIKE $1 LIMIT 2`, likePrefix(strings.ToLower(id)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var found []*v1.Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		found = append(found, r)
	}
	switch len(found) {
	case 0:
		return nil, NotFound("run %s", id)
	case 1:
		return found[0], nil
	}
	return nil, Conflict("run id prefix %q is ambiguous", id)
}

type RunFilter struct {
	Project, Experiment, Status, Target, BackendKind, BackendID string
	Tag                                                         string // one of metadata.tags
	Search                                                      string // substring of the run or experiment name
	Active                                                      bool   // non-terminal only
	Sort                                                        string // created | started | duration | name | status
	Asc                                                         bool
	Limit, Offset                                               int
}

// runSort maps the API's sort names to SQL, so a caller cannot inject an
// ORDER BY. Every option ends with created_at to stay deterministic.
var runSort = map[string]string{
	"":         "r.created_at",
	"created":  "r.created_at",
	"started":  "r.started_at",
	"duration": "COALESCE(r.finished_at, now()) - r.started_at",
	"name":     "r.name",
	"status":   "r.status",
	"target":   "r.target",
}

// SortOptions lists the accepted sort keys, for error messages.
func SortOptions() []string {
	out := make([]string, 0, len(runSort))
	for k := range runSort {
		if k != "" {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// predicates builds the shared WHERE clause, so listing and counting can
// never disagree about what matches.
func (f RunFilter) predicates() (string, []any) {
	var where []string
	var args []any
	add := func(cond string, v any) {
		args = append(args, v)
		where = append(where, fmt.Sprintf(cond, len(args)))
	}
	if f.Project != "" {
		add("p.name=$%d", f.Project)
	}
	if f.Experiment != "" {
		add("e.name=$%d", f.Experiment)
	}
	if f.Status != "" {
		add("r.status=ANY(string_to_array($%d, ','))", f.Status)
	}
	if f.Target != "" {
		add("r.target=$%d", f.Target)
	}
	if f.BackendKind != "" {
		add("r.backend_kind=$%d", f.BackendKind)
	}
	if f.BackendID != "" {
		add("r.backend_id=$%d", f.BackendID)
	}
	if f.Tag != "" {
		// jsonb_exists rather than the ? operator: ? reads as a placeholder
		// to enough tooling that it is not worth the ambiguity.
		add("jsonb_exists(r.metadata->'tags', $%d)", f.Tag)
	}
	if f.Search != "" {
		args = append(args, f.Search)
		n := len(args)
		where = append(where, fmt.Sprintf("(r.name ILIKE '%%' || $%d || '%%' OR e.name ILIKE '%%' || $%d || '%%')", n, n))
	}
	if f.Active {
		where = append(where, "r.status IN ('queued','starting','running')")
	}
	if len(where) == 0 {
		return "", args
	}
	return "WHERE " + strings.Join(where, " AND "), args
}

// CountRuns is how many runs match, ignoring limit and offset.
func (s *Store) CountRuns(ctx context.Context, f RunFilter) (int, error) {
	cond, args := f.predicates()
	var n int
	err := s.DB.QueryRow(ctx, `SELECT count(*) `+runFrom+cond, args...).Scan(&n)
	return n, err
}

func (s *Store) ListRuns(ctx context.Context, f RunFilter) ([]v1.Run, error) {
	cond, args := f.predicates()
	q := `SELECT ` + runCols + runFrom + cond
	col, okSort := runSort[f.Sort]
	if !okSort {
		return nil, UserError("unknown sort %q (want one of %s)", f.Sort, strings.Join(SortOptions(), ", "))
	}
	dir := "DESC"
	if f.Asc {
		dir = "ASC"
	}
	// NULLS LAST keeps queued runs (no started_at) out of the way when
	// sorting by time, and the created_at tiebreak keeps paging stable.
	q += fmt.Sprintf(" ORDER BY %s %s NULLS LAST, r.created_at DESC", col, dir)
	if f.Limit <= 0 || f.Limit > 5000 {
		f.Limit = 200
	}
	q += fmt.Sprintf(" LIMIT %d", f.Limit)
	if f.Offset > 0 {
		q += fmt.Sprintf(" OFFSET %d", f.Offset)
	}
	rows, err := s.DB.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []v1.Run{}
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

// RunUpdate is a partial status update. Terminal runs never change phase.
type RunUpdate struct {
	Phase      string
	ExitCode   *int
	StartedAt  *time.Time
	FinishedAt *time.Time
	Message    string
	Commit     string
	// FailureKind is "infra" when the platform lost the run rather than the
	// job failing on its own. Only the former is worth retrying.
	FailureKind string
}

// UpdateRunStatus applies u and reports whether the phase changed.
//
// The read-modify-write runs inside a transaction with the row locked: an
// agent reporting completion and a user cancelling at the same time used to
// read the same "current" row and overwrite each other.
func (s *Store) UpdateRunStatus(ctx context.Context, id string, u RunUpdate) (*v1.Run, bool, error) {
	cur, err := s.GetRun(ctx, id) // also resolves a unique id prefix
	if err != nil {
		return nil, false, err
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback(ctx)

	// Re-read under the row lock: cur may already be stale.
	var status, commit string
	var exit *int
	var started, finished *time.Time
	if err := tx.QueryRow(ctx, `SELECT status, exit_code, started_at, finished_at, code_commit
		FROM runs WHERE id=$1 FOR UPDATE`, cur.ID).Scan(&status, &exit, &started, &finished, &commit); err != nil {
		return nil, false, err
	}
	if v1.Terminal(status) && u.Phase != status {
		return cur, false, nil
	}
	phase := status
	if u.Phase != "" {
		phase = u.Phase
	}
	if started == nil && u.StartedAt != nil {
		started = u.StartedAt
	}
	if started == nil && (phase == v1.PhaseRunning || v1.Terminal(phase)) && phase != v1.PhaseCancelled {
		now := time.Now()
		started = &now
	}
	if v1.Terminal(phase) && finished == nil {
		if u.FinishedAt != nil {
			finished = u.FinishedAt
		} else {
			now := time.Now()
			finished = &now
		}
	}
	if u.ExitCode != nil {
		exit = u.ExitCode
	}
	if u.Commit != "" && commit == "" {
		commit = u.Commit
	}
	meta := map[string]any{}
	if u.Message != "" {
		meta["message"] = u.Message
	}
	if _, err := tx.Exec(ctx, `UPDATE runs SET status=$2, exit_code=$3, started_at=$4, finished_at=$5, code_commit=$6,
		metadata = metadata || $7::jsonb WHERE id=$1`, cur.ID, phase, exit, started, finished, commit, mustJSON(meta)); err != nil {
		return nil, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, err
	}
	r, err := s.GetRun(ctx, cur.ID)
	return r, phase != status, err
}

func (s *Store) SetRunHandle(ctx context.Context, id string, handle any) error {
	_, err := s.DB.Exec(ctx, `UPDATE runs SET backend_handle=$2 WHERE id=$1`, id, mustJSON(handle))
	return err
}

// MergeRunMetadata shallow-merges fields into runs.metadata.
func (s *Store) MergeRunMetadata(ctx context.Context, id string, fields map[string]any) error {
	_, err := s.DB.Exec(ctx, `UPDATE runs SET metadata = metadata || $2::jsonb WHERE id=$1`, id, mustJSON(fields))
	return err
}

// Events ----------------------------------------------------------------

func (s *Store) AddEvent(ctx context.Context, runID, kind string, payload any) {
	if _, err := s.DB.Exec(ctx, `INSERT INTO run_events(run_id, kind, payload) VALUES ($1,$2,$3)`, runID, kind, mustJSON(payload)); err != nil {
		slog.Warn("add run event", "run", runID, "kind", kind, "err", err)
	}
}

func (s *Store) ListEvents(ctx context.Context, runID string) ([]v1.RunEvent, error) {
	rows, err := s.DB.Query(ctx, `SELECT id, run_id, ts, kind, payload FROM run_events WHERE run_id=$1 ORDER BY id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []v1.RunEvent{}
	for rows.Next() {
		var e v1.RunEvent
		var rid uuid.UUID
		var payload []byte
		if err := rows.Scan(&e.ID, &rid, &e.TS, &e.Kind, &payload); err != nil {
			return nil, err
		}
		e.RunID, e.Payload = rid.String(), payload
		out = append(out, e)
	}
	return out, rows.Err()
}

// Metrics ---------------------------------------------------------------

func (s *Store) UpsertMetrics(ctx context.Context, runID string, pts []v1.MetricPoint) error {
	if len(pts) == 0 {
		return nil
	}
	// De-duplicate (last wins): ON CONFLICT cannot touch a row twice.
	type k struct {
		key  string
		step int64
	}
	idx := map[k]int{}
	var steps []int64
	var keys []string
	var vals []float64
	var tss []time.Time
	for _, p := range pts {
		if p.TS.IsZero() {
			p.TS = time.Now()
		}
		kk := k{p.Key, p.Step}
		if i, ok := idx[kk]; ok {
			vals[i], tss[i] = p.Value, p.TS
			continue
		}
		idx[kk] = len(keys)
		steps, keys, vals, tss = append(steps, p.Step), append(keys, p.Key), append(vals, p.Value), append(tss, p.TS)
	}
	_, err := s.DB.Exec(ctx, `INSERT INTO run_metrics(run_id, step, key, value, ts)
		SELECT $1, u.step, u.key, u.value, u.ts FROM unnest($2::bigint[], $3::text[], $4::float8[], $5::timestamptz[]) AS u(step, key, value, ts)
		ON CONFLICT (run_id, key, step) DO UPDATE SET value=excluded.value, ts=excluded.ts`,
		runID, steps, keys, vals, tss)
	return err
}

// DefaultMaxMetricPoints caps how many points a single key returns when the
// caller does not ask for a specific budget.
const DefaultMaxMetricPoints = 2000

// ListMetrics returns a run's points, at most maxPoints *per key*, sampled at
// an even stride with the first and last point always kept. maxPoints <= 0
// uses DefaultMaxMetricPoints. The bool reports whether any key was sampled.
//
// The per-key budget matters: a plain `ORDER BY key, step LIMIT n` silently
// drops whole series whose key sorts late once a run exceeds n points.
func (s *Store) ListMetrics(ctx context.Context, runID, key string, sinceStep int64, maxPoints int) ([]v1.MetricPoint, bool, error) {
	if maxPoints <= 0 {
		maxPoints = DefaultMaxMetricPoints
	}
	filter := ""
	args := []any{runID, sinceStep, int64(maxPoints)}
	if key != "" {
		filter = ` AND key = ANY(string_to_array($4, ','))`
		args = append(args, key)
	}
	// stride = ceil(total/$3); rows 1 and total are always kept so the curve
	// keeps its real endpoints.
	q := `WITH pts AS (
		SELECT step, key, value, ts,
		       row_number() OVER (PARTITION BY key ORDER BY step) AS rn,
		       count(*)     OVER (PARTITION BY key)               AS total
		FROM run_metrics WHERE run_id=$1 AND step >= $2` + filter + `)
		SELECT step, key, value, ts, total FROM pts
		WHERE total <= $3 OR rn = 1 OR rn = total OR (rn - 1) % ((total + $3 - 1) / $3) = 0
		ORDER BY key, step`
	rows, err := s.DB.Query(ctx, q, args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	out, sampled := []v1.MetricPoint{}, false
	for rows.Next() {
		var p v1.MetricPoint
		var total int64
		if err := rows.Scan(&p.Step, &p.Key, &p.Value, &p.TS, &total); err != nil {
			return nil, false, err
		}
		if total > int64(maxPoints) {
			sampled = true
		}
		out = append(out, p)
	}
	return out, sampled, rows.Err()
}

// LatestMetrics returns the highest-step value of every key.
func (s *Store) LatestMetrics(ctx context.Context, runID string) (map[string]v1.MetricPoint, error) {
	rows, err := s.DB.Query(ctx, `SELECT DISTINCT ON (key) step, key, value, ts FROM run_metrics WHERE run_id=$1 ORDER BY key, step DESC`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]v1.MetricPoint{}
	for rows.Next() {
		var p v1.MetricPoint
		if err := rows.Scan(&p.Step, &p.Key, &p.Value, &p.TS); err != nil {
			return nil, err
		}
		out[p.Key] = p
	}
	return out, rows.Err()
}

func (s *Store) MetricKeys(ctx context.Context, runID string) ([]string, error) {
	rows, err := s.DB.Query(ctx, `SELECT DISTINCT key FROM run_metrics WHERE run_id=$1 ORDER BY key`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// Artifacts & logs index --------------------------------------------------

func (s *Store) UpsertArtifacts(ctx context.Context, runID string, items []v1.Artifact) error {
	for _, a := range items {
		_, err := s.DB.Exec(ctx, `INSERT INTO run_artifacts(id, run_id, kind, uri, size_bytes, sha256) VALUES ($1,$2,$3,$4,$5,$6)
			ON CONFLICT (run_id, uri) DO UPDATE SET kind=excluded.kind,
			-- A caller that knows the uri but not the file (an episode
			-- reporting its video) must not blank out a real size.
			size_bytes=CASE WHEN excluded.size_bytes > 0 THEN excluded.size_bytes ELSE run_artifacts.size_bytes END,
			sha256=CASE WHEN excluded.sha256 <> '' THEN excluded.sha256 ELSE run_artifacts.sha256 END`,
			uuid.New(), runID, a.Kind, a.URI, a.SizeBytes, a.SHA256)
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) ListArtifacts(ctx context.Context, runID string) ([]v1.Artifact, error) {
	rows, err := s.DB.Query(ctx, `SELECT id, run_id, kind, uri, size_bytes, sha256, created_at FROM run_artifacts WHERE run_id=$1 ORDER BY kind, uri`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []v1.Artifact{}
	for rows.Next() {
		var a v1.Artifact
		var id, rid uuid.UUID
		if err := rows.Scan(&id, &rid, &a.Kind, &a.URI, &a.SizeBytes, &a.SHA256, &a.CreatedAt); err != nil {
			return nil, err
		}
		a.ID, a.RunID = id.String(), rid.String()
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) AddLogIndex(ctx context.Context, runID, stream string, offset int64, uri string) {
	if _, err := s.DB.Exec(ctx, `INSERT INTO run_logs_index(run_id, stream, offset_bytes, chunk_uri) VALUES ($1,$2,$3,$4)`, runID, stream, offset, uri); err != nil {
		slog.Warn("add log index", "run", runID, "stream", stream, "err", err)
	}
}

// DeleteRun removes a run and everything cascading from it (metrics,
// artifacts, events, log index). It returns the full id that was deleted so
// the caller can clean up the run's files.
func (s *Store) DeleteRun(ctx context.Context, id string) (string, error) {
	run, err := s.GetRun(ctx, id) // resolves a unique id prefix
	if err != nil {
		return "", err
	}
	if _, err := s.DB.Exec(ctx, `DELETE FROM runs WHERE id=$1`, run.ID); err != nil {
		return "", err
	}
	return run.ID, nil
}

// RunIDSet returns every run id, for reconciling the data directory.
func (s *Store) RunIDSet(ctx context.Context) (map[string]bool, error) {
	rows, err := s.DB.Query(ctx, `SELECT id FROM runs`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// ReferencedBlobs returns the sha of every blob a run still points at: its
// dirty-work-tree patch and its code bundle.
func (s *Store) ReferencedBlobs(ctx context.Context) (map[string]bool, error) {
	rows, err := s.DB.Query(ctx, `SELECT code_patch_uri, metadata->>'code_bundle_uri' FROM runs`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var patch, bundle *string
		if err := rows.Scan(&patch, &bundle); err != nil {
			return nil, err
		}
		for _, u := range []*string{patch, bundle} {
			if u != nil && *u != "" {
				out[strings.TrimPrefix(*u, "blob://")] = true
			}
		}
	}
	return out, rows.Err()
}

// SetRunBackend records where a pool run was placed. Pool runs are created
// without a node and get one when the scheduler finds a free match.
func (s *Store) SetRunBackend(ctx context.Context, id, backendID string) error {
	_, err := s.DB.Exec(ctx, `UPDATE runs SET backend_id=$2 WHERE id=$1`, id, backendID)
	return err
}
