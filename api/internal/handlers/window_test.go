package handlers

import (
	"net/http/httptest"
	"testing"
	"time"
)

// `mldojo usage`, `gpu idle` and `node history` all send a window, and the
// CLI's own default for usage is "7d". Go durations stop at hours, so the
// command failed with no flags at all.
func TestWindowAcceptsDays(t *testing.T) {
	for _, tc := range []struct {
		q    string
		want time.Duration
	}{
		{"7d", 7 * 24 * time.Hour},
		{"1.5d", 36 * time.Hour},
		{"24h", 24 * time.Hour},
		{"90m", 90 * time.Minute},
	} {
		since, _, err := window(httptest.NewRequest("GET", "/x?since="+tc.q, nil))
		if err != nil {
			t.Errorf("since=%s: %v", tc.q, err)
			continue
		}
		if got := time.Since(since); got < tc.want-time.Minute || got > tc.want+time.Minute {
			t.Errorf("since=%s is %v ago, want %v", tc.q, got.Round(time.Second), tc.want)
		}
	}
	if _, _, err := window(httptest.NewRequest("GET", "/x?since=7days", nil)); err == nil {
		t.Error("an unparseable window must be refused, not read as zero")
	}
	if _, _, err := window(httptest.NewRequest("GET", "/x?since=-3d", nil)); err == nil {
		t.Error("a negative window must be refused")
	}
}
