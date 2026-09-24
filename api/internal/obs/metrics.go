// Package obs is what MLDojo reports about itself: the Prometheus metrics
// every part of the server writes into (rendered by GET /api/v1/metrics) and
// the alert notifier that tells a human when something breaks.
//
// The metrics are package-level on purpose. They are written from the HTTP
// middleware, the backends and the hub, and threading a registry through all
// of them would be more plumbing than the values are worth -- the same
// trade-off slog.Default() makes.
package obs

import (
	"github.com/lovemoon-ai/mldojo/api/internal/promexp"
	"github.com/lovemoon-ai/mldojo/internal/version"
)

// R is the registry GET /api/v1/metrics renders.
var R = promexp.New()

// Process and build.
var (
	BuildInfo  = R.Gauge("mldojo_build_info", "Always 1; the build is in the label.", "version")
	Uptime     = R.Gauge("mldojo_uptime_seconds", "Seconds since this API process started.")
	Goroutines = R.Gauge("mldojo_goroutines", "Goroutines in the API process.")
	MemAlloc   = R.Gauge("mldojo_memory_alloc_bytes", "Heap bytes currently allocated.")
	MemSys     = R.Gauge("mldojo_memory_sys_bytes", "Bytes obtained from the OS.")
)

// Runs and scheduling.
var (
	Runs = R.Gauge("mldojo_runs", "Runs in the database, by status.", "status")
	// RunsFinished counts transitions, not rows: it survives run deletion
	// and is what a failure-rate alert should be built on.
	RunsFinished = R.Counter("mldojo_runs_finished_total", "Runs that reached a terminal status since this process started.", "status")
	RunDispatch  = R.Histogram("mldojo_run_dispatch_seconds",
		"Seconds from a run being queued to it starting.",
		[]float64{0.5, 1, 2, 5, 10, 30, 60, 300, 900, 3600}, "backend")
)

// Nodes, agents and GPUs.
var (
	AgentsOnline = R.Gauge("mldojo_agents_online", "Agents with a live connection to this API.")
	Nodes        = R.Gauge("mldojo_nodes", "Registered nodes, by agent status.", "agent_status")
	GPUsTotal    = R.Gauge("mldojo_gpus_total", "GPUs reported by connected agents.")
	GPUsBusy     = R.Gauge("mldojo_gpus_busy", "GPUs with at least one MLDojo run assigned to them.")
)

// HTTP. route is the mux pattern ("/api/v1/runs/{id}"), never a concrete id:
// one label per run would turn the registry into a run index.
var (
	HTTPRequests = R.Counter("mldojo_http_requests_total", "HTTP requests to /api/, by route and status.", "method", "route", "status")
	HTTPLatency  = R.Histogram("mldojo_http_request_duration_seconds",
		"Request duration, excluding WebSocket and agent streaming endpoints.", nil, "method", "route")
	HTTPInFlight = R.Gauge("mldojo_http_requests_in_flight", "HTTP requests to /api/ being served right now.")
)

// Database pool (pgxpool.Stat).
var (
	DBPoolConns    = R.Gauge("mldojo_db_pool_conns", "Connections in the pool, by state.", "state")
	DBPoolMax      = R.Gauge("mldojo_db_pool_max_conns", "Configured maximum pool size.")
	DBAcquires     = R.Counter("mldojo_db_pool_acquires_total", "Connections acquired from the pool.")
	DBEmptyAcquire = R.Counter("mldojo_db_pool_empty_acquires_total", "Acquires that had to wait for a free connection.")
	DBAcquireWait  = R.Counter("mldojo_db_pool_acquire_wait_seconds_total", "Total time spent waiting for a connection.")
)

// Storage, secrets and alerting.
var (
	DataDirBytes    = R.Gauge("mldojo_data_dir_bytes", "Bytes under api.data_dir (logs, blobs, caches), sampled periodically.")
	SecretsUnlocked = R.Gauge("mldojo_secrets_unlocked", "1 when the secrets manager holds the master key.")
	Alerts          = R.Counter("mldojo_alerts_total", "Alert events, by outcome (sent, suppressed, failed).", "event", "outcome")
)

func init() {
	BuildInfo.With(version.Version).Set(1)
	// Give every unlabelled metric a zero series up front. A metric that
	// only appears after the first event makes alerting rules fragile:
	// absent() and rate() behave differently from a flat zero.
	for _, m := range []*promexp.Vec{Uptime, Goroutines, MemAlloc, MemSys,
		AgentsOnline, GPUsTotal, GPUsBusy, HTTPInFlight,
		DBPoolMax, DBAcquires, DBEmptyAcquire, DBAcquireWait,
		DataDirBytes, SecretsUnlocked} {
		m.Set(0)
	}
}
