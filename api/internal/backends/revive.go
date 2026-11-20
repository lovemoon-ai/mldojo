package backends

import (
	"context"
	"log/slog"
	"time"

	v1 "github.com/lovemoon-ai/mldojo/proto/mldojo/v1"
)

// Agents on shared clusters run unsupervised: a reboot or a sweep that kills
// the user's processes leaves the node offline until someone redeploys it.
// The reviver redeploys them itself, well inside the reaper's grace window
// so their runs are re-adopted instead of failed.
const (
	reviveAfter    = 5 * time.Minute  // offline this long before we step in
	reviveBackoff  = 30 * time.Minute // between attempts on a node that stays dead
	reviveTimeout  = 3 * time.Minute  // one redeploy, including the 60s dial-back
	reviveInterval = time.Minute
)

// StartReviver redeploys agents that went offline and stayed offline.
func (b *NodeBackend) StartReviver(ctx context.Context) {
	go func() {
		tried := map[string]time.Time{}
		t := time.NewTicker(reviveInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			nodes, err := b.rt.Store.ListNodes(ctx)
			if err != nil {
				slog.Warn("reviver: list nodes", "err", err)
				continue
			}
			for _, n := range nodes {
				now := time.Now()
				if b.Hub.Conn(n.ID) != nil || !dueForRevive(n, tried[n.ID], now) {
					continue
				}
				tried[n.ID] = now
				slog.Info("reviving offline agent", "node", n.ID, "last_heartbeat", n.LastHeartbeat)
				rctx, cancel := context.WithTimeout(ctx, reviveTimeout)
				_, err := b.UpgradeNode(rctx, n.ID, ConnPatch{})
				cancel()
				if err != nil {
					slog.Warn("reviver: redeploy failed", "node", n.ID, "err", err, "retry_in", reviveBackoff)
				} else {
					slog.Info("reviver: agent back online", "node", n.ID)
				}
			}
		}
	}()
}

// dueForRevive: offline long enough, and not tried recently. A node that
// never connected (nil heartbeat) is left to whoever is adding it.
func dueForRevive(n v1.Node, lastTry, now time.Time) bool {
	if n.AgentStatus == "online" || n.LastHeartbeat == nil {
		return false
	}
	return now.Sub(*n.LastHeartbeat) >= reviveAfter && now.Sub(lastTry) >= reviveBackoff
}
