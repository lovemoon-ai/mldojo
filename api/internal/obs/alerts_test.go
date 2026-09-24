package obs

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestThrottlePerKey(t *testing.T) {
	n := NewNotifier(AlertConfig{WebhookURL: "http://x", MinInterval: 10 * time.Minute})
	t0 := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)

	if !n.allow("node.offline/gpu-a", t0) {
		t.Fatal("the first event for a key must go out")
	}
	if n.allow("node.offline/gpu-a", t0.Add(time.Second)) {
		t.Error("a flapping node must not alert again one second later")
	}
	if n.allow("node.offline/gpu-a", t0.Add(9*time.Minute)) {
		t.Error("still inside the dedup window")
	}
	if !n.allow("node.offline/gpu-a", t0.Add(11*time.Minute)) {
		t.Error("after the window the same key must alert again")
	}
	// A different node is a different key and is not affected.
	if !n.allow("node.offline/bastion", t0.Add(11*time.Minute)) {
		t.Error("another node must not be suppressed by the first one")
	}
}

func TestBurstCapStopsAFlood(t *testing.T) {
	n := NewNotifier(AlertConfig{WebhookURL: "http://x", MinInterval: time.Hour})
	t0 := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	// 200 runs fail at once on a dead node: every one has its own key, so
	// only the global cap can stop them.
	allowed := 0
	for i := 0; i < 200; i++ {
		if n.allow("run.failed/"+string(rune('a'+i%26))+time.Duration(i).String(), t0) {
			allowed++
		}
	}
	if allowed != int(burstCap) {
		t.Errorf("let through %d alerts at once, want %d", allowed, int(burstCap))
	}
	// The bucket refills at one per minute.
	if n.allow("run.failed/later", t0.Add(30*time.Second)) {
		t.Error("half a minute is not enough to refill a token")
	}
	if !n.allow("run.failed/later", t0.Add(2*time.Minute)) {
		t.Error("two minutes should have refilled the bucket")
	}
}

func TestSubscriptionFilter(t *testing.T) {
	n := NewNotifier(AlertConfig{WebhookURL: "http://x", On: []string{EventRunFailed}})
	if !n.on[EventRunFailed] {
		t.Error("run.failed was subscribed")
	}
	if n.on[EventNodeOffline] {
		t.Error("node.offline was not subscribed")
	}
	// An unsubscribed event must not even reach the queue.
	n.Notify(Event{Type: EventNodeOffline, Title: "x"})
	if len(n.q) != 0 {
		t.Errorf("queued %d unsubscribed events", len(n.q))
	}
	n.Notify(Event{Type: EventRunFailed, Title: "x"})
	if len(n.q) != 1 {
		t.Errorf("queued %d subscribed events, want 1", len(n.q))
	}
}

func TestEmptySubscriptionMeansEverything(t *testing.T) {
	n := NewNotifier(AlertConfig{WebhookURL: "http://x"})
	for _, e := range AllEvents {
		if !n.on[e] {
			t.Errorf("%s should be on by default", e)
		}
	}
}

func TestRedactKeepsCredentialsOutOfThePayload(t *testing.T) {
	cases := []struct{ in, mustNotContain string }{
		{"auth failed with mld_0123456789abcdef0123", "mld_0123456789abcdef0123"},
		{"curl -H 'Authorization: Bearer sk-abc123'", "sk-abc123"},
		{"dial postgres://mldojo:hunter2@10.0.0.1:5432/mldojo failed", "hunter2"},
		{"agent token hash 5f4dcc3b5aa765d61d8327deb882cf995f4dcc3b5aa765d61d8327deb882cf99", "5f4dcc3b5aa765d61d8327deb882cf99"},
		{`export API_KEY=abcdefgh`, "abcdefgh"},
		{`{"token": "s3cr3tvalue"}`, "s3cr3tvalue"},
	}
	for _, c := range cases {
		got := redact(c.in)
		if strings.Contains(got, c.mustNotContain) {
			t.Errorf("redact(%q) = %q, still leaks %q", c.in, got, c.mustNotContain)
		}
	}
	if got := redact("run failed: exit code 137 (OOM)"); got != "run failed: exit code 137 (OOM)" {
		t.Errorf("a harmless message must survive redaction, got %q", got)
	}
	if got := redact(strings.Repeat("x", 5000)); len(got) > maxFieldLen+3 {
		t.Errorf("a 5000 byte message became a %d byte alert", len(got))
	}
}

func TestPayloadShape(t *testing.T) {
	n := NewNotifier(AlertConfig{WebhookURL: "http://x", Instance: "https://mldojo.example.com"})
	body := n.payload(Event{Type: EventRunFailed, Level: "error", Title: "run failed: demo/sweep 1a2b3c4d",
		Text: "exit code 1", Fields: map[string]string{"run": "1a2b3c4d", "node": "gpu-a", "leak": "mld_00112233445566778899"}},
		time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC))

	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("payload is not valid JSON: %v", err)
	}
	if got["msg_type"] != "text" { // Feishu custom bot
		t.Errorf("msg_type = %v", got["msg_type"])
	}
	if c, _ := got["content"].(map[string]any); c["text"] == "" || c["text"] == nil {
		t.Error("content.text is what a Feishu bot renders")
	}
	if got["text"] == "" || got["text"] == nil { // Slack
		t.Error("text is what a Slack webhook renders")
	}
	if got["event"] != EventRunFailed || got["level"] != "error" {
		t.Errorf("structured fields wrong: %v", got)
	}
	if got["time"] != "2026-09-20T12:00:00Z" {
		t.Errorf("time = %v", got["time"])
	}
	if s := string(body); strings.Contains(s, "mld_00112233445566778899") {
		t.Errorf("a token in a field reached the payload: %s", s)
	}
	if s := string(body); !strings.Contains(s, "https://mldojo.example.com") {
		t.Error("the payload should say which instance it came from")
	}
}

func TestSendsToWebhook(t *testing.T) {
	got := make(chan []byte, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- b
		w.WriteHeader(200)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n := NewNotifier(AlertConfig{WebhookURL: srv.URL})
	n.Start(ctx)
	n.Notify(Event{Type: EventDiskHigh, Title: "data dir is 95% full"})
	select {
	case b := <-got:
		if !strings.Contains(string(b), "data dir is 95% full") {
			t.Errorf("webhook body = %s", b)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the webhook was never called")
	}
}

// A webhook that hangs must not stall the caller, and a webhook that errors
// must not panic: alerting sits on the run status path.
func TestABrokenWebhookDoesNotBlockTheCaller(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
	}))
	defer srv.Close()
	defer close(block)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n := NewNotifier(AlertConfig{WebhookURL: srv.URL, MinInterval: time.Nanosecond, Timeout: time.Second})
	n.Start(ctx)
	done := make(chan struct{})
	go func() {
		for i := 0; i < queueSize*4; i++ {
			n.Notify(Event{Type: EventRunFailed, Title: "x", Key: "k"})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Notify blocked on a hanging webhook")
	}
}

func TestDisabledNotifierIsANoop(t *testing.T) {
	var n *Notifier
	if n.Enabled() {
		t.Error("a nil notifier is not enabled")
	}
	n = NewNotifier(AlertConfig{}) // no webhook_url configured
	if n.Enabled() {
		t.Error("no webhook url means disabled")
	}
	n.Notify(Event{Type: EventRunFailed, Title: "x"})
	if len(n.q) != 0 {
		t.Error("a disabled notifier must not queue anything")
	}
	Alert(Event{Type: EventRunFailed, Title: "no notifier installed"}) // must not panic
}
