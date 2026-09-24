package obs

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Event types. These are the names accepted in alerts.on.
const (
	EventRunFailed     = "run.failed"
	EventNodeOffline   = "node.offline"
	EventDiskHigh      = "disk.high"
	EventSecretsLocked = "secrets.locked"
)

// AllEvents is the default subscription.
var AllEvents = []string{EventRunFailed, EventNodeOffline, EventDiskHigh, EventSecretsLocked}

// Event is one thing worth waking someone up for. Fields must contain
// identifiers and status only: this struct is serialised to a third-party
// webhook, so no secret values, tokens or connection strings.
type Event struct {
	Type   string // one of the Event* constants
	Level  string // info | warning | error
	Title  string
	Text   string
	Fields map[string]string
	// Key collapses repeats: the same key does not fire again within
	// MinInterval. Defaults to Type, so per-node or per-run keys need to be
	// set explicitly ("node.offline/gpu-a").
	Key string
}

func (e Event) key() string {
	if e.Key != "" {
		return e.Key
	}
	return e.Type
}

// AlertConfig is the alerts: section of the API config.
type AlertConfig struct {
	WebhookURL  string
	On          []string      // subscribed event types; empty means AllEvents
	MinInterval time.Duration // per-key dedup window
	Instance    string        // which MLDojo this is (public URL)
	Timeout     time.Duration // one webhook POST
}

const (
	// DefaultMinInterval is deliberately long. A node that flaps every
	// minute must not produce a message per flap.
	DefaultMinInterval = 10 * time.Minute
	defaultTimeout     = 10 * time.Second
	queueSize          = 64
	// burstCap and burstRefill bound the total rate across all keys, so a
	// mass failure (200 runs on a dead node) cannot flood the channel even
	// though every run has its own dedup key.
	burstCap    = 20.0
	burstRefill = time.Minute
)

// Notifier posts events to a webhook, off the caller's goroutine.
type Notifier struct {
	cfg  AlertConfig
	on   map[string]bool
	http *http.Client
	q    chan Event
	now  func() time.Time

	mu     sync.Mutex
	last   map[string]time.Time
	tokens float64
	filled time.Time
}

// NewNotifier builds a notifier. It sends nothing until Start is called.
func NewNotifier(cfg AlertConfig) *Notifier {
	if cfg.MinInterval <= 0 {
		cfg.MinInterval = DefaultMinInterval
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultTimeout
	}
	if len(cfg.On) == 0 {
		cfg.On = AllEvents
	}
	on := map[string]bool{}
	for _, e := range cfg.On {
		if e = strings.TrimSpace(e); e != "" {
			on[e] = true
		}
	}
	now := time.Now()
	return &Notifier{cfg: cfg, on: on, q: make(chan Event, queueSize), now: time.Now,
		http: &http.Client{Timeout: cfg.Timeout}, last: map[string]time.Time{},
		tokens: burstCap, filled: now}
}

// Enabled reports whether there is anywhere to send.
func (n *Notifier) Enabled() bool { return n != nil && n.cfg.WebhookURL != "" }

// Start runs the sender until ctx is done.
func (n *Notifier) Start(ctx context.Context) {
	if !n.Enabled() {
		return
	}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case ev := <-n.q:
				n.post(ctx, ev)
			}
		}
	}()
}

// Notify queues an event. It never blocks and never fails: alerting is not
// allowed to slow down or break the thing it is watching.
func (n *Notifier) Notify(ev Event) {
	if !n.Enabled() || !n.on[ev.Type] {
		return
	}
	if !n.allow(ev.key(), n.now()) {
		Alerts.With(ev.Type, "suppressed").Inc()
		return
	}
	select {
	case n.q <- ev:
	default:
		// The sender is stuck (a webhook that hangs); dropping is better
		// than growing a queue nobody will ever drain.
		Alerts.With(ev.Type, "suppressed").Inc()
		slog.Warn("alert dropped: queue full", "event", ev.Type)
	}
}

// allow applies the per-key dedup window and the global burst cap. Pure
// apart from the mutex: now is passed in so it can be tested.
func (n *Notifier) allow(key string, now time.Time) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	if t, ok := n.last[key]; ok && now.Sub(t) < n.cfg.MinInterval {
		return false
	}
	if d := now.Sub(n.filled); d > 0 { // a clock stepping back must not drain the bucket
		n.tokens += d.Seconds() / burstRefill.Seconds()
	}
	if n.tokens > burstCap {
		n.tokens = burstCap
	}
	n.filled = now
	if n.tokens < 1 {
		return false
	}
	n.tokens--
	if len(n.last) > 1000 { // keys are per run id, so this map needs pruning
		for k, t := range n.last {
			if now.Sub(t) > 2*n.cfg.MinInterval {
				delete(n.last, k)
			}
		}
	}
	n.last[key] = now
	return true
}

func (n *Notifier) post(ctx context.Context, ev Event) {
	body := n.payload(ev, n.now())
	rctx, cancel := context.WithTimeout(ctx, n.cfg.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodPost, n.cfg.WebhookURL, bytes.NewReader(body))
	if err != nil {
		Alerts.With(ev.Type, "failed").Inc()
		slog.Warn("alert webhook", "event", ev.Type, "err", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := n.http.Do(req)
	if err != nil {
		Alerts.With(ev.Type, "failed").Inc()
		slog.Warn("alert webhook", "event", ev.Type, "err", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		Alerts.With(ev.Type, "failed").Inc()
		slog.Warn("alert webhook", "event", ev.Type, "status", resp.StatusCode)
		return
	}
	Alerts.With(ev.Type, "sent").Inc()
}

// payload is one JSON body that three kinds of receiver understand: Feishu
// custom bots read msg_type/content.text, Slack incoming webhooks read text,
// and anything else gets the structured fields.
type payload struct {
	MsgType string `json:"msg_type"`
	Content struct {
		Text string `json:"text"`
	} `json:"content"`
	Text     string            `json:"text"`
	Source   string            `json:"source"`
	Instance string            `json:"instance,omitempty"`
	Event    string            `json:"event"`
	Level    string            `json:"level"`
	Title    string            `json:"title"`
	Fields   map[string]string `json:"fields,omitempty"`
	Time     string            `json:"time"`
}

func (n *Notifier) payload(ev Event, now time.Time) []byte {
	p := payload{MsgType: "text", Source: "mldojo", Instance: n.cfg.Instance,
		Event: ev.Type, Level: level(ev.Level), Title: redact(ev.Title),
		Time: now.Format(time.RFC3339)}
	if len(ev.Fields) > 0 {
		p.Fields = make(map[string]string, len(ev.Fields))
		for k, v := range ev.Fields {
			p.Fields[k] = redact(v)
		}
	}
	p.Text = renderText(p, redact(ev.Text))
	p.Content.Text = p.Text
	b, err := json.Marshal(p)
	if err != nil { // only possible on a value json cannot encode; none here
		return []byte(`{"msg_type":"text","content":{"text":"mldojo alert"}}`)
	}
	return b
}

func level(l string) string {
	if l == "" {
		return "warning"
	}
	return l
}

// renderText is the human-readable line every receiver shows.
func renderText(p payload, text string) string {
	var b strings.Builder
	b.WriteString("[MLDojo] ")
	b.WriteString(p.Title)
	if text != "" {
		b.WriteString("\n")
		b.WriteString(text)
	}
	keys := make([]string, 0, len(p.Fields))
	for k := range p.Fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		b.WriteString("\n")
		b.WriteString(k)
		b.WriteString(": ")
		b.WriteString(p.Fields[k])
	}
	if p.Instance != "" {
		b.WriteString("\n")
		b.WriteString(p.Instance)
	}
	return b.String()
}

// secretish matches the shapes a credential takes in this codebase: API and
// run tokens, agent token hashes, database URLs and Authorization headers.
// Alert text comes from error messages, and error messages quote commands.
var secretish = regexp.MustCompile(`(?i)mld_[a-z0-9]{8,}|bearer\s+\S+|(?:token|password|secret|passwd|api[_-]?key)["' ]*[:=]\s*\S+|postgres(?:ql)?://\S+|\b[0-9a-f]{32,}\b`)

const maxFieldLen = 300

// redact removes anything that looks like a credential and bounds the
// length, so a runaway error message cannot become the alert.
func redact(s string) string {
	s = secretish.ReplaceAllString(s, "[redacted]")
	if len(s) > maxFieldLen {
		s = s[:maxFieldLen] + "..."
	}
	return s
}

// The default notifier, set once at startup. Nil until then, which is why
// every call goes through Alert.
var def atomic.Pointer[Notifier]

// SetNotifier installs the notifier package-level Alert calls use.
func SetNotifier(n *Notifier) { def.Store(n) }

// Alert sends an event through the configured notifier, if any.
func Alert(ev Event) {
	if n := def.Load(); n != nil {
		n.Notify(ev)
	}
}
