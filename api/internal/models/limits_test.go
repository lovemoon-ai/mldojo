package models

import (
	"context"
	"encoding/json"
	"testing"

	v1 "github.com/lovemoon-ai/mldojo/proto/mldojo/v1"
)

// Queued runs must not count towards a concurrency limit. They hold nothing
// yet, and counting them makes the limit self-blocking: with max_runs=1 and
// two runs queued, both are refused, neither can ever start, and the node
// sits idle forever.
func TestBusyRunsIgnoresQueued(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	seed := newRun(t, st) // backend node "test", phase running

	for name, phase := range map[string]string{
		"q1":   v1.PhaseQueued,
		"q2":   v1.PhaseQueued,
		"s1":   v1.PhaseStarting,
		"done": v1.PhaseSucceeded,
	} {
		run := &v1.Run{
			ExperimentID: seed.ExperimentID, Name: name, Target: "node:test",
			BackendKind: "node", BackendID: "test", Status: phase,
			Resources: json.RawMessage(`{}`), Env: json.RawMessage(`{}`),
		}
		if err := st.InsertRun(ctx, run); err != nil {
			t.Fatalf("insert %s run: %v", phase, err)
		}
	}

	// ActiveRuns, which the UI shows, counts queued runs too. The limit
	// check deliberately does not.
	busy, err := st.NodeBusyRuns(ctx, "test")
	if err != nil {
		t.Fatalf("NodeBusyRuns: %v", err)
	}
	if busy != 2 { // seed (running) + s1 (starting)
		t.Errorf("NodeBusyRuns = %d, want 2: running + starting, neither queued nor finished", busy)
	}

	run, err := st.GetRun(ctx, seed.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	pbusy, err := st.ProjectBusyRuns(ctx, run.Project)
	if err != nil {
		t.Fatalf("ProjectBusyRuns: %v", err)
	}
	if pbusy != 2 {
		t.Errorf("ProjectBusyRuns = %d, want 2: running + starting, neither queued nor finished", pbusy)
	}
}
