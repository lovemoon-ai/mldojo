package models

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
	v1 "github.com/lovemoon-ai/mldojo/proto/mldojo/v1"
)

// UpsertEpisodes replaces what is known about a run's episodes. The agent
// re-sends the whole list, because a harness usually writes its manifest
// once, at the end, and often revises earlier rows when it does.
func (s *Store) UpsertEpisodes(ctx context.Context, runID string, eps []v1.Episode) error {
	if len(eps) == 0 {
		return nil
	}
	id, err := uuid.Parse(runID)
	if err != nil {
		return UserError("run id %q is not a uuid", runID)
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	for _, e := range eps {
		extra, _ := json.Marshal(e.Extra)
		if len(extra) == 0 || string(extra) == "null" {
			extra = []byte("{}")
		}
		_, err := tx.Exec(ctx, `INSERT INTO run_episodes
			(run_id, idx, seed, success, steps, duration_ms, video_uri, extra)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
			ON CONFLICT (run_id, idx) DO UPDATE SET
			  seed = coalesce(excluded.seed, run_episodes.seed),
			  success = excluded.success,
			  steps = greatest(excluded.steps, run_episodes.steps),
			  duration_ms = greatest(excluded.duration_ms, run_episodes.duration_ms),
			  video_uri = coalesce(nullif(excluded.video_uri, ''), run_episodes.video_uri),
			  extra = run_episodes.extra || excluded.extra`,
			id, e.Index, e.Seed, e.Success, nullInt(e.Steps), nullInt64(e.DurationMS), e.VideoURI, extra)
		if err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func nullInt(v int) any {
	if v == 0 {
		return nil
	}
	return v
}

func nullInt64(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

// ListEpisodes returns a run's episodes in order.
func (s *Store) ListEpisodes(ctx context.Context, runID string) ([]v1.Episode, error) {
	id, err := uuid.Parse(runID)
	if err != nil {
		return nil, UserError("run id %q is not a uuid", runID)
	}
	rows, err := s.DB.Query(ctx, `SELECT idx, seed, success, coalesce(steps,0), coalesce(duration_ms,0),
		coalesce(video_uri,''), extra FROM run_episodes WHERE run_id=$1 ORDER BY idx`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []v1.Episode{}
	for rows.Next() {
		var e v1.Episode
		var extra []byte
		if err := rows.Scan(&e.Index, &e.Seed, &e.Success, &e.Steps, &e.DurationMS, &e.VideoURI, &extra); err != nil {
			return nil, err
		}
		if len(extra) > 0 {
			_ = json.Unmarshal(extra, &e.Extra)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// EpisodeCounts summarises several runs at once, for list views that would
// otherwise fetch every episode of every run to print one fraction.
func (s *Store) EpisodeCounts(ctx context.Context, runIDs []string) (map[string]v1.EpisodeSummary, error) {
	out := map[string]v1.EpisodeSummary{}
	if len(runIDs) == 0 {
		return out, nil
	}
	ids := make([]uuid.UUID, 0, len(runIDs))
	for _, r := range runIDs {
		if id, err := uuid.Parse(r); err == nil {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.DB.Query(ctx, `SELECT run_id, count(*), count(*) FILTER (WHERE success),
		count(*) FILTER (WHERE video_uri <> '') FROM run_episodes WHERE run_id = ANY($1) GROUP BY run_id`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var sum v1.EpisodeSummary
		if err := rows.Scan(&id, &sum.Total, &sum.Successes, &sum.WithVideo); err != nil {
			return nil, err
		}
		if sum.Total > 0 {
			sum.SuccessRate = float64(sum.Successes) / float64(sum.Total)
		}
		out[id.String()] = sum
	}
	return out, rows.Err()
}

// SeedComparison lines two evaluations up trial by trial. A rate hides the
// thing worth knowing: re-running one checkpoint on the same seeds scored
// 6/20 and then 5/20, and the question was which seeds flipped.
type SeedComparison struct {
	Seed  int64 `json:"seed"`
	Index int   `json:"index"`
	A     *bool `json:"a"`
	B     *bool `json:"b"`
}

// CompareEpisodes joins two runs on seed, falling back to episode index for
// harnesses that do not record one.
func (s *Store) CompareEpisodes(ctx context.Context, runA, runB string) ([]SeedComparison, error) {
	a, err := s.ListEpisodes(ctx, runA)
	if err != nil {
		return nil, err
	}
	b, err := s.ListEpisodes(ctx, runB)
	if err != nil {
		return nil, err
	}
	type key struct {
		seed int64
		idx  int
	}
	keyOf := func(e v1.Episode) key {
		if e.Seed != nil {
			return key{seed: *e.Seed, idx: -1}
		}
		return key{seed: -1, idx: e.Index}
	}
	merged := map[key]*SeedComparison{}
	var order []key
	add := func(eps []v1.Episode, side int) {
		for _, e := range eps {
			k := keyOf(e)
			c := merged[k]
			if c == nil {
				c = &SeedComparison{Index: e.Index}
				if e.Seed != nil {
					c.Seed = *e.Seed
				} else {
					c.Seed = -1
				}
				merged[k] = c
				order = append(order, k)
			}
			ok := e.Success
			if side == 0 {
				c.A = &ok
			} else {
				c.B = &ok
			}
		}
	}
	add(a, 0)
	add(b, 1)
	out := make([]SeedComparison, 0, len(order))
	for _, k := range order {
		out = append(out, *merged[k])
	}
	return out, nil
}
