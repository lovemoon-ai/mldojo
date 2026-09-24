package handlers

import (
	"context"
	"log/slog"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lovemoon-ai/mldojo/api/internal/obs"
	"github.com/lovemoon-ai/mldojo/api/internal/promexp"
	v1 "github.com/lovemoon-ai/mldojo/proto/mldojo/v1"
)

// dbCountTTL bounds how often a scrape may count rows. Prometheus scrapes
// every 15s by default, and nothing stops a second scraper from pointing at
// the same endpoint; the counts must not turn into database load.
const dbCountTTL = 15 * time.Second

// gaugeCache remembers when the row counts were last refreshed.
type gaugeCache struct {
	mu sync.Mutex
	at time.Time
}

var processStart = time.Now()

// metrics renders the registry in the Prometheus text format. Who may call
// it is decided in Routes: administrators, or anyone when
// api.metrics_public is set (scrapers do not carry credentials).
func (s *Server) metrics(w http.ResponseWriter, r *http.Request) error {
	s.collect(r.Context())
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	return obs.R.Write(w)
}

// collect refreshes the values that are cheaper to read at scrape time than
// to maintain on every change. Counters and histograms are not here: those
// are incremented where the events happen.
func (s *Server) collect(ctx context.Context) {
	obs.Uptime.Set(time.Since(processStart).Seconds())
	obs.Goroutines.Set(float64(runtime.NumGoroutine()))
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	obs.MemAlloc.Set(float64(m.Alloc))
	obs.MemSys.Set(float64(m.Sys))

	if s.Node != nil && s.Node.Hub != nil {
		obs.AgentsOnline.Set(float64(s.Node.Hub.Online()))
		total, busy := s.Node.Hub.GPUCounts()
		obs.GPUsTotal.Set(float64(total))
		obs.GPUsBusy.Set(float64(busy))
	}
	if s.RT != nil && s.RT.Secrets != nil {
		obs.SecretsUnlocked.Set(boolGauge(s.RT.Secrets.Status().Unlocked))
	}
	if s.RT == nil || s.RT.Store == nil || s.RT.Store.DB == nil {
		return
	}
	st := s.RT.Store.DB.Stat()
	obs.DBPoolConns.With("acquired").Set(float64(st.AcquiredConns()))
	obs.DBPoolConns.With("idle").Set(float64(st.IdleConns()))
	obs.DBPoolConns.With("total").Set(float64(st.TotalConns()))
	obs.DBPoolMax.Set(float64(st.MaxConns()))
	obs.DBAcquires.Set(float64(st.AcquireCount()))
	obs.DBEmptyAcquire.Set(float64(st.EmptyAcquireCount()))
	obs.DBAcquireWait.Set(st.AcquireDuration().Seconds())
	s.collectCounts(ctx)
}

// collectCounts refreshes the run and node gauges from the database, at most
// once per dbCountTTL. On error the previous values stay: a stale number is
// more useful than a zero that looks like an outage.
func (s *Server) collectCounts(ctx context.Context) {
	s.gauges.mu.Lock()
	defer s.gauges.mu.Unlock()
	if time.Since(s.gauges.at) < dbCountTTL {
		return
	}
	s.gauges.at = time.Now()
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	db := s.store().DB
	if counts, err := groupCount(ctx, db, `SELECT status, count(*) FROM runs GROUP BY status`); err != nil {
		slog.Debug("metrics: count runs", "err", err)
	} else {
		setGroup(obs.Runs, counts, []string{v1.PhaseQueued, v1.PhaseStarting, v1.PhaseRunning,
			v1.PhaseSucceeded, v1.PhaseFailed, v1.PhaseCancelled})
	}
	if counts, err := groupCount(ctx, db, `SELECT agent_status, count(*) FROM nodes GROUP BY agent_status`); err != nil {
		slog.Debug("metrics: count nodes", "err", err)
	} else {
		setGroup(obs.Nodes, counts, []string{"online", "offline"})
	}
}

func groupCount(ctx context.Context, db *pgxpool.Pool, q string) (map[string]int64, error) {
	rows, err := db.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var k string
		var n int64
		if err := rows.Scan(&k, &n); err != nil {
			return nil, err
		}
		out[k] = n
	}
	return out, rows.Err()
}

// setGroup writes one series per label value, including an explicit zero for
// the known values that returned no rows -- a status that drops off the
// result set must fall to 0, not keep its last value forever.
func setGroup(g *promexp.Vec, counts map[string]int64, known []string) {
	for _, k := range known {
		g.With(k).Set(float64(counts[k]))
	}
	for k, n := range counts {
		g.With(k).Set(float64(n))
	}
}

func boolGauge(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// routePattern is the mux pattern a request matched ("GET /api/v1/runs/{id}"),
// reduced to the path part. Using the pattern and not the URL is what keeps
// the HTTP metrics at a few dozen series instead of one per run id.
func routePattern(mux *http.ServeMux, r *http.Request) string {
	_, pattern := mux.Handler(r)
	if i := strings.IndexByte(pattern, '/'); i >= 0 {
		pattern = pattern[i:]
	}
	if pattern == "" {
		return "other"
	}
	return pattern
}
