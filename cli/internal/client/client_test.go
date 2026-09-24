package client

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestExitCode(t *testing.T) {
	cases := []struct {
		code string
		want int
	}{
		{"user_error", ExitUser},
		{"not_found", ExitUser},
		{"unauthorized", ExitUser},
		// An agent that reads "you may not do this" as a transient backend
		// fault retries it forever.
		{"forbidden", ExitUser},
		{"unreachable", ExitUnreachable},
		{"conflict", ExitConflict},
		{"backend_error", ExitBackend},
		{"internal", ExitBackend},
	}
	for _, c := range cases {
		if got := ExitCode(&Error{Code: c.code}); got != c.want {
			t.Errorf("ExitCode(%q) = %d, want %d", c.code, got, c.want)
		}
	}
	if got := ExitCode(nil); got != ExitOK {
		t.Errorf("ExitCode(nil) = %d, want %d", got, ExitOK)
	}
	if got := ExitCode(errors.New("connection refused")); got != ExitUnreachable {
		t.Errorf("ExitCode(connection refused) = %d, want %d", got, ExitUnreachable)
	}
}

func TestRetryAfter(t *testing.T) {
	at := func(v string) time.Duration {
		return retryAfter(&http.Response{Header: http.Header{"Retry-After": []string{v}}})
	}
	if d := retryAfter(&http.Response{Header: http.Header{}}); d != 5*time.Second {
		t.Errorf("no header: %v, want 5s", d)
	}
	if d := at("10"); d != 10*time.Second {
		t.Errorf("Retry-After 10: %v, want 10s", d)
	}
	if d := at("garbage"); d != 5*time.Second {
		t.Errorf("unparseable header: %v, want the 5s default", d)
	}
	// A hostile or buggy header must not park the CLI for hours.
	if d := at("99999"); d != 30*time.Second {
		t.Errorf("Retry-After 99999: %v, want the 30s clamp", d)
	}
}

func TestRequestRetriesRateLimit(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "0") // falls back to the default, so clamp it below
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "t")
	// Keep the test fast: the retry sleeps, so only assert it happened.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var out struct {
		OK bool `json:"ok"`
	}
	if err := c.Do(ctx, http.MethodPost, "/projects", map[string]string{"name": "x"}, &out); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if !out.OK {
		t.Error("retry did not reach the successful response")
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("server saw %d calls, want 2 (one 429, one retry)", n)
	}
}

func TestRewind(t *testing.T) {
	if !rewind(nil) {
		t.Error("a nil body is always replayable")
	}
	if rewind(onlyReader{}) {
		t.Error("a one-shot stream must not be reported as replayable")
	}
}

type onlyReader struct{}

func (onlyReader) Read([]byte) (int, error) { return 0, nil }
