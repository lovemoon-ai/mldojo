package backends

import (
	"testing"
	"time"

	v1 "github.com/lovemoon-ai/mldojo/proto/mldojo/v1"
)

func TestDueForRevive(t *testing.T) {
	now := time.Now()
	ago := func(d time.Duration) *time.Time { t := now.Add(-d); return &t }
	cases := []struct {
		name    string
		n       v1.Node
		lastTry time.Time
		want    bool
	}{
		{"online", v1.Node{AgentStatus: "online", LastHeartbeat: ago(time.Hour)}, time.Time{}, false},
		{"never connected", v1.Node{AgentStatus: "offline"}, time.Time{}, false},
		{"just dropped", v1.Node{AgentStatus: "offline", LastHeartbeat: ago(time.Minute)}, time.Time{}, false},
		{"offline long enough", v1.Node{AgentStatus: "offline", LastHeartbeat: ago(10 * time.Minute)}, time.Time{}, true},
		{"tried recently", v1.Node{AgentStatus: "offline", LastHeartbeat: ago(time.Hour)}, now.Add(-10 * time.Minute), false},
		{"backoff elapsed", v1.Node{AgentStatus: "offline", LastHeartbeat: ago(time.Hour)}, now.Add(-time.Hour), true},
	}
	for _, c := range cases {
		if got := dueForRevive(c.n, c.lastTry, now); got != c.want {
			t.Errorf("%s: dueForRevive = %v, want %v", c.name, got, c.want)
		}
	}
}
