package backends

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/lovemoon-ai/mldojo/api/internal/models"
	v1 "github.com/lovemoon-ai/mldojo/proto/mldojo/v1"
)

// Reaping settings. An agent that reconnects reconciles its own runs
// (see onOnline), so these only cover agents that never come back.
const (
	// DefaultOrphanGrace is how long a node's agent may stay unreachable
	// before its active runs are declared lost. Generous on purpose: an
	// agent restart or a flaky link must not kill a healthy training job.
	// If the agent does come back, stopStrayRuns kills whatever it is still
	// running for a reaped run, so a wrong guess here does not leak GPUs.
	DefaultOrphanGrace = 30 * time.Minute
	// DefaultStartingTimeout bounds the starting phase, which covers code
	// upload, image pull and environment setup.
	DefaultStartingTimeout = 2 * time.Hour
	reapInterval           = time.Minute
)

// StartReaper runs Reap periodically until ctx is done.
func (b *NodeBackend) StartReaper(ctx context.Context, grace, startingTimeout time.Duration) {
	if grace <= 0 {
		grace = DefaultOrphanGrace
	}
	if startingTimeout <= 0 {
		startingTimeout = DefaultStartingTimeout
	}
	go func() {
		t := time.NewTicker(reapInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				b.Reap(ctx, grace, startingTimeout)
			}
		}
	}()
}

// Reap fails runs nobody can report on any more: their node was deleted, or
// its agent has been gone long enough that the run's fate is unknowable.
// Without this they sit in running forever.
func (b *NodeBackend) Reap(ctx context.Context, grace, startingTimeout time.Duration) {
	runs, err := b.rt.Store.ListRuns(ctx, models.RunFilter{BackendKind: "node", Active: true, Limit: 5000})
	if err != nil {
		slog.Warn("reaper: list runs", "err", err)
		return
	}
	for _, r := range runs {
		reason := b.orphanReason(ctx, r, grace, startingTimeout)
		if reason == "" {
			continue
		}
		slog.Warn("reaping orphaned run", "run", r.ID, "node", r.BackendID, "status", r.Status, "reason", reason)
		// The run is lost, not wrong: a reap is exactly the case a retry
		// policy exists for.
		b.rt.FailInfra(ctx, r.ID, fmt.Errorf("%s", reason))
	}
}

// orphanReason returns why r should be failed, or "" to leave it alone.
func (b *NodeBackend) orphanReason(ctx context.Context, r v1.Run, grace, startingTimeout time.Duration) string {
	node, err := b.rt.Store.GetNode(ctx, r.BackendID)
	if models.IsNotFound(err) {
		return fmt.Sprintf("node %s was removed while this run was %s", r.BackendID, r.Status)
	}
	if err != nil {
		return "" // transient store error: try again next tick
	}
	// A queued run has not started, so it costs nothing to keep waiting for
	// the agent to come back and pick it up.
	if r.Status == v1.PhaseQueued {
		return ""
	}
	if r.Status == v1.PhaseStarting && time.Since(r.CreatedAt) > startingTimeout {
		return fmt.Sprintf("stuck in starting for more than %s", startingTimeout)
	}
	if b.Hub.Conn(r.BackendID) != nil {
		return "" // agent is connected and reporting
	}
	last := node.LastHeartbeat
	if last == nil {
		// Never reported at all; fall back to when the run was created.
		if time.Since(r.CreatedAt) > grace {
			return fmt.Sprintf("agent on %s has never reported in", r.BackendID)
		}
		return ""
	}
	if gone := time.Since(*last); gone > grace {
		return fmt.Sprintf("agent on %s has been unreachable for %s", r.BackendID, gone.Round(time.Second))
	}
	return ""
}
