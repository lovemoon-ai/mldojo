package promexp

import (
	"strings"
	"sync"
	"testing"
)

func TestCounterAndGaugeText(t *testing.T) {
	r := New()
	c := r.Counter("mldojo_http_requests_total", "Requests by route.", "method", "route")
	g := r.Gauge("mldojo_agents_online", "Connected agents.")
	c.With("GET", "/api/v1/runs").Inc()
	c.With("GET", "/api/v1/runs").Inc()
	c.With("POST", "/api/v1/runs").Add(3)
	g.Set(7)

	want := `# HELP mldojo_http_requests_total Requests by route.
# TYPE mldojo_http_requests_total counter
mldojo_http_requests_total{method="GET",route="/api/v1/runs"} 2
mldojo_http_requests_total{method="POST",route="/api/v1/runs"} 3
# HELP mldojo_agents_online Connected agents.
# TYPE mldojo_agents_online gauge
mldojo_agents_online 7
`
	if got := string(r.Text()); got != want {
		t.Errorf("text mismatch\n got:\n%s\nwant:\n%s", got, want)
	}
}

func TestHistogramText(t *testing.T) {
	r := New()
	h := r.Histogram("mldojo_run_dispatch_seconds", "Queued to starting.", []float64{1, 5}, "target")
	h.With("node").Observe(0.5)
	h.With("node").Observe(2)
	h.With("node").Observe(60)

	want := `# HELP mldojo_run_dispatch_seconds Queued to starting.
# TYPE mldojo_run_dispatch_seconds histogram
mldojo_run_dispatch_seconds_bucket{target="node",le="1"} 1
mldojo_run_dispatch_seconds_bucket{target="node",le="5"} 2
mldojo_run_dispatch_seconds_bucket{target="node",le="+Inf"} 3
mldojo_run_dispatch_seconds_sum{target="node"} 62.5
mldojo_run_dispatch_seconds_count{target="node"} 3
`
	if got := string(r.Text()); got != want {
		t.Errorf("text mismatch\n got:\n%s\nwant:\n%s", got, want)
	}
}

// Buckets are cumulative and the last one must equal _count, or every
// histogram_quantile() over this metric is wrong.
func TestHistogramBucketsAreCumulative(t *testing.T) {
	r := New()
	h := r.Histogram("x_seconds", "", []float64{0.1, 1, 10})
	for _, v := range []float64{0.05, 0.2, 0.3, 2, 20} {
		h.Observe(v)
	}
	lines := strings.Split(strings.TrimSpace(string(r.Text())), "\n")
	want := []string{
		`x_seconds_bucket{le="0.1"} 1`,
		`x_seconds_bucket{le="1"} 3`,
		`x_seconds_bucket{le="10"} 4`,
		`x_seconds_bucket{le="+Inf"} 5`,
		`x_seconds_sum 22.55`,
		`x_seconds_count 5`,
	}
	got := lines[1:] // skip the TYPE line (no help was given)
	if len(got) != len(want) {
		t.Fatalf("got %d lines, want %d:\n%s", len(got), len(want), strings.Join(got, "\n"))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d: got %q, want %q", i, got[i], want[i])
		}
	}
}

func TestEscaping(t *testing.T) {
	r := New()
	c := r.Counter("weird_total", "help with \\ and\nnewline", "label")
	c.With("a\"b\\c\nd").Inc()
	got := string(r.Text())
	if !strings.Contains(got, `# HELP weird_total help with \\ and\nnewline`) {
		t.Errorf("help not escaped: %q", got)
	}
	if !strings.Contains(got, `weird_total{label="a\"b\\c\nd"} 1`) {
		t.Errorf("label value not escaped: %q", got)
	}
}

func TestNumFormatting(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{0, "0"},
		{1, "1"},
		{1e6, "1000000"},                 // not 1e+06: readable, still valid
		{1234567890123, "1234567890123"}, // a byte counter must not lose digits
		{0.5, "0.5"},
		{-2.25, "-2.25"},
		{1e20, "1e+20"},
	}
	for _, c := range cases {
		if got := num(c.in); got != c.want {
			t.Errorf("num(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

// A label fed from something unbounded must not grow the registry forever.
func TestSeriesCap(t *testing.T) {
	r := New()
	c := r.Counter("capped_total", "", "id")
	for i := 0; i < maxSeries+50; i++ {
		c.With(string(rune('a'+i%26)) + string(rune(i))).Inc()
	}
	lines := strings.Count(string(r.Text()), "\n") - 1 // minus the TYPE line
	if lines > maxSeries+1 {
		t.Errorf("got %d series, want at most %d", lines, maxSeries+1)
	}
	if !strings.Contains(string(r.Text()), `capped_total{id="other"}`) {
		t.Error("overflowing series should collapse into id=\"other\"")
	}
}

func TestEmptyFamilyIsOmitted(t *testing.T) {
	r := New()
	r.Counter("never_used_total", "nothing here", "a")
	if got := string(r.Text()); got != "" {
		t.Errorf("a metric with no series should render nothing, got %q", got)
	}
}

func TestConcurrentUse(t *testing.T) {
	r := New()
	c := r.Counter("hits_total", "", "route")
	h := r.Histogram("dur_seconds", "", nil, "route")
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				c.With("/a").Inc()
				h.With("/a").Observe(float64(j) / 1000)
				_ = r.Text()
			}
		}(i)
	}
	wg.Wait()
	if !strings.Contains(string(r.Text()), `hits_total{route="/a"} 3200`) {
		t.Errorf("lost increments:\n%s", r.Text())
	}
}

func TestDuplicateRegistrationPanics(t *testing.T) {
	r := New()
	r.Gauge("dup", "")
	defer func() {
		if recover() == nil {
			t.Error("registering the same name twice should panic")
		}
	}()
	r.Gauge("dup", "")
}

func TestWrongLabelCountPanics(t *testing.T) {
	r := New()
	c := r.Counter("two_total", "", "a", "b")
	defer func() {
		if recover() == nil {
			t.Error("a wrong number of label values should panic")
		}
	}()
	c.With("only-one").Inc()
}
