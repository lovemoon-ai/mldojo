// Package promexp is a minimal Prometheus exposition registry: counters,
// gauges and histograms with labels, rendered in the text format
// (https://prometheus.io/docs/instrumenting/exposition_formats/).
//
// It exists instead of the upstream client library because that library
// pulls in a handful of modules to produce the ~60 lines of formatting
// below, and this repository keeps its dependency list short enough to audit.
package promexp

import (
	"bytes"
	"fmt"
	"io"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// Metric types, as written in the "# TYPE" line.
const (
	TypeCounter   = "counter"
	TypeGauge     = "gauge"
	TypeHistogram = "histogram"
)

// DefBuckets are latency buckets in seconds, for request-shaped work.
var DefBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

// maxSeries caps one metric's label combinations. A label whose values come
// from user input (a run id, a path) would otherwise grow the registry
// without bound; past the cap everything lands in a single "other" series so
// the damage is one extra line, not an ever-growing map.
const maxSeries = 2000

var nameRE = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// Registry holds metric families and renders them. It is safe for
// concurrent use; the zero value is not usable, call New.
type Registry struct {
	mu     sync.Mutex
	order  []*family
	byName map[string]*family
}

func New() *Registry { return &Registry{byName: map[string]*family{}} }

type family struct {
	name    string
	help    string
	typ     string
	labels  []string
	buckets []float64

	mu     sync.Mutex
	series map[string]*entry
}

type entry struct {
	labels []string // values, positional to family.labels
	val    Value
	hist   *Hist
}

func (r *Registry) register(typ, name, help string, buckets []float64, labels []string) *family {
	if !nameRE.MatchString(name) {
		panic("promexp: invalid metric name " + name)
	}
	for _, l := range labels {
		if !nameRE.MatchString(l) {
			panic("promexp: invalid label name " + l + " on " + name)
		}
	}
	f := &family{name: name, help: help, typ: typ, labels: labels, buckets: buckets,
		series: map[string]*entry{}}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.byName[name]; dup {
		panic("promexp: metric registered twice: " + name)
	}
	r.byName[name] = f
	r.order = append(r.order, f)
	return f
}

// Counter registers a monotonic counter. The name should end in _total.
func (r *Registry) Counter(name, help string, labels ...string) *Vec {
	return &Vec{f: r.register(TypeCounter, name, help, nil, labels)}
}

// Gauge registers a value that goes up and down.
func (r *Registry) Gauge(name, help string, labels ...string) *Vec {
	return &Vec{f: r.register(TypeGauge, name, help, nil, labels)}
}

// Histogram registers cumulative buckets plus _sum and _count. Buckets are
// upper bounds in ascending order; nil means DefBuckets.
func (r *Registry) Histogram(name, help string, buckets []float64, labels ...string) *HistVec {
	if len(buckets) == 0 {
		buckets = DefBuckets
	}
	b := append([]float64(nil), buckets...)
	sort.Float64s(b)
	return &HistVec{f: r.register(TypeHistogram, name, help, b, labels)}
}

// lookup finds or creates the series for a set of label values.
func (f *family) lookup(vals []string) *entry {
	if len(vals) != len(f.labels) {
		panic(fmt.Sprintf("promexp: %s wants %d label values, got %d", f.name, len(f.labels), len(vals)))
	}
	key := strings.Join(vals, "\xff")
	f.mu.Lock()
	defer f.mu.Unlock()
	if e := f.series[key]; e != nil {
		return e
	}
	if len(f.series) >= maxSeries {
		vals, key = overflow(len(f.labels)), "\x00overflow"
		if e := f.series[key]; e != nil {
			return e
		}
	}
	e := &entry{labels: append([]string(nil), vals...)}
	if f.typ == TypeHistogram {
		e.hist = &Hist{bounds: f.buckets, counts: make([]uint64, len(f.buckets))}
	}
	f.series[key] = e
	return e
}

func overflow(n int) []string {
	v := make([]string, n)
	for i := range v {
		v[i] = "other"
	}
	return v
}

// Vec is a counter or gauge, possibly with labels.
type Vec struct{ f *family }

// With selects the series for these label values (in declaration order).
func (v *Vec) With(labelValues ...string) *Value { return &v.f.lookup(labelValues).val }

// Inc, Add and Set act on the unlabelled series of a metric declared
// without labels.
func (v *Vec) Inc()          { v.With().Inc() }
func (v *Vec) Add(x float64) { v.With().Add(x) }
func (v *Vec) Set(x float64) { v.With().Set(x) }

// Value is one counter or gauge series.
type Value struct{ bits atomic.Uint64 }

func (s *Value) Set(x float64) { s.bits.Store(math.Float64bits(x)) }
func (s *Value) Get() float64  { return math.Float64frombits(s.bits.Load()) }
func (s *Value) Inc()          { s.Add(1) }

func (s *Value) Add(x float64) {
	for {
		old := s.bits.Load()
		if s.bits.CompareAndSwap(old, math.Float64bits(math.Float64frombits(old)+x)) {
			return
		}
	}
}

// HistVec is a histogram, possibly with labels.
type HistVec struct{ f *family }

func (h *HistVec) With(labelValues ...string) *Hist { return h.f.lookup(labelValues).hist }

// Observe records one sample on the unlabelled series.
func (h *HistVec) Observe(x float64) { h.With().Observe(x) }

// Hist is one histogram series. counts are cumulative per bucket bound.
type Hist struct {
	bounds []float64 // shared with the family, read-only after registration
	mu     sync.Mutex
	counts []uint64
	sum    float64
	count  uint64
}

// Observe records one sample.
func (h *Hist) Observe(x float64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.count++
	h.sum += x
	for i, b := range h.bounds {
		if x <= b {
			h.counts[i]++
		}
	}
}

// Text renders the whole registry in the Prometheus text format.
func (r *Registry) Text() []byte {
	r.mu.Lock()
	fams := append([]*family(nil), r.order...)
	r.mu.Unlock()
	var b bytes.Buffer
	for _, f := range fams {
		f.writeTo(&b)
	}
	return b.Bytes()
}

// Write renders the registry into w.
func (r *Registry) Write(w io.Writer) error {
	_, err := w.Write(r.Text())
	return err
}

func (f *family) writeTo(b *bytes.Buffer) {
	f.mu.Lock()
	es := make([]*entry, 0, len(f.series))
	for _, e := range f.series {
		es = append(es, e)
	}
	f.mu.Unlock()
	if len(es) == 0 {
		return
	}
	// Stable output: scrapes diff cleanly and tests can compare strings.
	sort.Slice(es, func(i, j int) bool {
		return strings.Join(es[i].labels, "\xff") < strings.Join(es[j].labels, "\xff")
	})
	if f.help != "" {
		fmt.Fprintf(b, "# HELP %s %s\n", f.name, escapeHelp(f.help))
	}
	fmt.Fprintf(b, "# TYPE %s %s\n", f.name, f.typ)
	for _, e := range es {
		if f.typ != TypeHistogram {
			b.WriteString(f.name)
			b.WriteString(f.labelSet(e.labels, "", ""))
			b.WriteByte(' ')
			b.WriteString(num(e.val.Get()))
			b.WriteByte('\n')
			continue
		}
		e.hist.mu.Lock()
		counts, sum, count := append([]uint64(nil), e.hist.counts...), e.hist.sum, e.hist.count
		e.hist.mu.Unlock()
		for i, bound := range f.buckets {
			fmt.Fprintf(b, "%s_bucket%s %d\n", f.name, f.labelSet(e.labels, "le", num(bound)), counts[i])
		}
		fmt.Fprintf(b, "%s_bucket%s %d\n", f.name, f.labelSet(e.labels, "le", "+Inf"), count)
		fmt.Fprintf(b, "%s_sum%s %s\n", f.name, f.labelSet(e.labels, "", ""), num(sum))
		fmt.Fprintf(b, "%s_count%s %d\n", f.name, f.labelSet(e.labels, "", ""), count)
	}
}

// labelSet renders {a="1",b="2"}, optionally with one extra label appended
// (le, for histogram buckets).
func (f *family) labelSet(vals []string, extraK, extraV string) string {
	if len(vals) == 0 && extraK == "" {
		return ""
	}
	var b strings.Builder
	b.WriteByte('{')
	for i, v := range vals {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(f.labels[i])
		b.WriteString(`="`)
		b.WriteString(escapeValue(v))
		b.WriteByte('"')
	}
	if extraK != "" {
		if len(vals) > 0 {
			b.WriteByte(',')
		}
		b.WriteString(extraK)
		b.WriteString(`="`)
		b.WriteString(escapeValue(extraV))
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}

var valueEscaper = strings.NewReplacer(`\`, `\\`, "\n", `\n`, `"`, `\"`)
var helpEscaper = strings.NewReplacer(`\`, `\\`, "\n", `\n`)

func escapeValue(s string) string { return valueEscaper.Replace(s) }
func escapeHelp(s string) string  { return helpEscaper.Replace(s) }

// num formats a sample value the way Prometheus parses it: plain decimal for
// whole numbers (so counters do not turn into 1e+06), shortest round-trip
// otherwise.
func num(v float64) string {
	switch {
	case math.IsNaN(v):
		return "NaN"
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	case v == math.Trunc(v) && math.Abs(v) < 1e15:
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}
