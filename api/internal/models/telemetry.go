package models

import (
	"context"
	"log/slog"
	"strconv"
	"time"

	"github.com/google/uuid"
	v1 "github.com/lovemoon-ai/mldojo/proto/mldojo/v1"
)

// Telemetry ---------------------------------------------------------------
//
// Heartbeats arrive every few seconds. Storing every one of them for every
// card would outgrow the runs themselves, so samples are written at a coarser
// cadence (see obs.Sampler) and aggregated on read.

// InsertGPUSamples records one heartbeat's worth of GPU readings.
func (s *Store) InsertGPUSamples(ctx context.Context, nodeID string, ts time.Time, stats []v1.GPUStat) error {
	if len(stats) == 0 {
		return nil
	}
	rows := make([][]any, 0, len(stats))
	for _, g := range stats {
		var runID *uuid.UUID
		if len(g.RunIDs) > 0 {
			if id, err := uuid.Parse(g.RunIDs[0]); err == nil {
				runID = &id
			}
		}
		procs, mem := g.Foreign()
		rows = append(rows, []any{nodeID, g.Index, ts, g.Util, g.MemUsedMB, g.MemTotMB, g.Temp, runID, mem, procs})
	}
	for _, r := range rows {
		_, err := s.DB.Exec(ctx, `INSERT INTO node_gpu_samples
			(node_id, gpu_index, ts, util, mem_used_mb, mem_total_mb, temp, run_id, foreign_mem_mb, foreign_procs)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) ON CONFLICT DO NOTHING`, r...)
		if err != nil {
			return err
		}
	}
	return nil
}

// InsertDiskSamples records one heartbeat's worth of filesystem readings.
func (s *Store) InsertDiskSamples(ctx context.Context, nodeID string, ts time.Time, disks []v1.DiskStat) error {
	for _, d := range disks {
		_, err := s.DB.Exec(ctx, `INSERT INTO node_disk_samples (node_id, path, ts, total_gb, free_gb)
			VALUES ($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, nodeID, d.Path, ts, d.TotalGB, d.FreeGB)
		if err != nil {
			return err
		}
	}
	return nil
}

// GPUSeries is one card's history over a window.
type GPUSeries struct {
	GPUIndex int                 `json:"gpu_index"`
	Points   []v1.GPUSamplePoint `json:"points"`
}

// GPUHistoryQuery selects which samples to read back.
type GPUHistoryQuery struct {
	NodeID    string
	RunID     string
	Since     time.Time
	Until     time.Time
	MaxPoints int // per card; 0 means 500
}

// GPUHistory reads samples back, bucketed so a long window returns a usable
// number of points instead of a week of heartbeats.
func (s *Store) GPUHistory(ctx context.Context, q GPUHistoryQuery) ([]GPUSeries, error) {
	if q.MaxPoints <= 0 {
		q.MaxPoints = 500
	}
	if q.Until.IsZero() {
		q.Until = time.Now()
	}
	if q.Since.IsZero() {
		q.Since = q.Until.Add(-6 * time.Hour)
	}
	// One bucket per output point, so the caller gets what it asked for
	// whether the window is ten minutes or ten days.
	span := q.Until.Sub(q.Since)
	if span <= 0 {
		span = time.Minute
	}
	bucket := span / time.Duration(q.MaxPoints)
	if bucket < time.Second {
		bucket = time.Second
	}

	where := "ts >= $1 AND ts <= $2"
	args := []any{q.Since, q.Until}
	if q.NodeID != "" {
		args = append(args, q.NodeID)
		where += " AND node_id = $3"
	} else if q.RunID != "" {
		id, err := uuid.Parse(q.RunID)
		if err != nil {
			return nil, UserError("run id %q is not a uuid", q.RunID)
		}
		args = append(args, id)
		where += " AND run_id = $3"
	}
	args = append(args, bucket.Seconds())
	n := strconv.Itoa(len(args))

	rows, err := s.DB.Query(ctx, `
		SELECT gpu_index,
		       to_timestamp(floor(extract(epoch FROM ts) / $`+n+`) * $`+n+`) AS b,
		       avg(util), avg(mem_used_mb), max(mem_total_mb), avg(temp),
		       max(foreign_mem_mb), bool_or(run_id IS NOT NULL)
		FROM node_gpu_samples
		WHERE `+where+`
		GROUP BY gpu_index, b
		ORDER BY gpu_index, b`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	byIdx := map[int]*GPUSeries{}
	var order []int
	for rows.Next() {
		var idx int
		var p v1.GPUSamplePoint
		var mine bool
		if err := rows.Scan(&idx, &p.TS, &p.Util, &p.MemUsedMB, &p.MemTotMB, &p.Temp, &p.ForeignMemMB, &mine); err != nil {
			return nil, err
		}
		p.Mine = mine
		if byIdx[idx] == nil {
			byIdx[idx] = &GPUSeries{GPUIndex: idx}
			order = append(order, idx)
		}
		byIdx[idx].Points = append(byIdx[idx].Points, p)
	}
	out := make([]GPUSeries, 0, len(order))
	for _, i := range order {
		out = append(out, *byIdx[i])
	}
	return out, rows.Err()
}

// GPUHours totals allocated card-time per project, per run or per submitter.
// The ingredients were always there -- started_at, finished_at, the assigned
// indices -- but nothing ever added them up.
type UsageRow struct {
	Key      string  `json:"key"`
	GPUHours float64 `json:"gpu_hours"`
	// BusyHours is the part of that time the cards were actually working.
	// The gap between the two is what a cluster wastes.
	BusyHours float64 `json:"busy_hours"`
	Samples   int     `json:"samples"`
}

// Usage aggregates GPU samples by project, run or submitter over a window.
func (s *Store) Usage(ctx context.Context, by string, since, until time.Time) ([]UsageRow, error) {
	col := map[string]string{
		"project":   "p.name",
		"run":       "r.id::text",
		"submitter": "coalesce(r.metadata->>'submitter', 'unknown')",
		"node":      "g.node_id",
	}[by]
	if col == "" {
		return nil, UserError("group by must be project|run|submitter|node (got %q)", by)
	}
	if until.IsZero() {
		until = time.Now()
	}
	if since.IsZero() {
		since = until.Add(-7 * 24 * time.Hour)
	}
	// Each sample stands for the interval until the next one. Using the
	// median gap rather than a constant keeps the total honest when the
	// sampler's cadence changes.
	rows, err := s.DB.Query(ctx, `
		WITH gap AS (
		  SELECT coalesce(percentile_cont(0.5) WITHIN GROUP (ORDER BY d), 60) AS secs
		  FROM (SELECT extract(epoch FROM ts - lag(ts) OVER (PARTITION BY node_id, gpu_index ORDER BY ts)) AS d
		        FROM node_gpu_samples WHERE ts >= $1 AND ts <= $2) x
		  WHERE d > 0 AND d < 3600
		)
		SELECT `+col+` AS k,
		       count(*) * (SELECT secs FROM gap) / 3600.0,
		       sum(CASE WHEN g.util >= 10 THEN 1 ELSE 0 END) * (SELECT secs FROM gap) / 3600.0,
		       count(*)
		FROM node_gpu_samples g
		LEFT JOIN runs r ON g.run_id = r.id
		LEFT JOIN experiments e ON r.experiment_id = e.id
		LEFT JOIN projects p ON e.project_id = p.id
		WHERE g.ts >= $1 AND g.ts <= $2 AND `+notNull(by)+`
		GROUP BY k
		ORDER BY 2 DESC`, since, until)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UsageRow
	for rows.Next() {
		var u UsageRow
		if err := rows.Scan(&u.Key, &u.GPUHours, &u.BusyHours, &u.Samples); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// notNull keeps rows a grouping cannot attribute out of that grouping: a
// card nobody of ours was on has no project, and counting it as "unknown
// project" would be a lie. Grouping by node is the exception -- every sample
// has one.
func notNull(by string) string {
	if by == "node" {
		return "true"
	}
	return "g.run_id IS NOT NULL"
}

// IdleGPUs finds cards an MLDojo run held without using, over a window.
// This is the waste nobody could see: allocated, paid for, at 0%.
func (s *Store) IdleGPUs(ctx context.Context, since time.Time, minHours float64) ([]v1.IdleGPU, error) {
	if since.IsZero() {
		since = time.Now().Add(-24 * time.Hour)
	}
	rows, err := s.DB.Query(ctx, `
		WITH gap AS (
		  SELECT coalesce(percentile_cont(0.5) WITHIN GROUP (ORDER BY d), 60) AS secs
		  FROM (SELECT extract(epoch FROM ts - lag(ts) OVER (PARTITION BY node_id, gpu_index ORDER BY ts)) AS d
		        FROM node_gpu_samples WHERE ts >= $1) x
		  WHERE d > 0 AND d < 3600
		)
		SELECT g.node_id, g.gpu_index, g.run_id::text, p.name,
		       count(*) * (SELECT secs FROM gap) / 3600.0 AS held_h,
		       sum(CASE WHEN g.util >= 10 THEN 1 ELSE 0 END) * (SELECT secs FROM gap) / 3600.0 AS busy_h,
		       avg(g.util), max(g.ts)
		FROM node_gpu_samples g
		JOIN runs r ON g.run_id = r.id
		LEFT JOIN experiments e ON r.experiment_id = e.id
		LEFT JOIN projects p ON e.project_id = p.id
		WHERE g.ts >= $1 AND g.run_id IS NOT NULL
		GROUP BY g.node_id, g.gpu_index, g.run_id, p.name
		HAVING count(*) * (SELECT secs FROM gap) / 3600.0 >= $2
		   AND avg(g.util) < 10
		ORDER BY held_h DESC`, since, minHours)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []v1.IdleGPU{}
	for rows.Next() {
		var g v1.IdleGPU
		var project *string
		if err := rows.Scan(&g.NodeID, &g.GPUIndex, &g.RunID, &project, &g.HeldHours, &g.BusyHours, &g.AvgUtil, &g.LastSeen); err != nil {
			return nil, err
		}
		if project != nil {
			g.Project = *project
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// PruneTelemetry drops samples older than the retention window. Without it
// this table is the one thing in MLDojo that grows without bound.
func (s *Store) PruneTelemetry(ctx context.Context, keep time.Duration) (int64, error) {
	cutoff := time.Now().Add(-keep)
	g, err := s.DB.Exec(ctx, `DELETE FROM node_gpu_samples WHERE ts < $1`, cutoff)
	if err != nil {
		return 0, err
	}
	d, err := s.DB.Exec(ctx, `DELETE FROM node_disk_samples WHERE ts < $1`, cutoff)
	if err != nil {
		return g.RowsAffected(), err
	}
	return g.RowsAffected() + d.RowsAffected(), nil
}

// WithNodeLock runs f while holding a Postgres advisory lock for a node.
//
// Admission is a read followed by a write -- count what is on the node, then
// mark this run starting -- and four runs submitted together did all four
// reads before any of the writes landed, so a node limited to one run
// started three. The lock makes that pair atomic across API processes.
// It is held only across the decision, not the spawn that follows.
func (s *Store) WithNodeLock(ctx context.Context, nodeID string, f func(context.Context) error) error {
	conn, err := s.DB.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	key := "mldojo/node/" + nodeID
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(hashtext($1))`, key); err != nil {
		return err
	}
	defer func() {
		// Releasing must not be skipped because the caller's context died.
		if _, err := conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock(hashtext($1))`, key); err != nil {
			slog.Warn("release node lock", "node", nodeID, "err", err)
		}
	}()
	return f(ctx)
}
