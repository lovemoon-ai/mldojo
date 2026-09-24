package models

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	v1 "github.com/lovemoon-ai/mldojo/proto/mldojo/v1"
)

// testStore connects to MLDOJO_TEST_DATABASE_URL and applies the migrations.
// Without that variable the integration tests skip, so `go test ./...` still
// works on a machine with no PostgreSQL.
func testStore(t *testing.T) *Store {
	t.Helper()
	url := os.Getenv("MLDOJO_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set MLDOJO_TEST_DATABASE_URL to run the models integration tests")
	}
	ctx := context.Background()
	st, err := Open(ctx, url)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(st.Close)
	return st
}

// newRun creates a throwaway project/experiment/run and removes the project
// (cascading to everything below it) when the test ends.
func newRun(t *testing.T, st *Store) *v1.Run {
	t.Helper()
	ctx := context.Background()
	name := fmt.Sprintf("test-%d-%s", time.Now().UnixNano(), t.Name())
	if len(name) > 60 {
		name = name[:60]
	}
	p, err := st.CreateProject(ctx, name, "models integration test", "test")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	t.Cleanup(func() { st.DeleteProject(context.Background(), p.Name, true) })
	e, err := st.UpsertExperiment(ctx, p.ID, "exp", "", "", nil)
	if err != nil {
		t.Fatalf("upsert experiment: %v", err)
	}
	r := &v1.Run{
		ExperimentID: e.ID, Name: "r", Target: "node:test", BackendKind: "node", BackendID: "test",
		Status: v1.PhaseRunning, Resources: json.RawMessage(`{}`), Env: json.RawMessage(`{}`),
	}
	if err := st.InsertRun(ctx, r); err != nil {
		t.Fatalf("insert run: %v", err)
	}
	return r
}

// TestListMetricsKeepsEveryKey is the regression test for the truncation bug:
// `ORDER BY key, step LIMIT n` used to drop whole series whose key sorted late
// once a run had more than n points in total.
func TestListMetricsKeepsEveryKey(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	r := newRun(t, st)

	keys := []string{"aaa/first", "mmm/middle", "zzz/last"}
	const perKey = 900
	var pts []v1.MetricPoint
	for _, k := range keys {
		for i := range perKey {
			pts = append(pts, v1.MetricPoint{Step: int64(i), Key: k, Value: float64(i), TS: time.Now()})
		}
	}
	if err := st.UpsertMetrics(ctx, r.ID, pts); err != nil {
		t.Fatalf("upsert metrics: %v", err)
	}

	// A budget far below the total forces sampling on every key.
	got, sampled, err := st.ListMetrics(ctx, r.ID, "", 0, 100)
	if err != nil {
		t.Fatalf("list metrics: %v", err)
	}
	if !sampled {
		t.Error("sampled = false, want true: every key has more points than the budget")
	}
	byKey := map[string][]v1.MetricPoint{}
	for _, p := range got {
		byKey[p.Key] = append(byKey[p.Key], p)
	}
	for _, k := range keys {
		in := byKey[k]
		if len(in) == 0 {
			t.Errorf("key %q is missing entirely (the truncation bug)", k)
			continue
		}
		if len(in) > 110 { // budget + the always-kept endpoints
			t.Errorf("key %q returned %d points, want <= ~100", k, len(in))
		}
		if in[0].Step != 0 {
			t.Errorf("key %q first step = %d, want 0", k, in[0].Step)
		}
		if last := in[len(in)-1].Step; last != perKey-1 {
			t.Errorf("key %q last step = %d, want %d", k, last, perKey-1)
		}
	}
}

// TestListMetricsSmallRunIsExact checks that a run below the budget comes back
// whole and unflagged.
func TestListMetricsSmallRunIsExact(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	r := newRun(t, st)

	var pts []v1.MetricPoint
	for i := range 50 {
		pts = append(pts, v1.MetricPoint{Step: int64(i), Key: "loss", Value: float64(i), TS: time.Now()})
	}
	if err := st.UpsertMetrics(ctx, r.ID, pts); err != nil {
		t.Fatalf("upsert metrics: %v", err)
	}
	got, sampled, err := st.ListMetrics(ctx, r.ID, "", 0, 100)
	if err != nil {
		t.Fatalf("list metrics: %v", err)
	}
	if sampled {
		t.Error("sampled = true, want false: the run fits in the budget")
	}
	if len(got) != 50 {
		t.Errorf("got %d points, want 50", len(got))
	}
	for i, p := range got {
		if p.Step != int64(i) {
			t.Fatalf("point %d has step %d, want %d: order or sampling is wrong", i, p.Step, i)
		}
	}
}

// TestListMetricsKeyFilter checks the key filter still works with sampling.
func TestListMetricsKeyFilter(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	r := newRun(t, st)

	var pts []v1.MetricPoint
	for i := range 10 {
		pts = append(pts,
			v1.MetricPoint{Step: int64(i), Key: "loss", Value: 1, TS: time.Now()},
			v1.MetricPoint{Step: int64(i), Key: "acc", Value: 2, TS: time.Now()})
	}
	if err := st.UpsertMetrics(ctx, r.ID, pts); err != nil {
		t.Fatalf("upsert metrics: %v", err)
	}
	got, _, err := st.ListMetrics(ctx, r.ID, "loss", 0, 0)
	if err != nil {
		t.Fatalf("list metrics: %v", err)
	}
	if len(got) != 10 {
		t.Fatalf("got %d points, want 10", len(got))
	}
	for _, p := range got {
		if p.Key != "loss" {
			t.Fatalf("got key %q, want only loss", p.Key)
		}
	}
}
