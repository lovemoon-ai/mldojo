package handlers

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/lovemoon-ai/mldojo/api/internal/backends"
	"github.com/lovemoon-ai/mldojo/api/internal/obs"
)

// sampleRE is the text exposition grammar for one sample line:
// name{label="value",...} number
var sampleRE = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*(\{[a-zA-Z_][a-zA-Z0-9_]*="([^"\\]|\\.)*"(,[a-zA-Z_][a-zA-Z0-9_]*="([^"\\]|\\.)*")*\})? \S+$`)

// checkExposition fails the test unless body is something Prometheus would
// accept: every line is a HELP/TYPE comment or a well-formed sample, every
// sample has a declared type, and every value parses as a float.
func checkExposition(t *testing.T, body string) map[string]string {
	t.Helper()
	types := map[string]string{}
	seenHelp := map[string]bool{}
	for i, line := range strings.Split(strings.TrimRight(body, "\n"), "\n") {
		where := "line " + strconv.Itoa(i+1) + ": " + line
		switch {
		case strings.HasPrefix(line, "# HELP "):
			name, _, _ := strings.Cut(strings.TrimPrefix(line, "# HELP "), " ")
			if seenHelp[name] {
				t.Errorf("%s: HELP repeated for %s", where, name)
			}
			seenHelp[name] = true
		case strings.HasPrefix(line, "# TYPE "):
			name, typ, ok := strings.Cut(strings.TrimPrefix(line, "# TYPE "), " ")
			if !ok {
				t.Errorf("%s: malformed TYPE", where)
				continue
			}
			switch typ {
			case "counter", "gauge", "histogram", "summary", "untyped":
			default:
				t.Errorf("%s: unknown metric type %q", where, typ)
			}
			if _, dup := types[name]; dup {
				t.Errorf("%s: TYPE repeated for %s", where, name)
			}
			types[name] = typ
		default:
			if !sampleRE.MatchString(line) {
				t.Errorf("%s: not a valid sample line", where)
				continue
			}
			name, value, _ := strings.Cut(line, " ")
			if i := strings.IndexByte(name, '{'); i > 0 {
				name = name[:i]
			}
			if _, err := strconv.ParseFloat(value, 64); err != nil {
				t.Errorf("%s: value is not a number", where)
			}
			base := name
			for _, suffix := range []string{"_bucket", "_sum", "_count"} {
				if s, ok := strings.CutSuffix(name, suffix); ok && types[s] == "histogram" {
					base = s
				}
			}
			if _, ok := types[base]; !ok {
				t.Errorf("%s: sample before its TYPE line", where)
			}
		}
	}
	return types
}

func TestMetricsExposition(t *testing.T) {
	// No store and no hub: the process-level collectors must still work, and
	// the ones that need a database must simply be skipped.
	s := &Server{}
	rec := httptest.NewRecorder()
	if err := s.metrics(rec, httptest.NewRequest("GET", "/api/v1/metrics", nil)); err != nil {
		t.Fatalf("metrics: %v", err)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, Prometheus wants text/plain", ct)
	}
	body := rec.Body.String()
	types := checkExposition(t, body)

	for name, want := range map[string]string{
		"mldojo_build_info":       "gauge",
		"mldojo_uptime_seconds":   "gauge",
		"mldojo_goroutines":       "gauge",
		"mldojo_memory_sys_bytes": "gauge",
	} {
		if types[name] != want {
			t.Errorf("%s: type %q, want %q", name, types[name], want)
		}
	}
	if !strings.Contains(body, `mldojo_build_info{version=`) {
		t.Error("build info should carry the version as a label")
	}
}

func TestMetricsCoversTheDocumentedNames(t *testing.T) {
	// Exercise the metrics that are only created on first use, so the
	// exposition contains the full set docs/api.md promises.
	obs.HTTPRequests.With("GET", "/api/v1/runs", "200").Inc()
	obs.HTTPLatency.With("GET", "/api/v1/runs").Observe(0.01)
	obs.RunsFinished.With("failed").Inc()
	obs.RunDispatch.With("node").Observe(2)
	obs.Runs.With("running").Set(1)
	obs.Nodes.With("online").Set(1)
	obs.GPUsTotal.Set(8)
	obs.GPUsBusy.Set(2)
	obs.AgentsOnline.Set(1)
	obs.DBPoolConns.With("idle").Set(1)
	obs.DataDirBytes.Set(1 << 30)
	obs.SecretsUnlocked.Set(1)
	obs.Alerts.With(obs.EventRunFailed, "sent").Inc()

	s := &Server{}
	rec := httptest.NewRecorder()
	if err := s.metrics(rec, httptest.NewRequest("GET", "/api/v1/metrics", nil)); err != nil {
		t.Fatalf("metrics: %v", err)
	}
	body := rec.Body.String()
	checkExposition(t, body)
	for _, want := range []string{
		"mldojo_runs{status=", "mldojo_runs_finished_total{status=",
		"mldojo_run_dispatch_seconds_bucket{backend=", "mldojo_agents_online",
		"mldojo_nodes{agent_status=", "mldojo_gpus_total", "mldojo_gpus_busy",
		"mldojo_http_requests_total{method=", "mldojo_http_request_duration_seconds_bucket{",
		"mldojo_http_requests_in_flight", "mldojo_db_pool_conns{state=",
		"mldojo_data_dir_bytes", "mldojo_secrets_unlocked", "mldojo_alerts_total{event=",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %s in:\n%s", want, body)
		}
	}
}

// The route label must be the mux pattern. One label per run id would make
// every scrape carry the whole run table.
func TestRoutePatternIsNotAnID(t *testing.T) {
	mux := http.NewServeMux()
	h := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	mux.Handle("GET /api/v1/runs/{id}", h)
	mux.Handle("GET /api/v1/runs", h)
	mux.Handle("GET /api/v1/queues/{id...}", h)
	mux.Handle("/", h)

	cases := []struct{ path, want string }{
		{"/api/v1/runs/9d3f2c1a-0000-4000-8000-000000000000", "/api/v1/runs/{id}"},
		{"/api/v1/runs", "/api/v1/runs"},
		{"/api/v1/queues/myqueue/gpu-h100", "/api/v1/queues/{id...}"},
		{"/anything/else", "/"},
	}
	for _, c := range cases {
		got := routePattern(mux, httptest.NewRequest("GET", c.path, nil))
		if got != c.want {
			t.Errorf("routePattern(%s) = %q, want %q", c.path, got, c.want)
		}
	}
}

func TestHTTPMetricsAreRecorded(t *testing.T) {
	mux := http.NewServeMux()
	mux.Handle("GET /api/v1/runs/{id}", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
	}))
	before := obs.HTTPRequests.With("GET", "/api/v1/runs/{id}", "404").Get()
	h := logRequests(mux, mux)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/v1/runs/abc", nil))

	if got := obs.HTTPRequests.With("GET", "/api/v1/runs/{id}", "404").Get(); got != before+1 {
		t.Errorf("request counter = %v, want %v", got, before+1)
	}
	if got := obs.HTTPInFlight.With().Get(); got != 0 {
		t.Errorf("in-flight gauge left at %v after the request finished", got)
	}
}

func TestMetricsEndpointAuth(t *testing.T) {
	node := backends.NewNodeBackend(&backends.Runtime{})
	for _, public := range []bool{false, true} {
		s := &Server{Node: node, MetricsPublic: public}
		rec := httptest.NewRecorder()
		s.Routes().ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/metrics", nil))
		want := 401
		if public {
			want = 200
		}
		if rec.Code != want {
			t.Errorf("metrics_public=%v: status %d, want %d", public, rec.Code, want)
		}
	}
}
