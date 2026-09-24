package backends

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/lovemoon-ai/mldojo/api/internal/models"
	v1 "github.com/lovemoon-ai/mldojo/proto/mldojo/v1"
	"github.com/lovemoon-ai/mldojo/recipes"
)

// Admission and placement.
//
// Decision #1 stands: MLDojo does not pack jobs, does not preempt and does
// not migrate anything. But "no scheduler" had come to mean no admission
// control at all -- a node took unlimited concurrent runs, a queued run was
// only retried when its agent happened to reconnect, and a recipe had to
// name one machine even when any 5090 would do.

const reconcileInterval = 15 * time.Second

// fits reports whether a node's static capacity can satisfy the request.
func fits(n *v1.Node, res v1.Resources) bool {
	if res.GPUs == 0 {
		return true
	}
	if n.Capacity == nil {
		return false
	}
	usable := 0
	for _, g := range n.Capacity.GPUs {
		if !recipes.GPUTypeMatches(res.GPUType, g.Model) {
			continue
		}
		if res.MinMemGB > 0 && g.MemGB+1 < res.MinMemGB {
			continue
		}
		usable++
	}
	return usable >= res.GPUs
}

// usableGPUs counts the cards a run could take right now: no MLDojo run on
// them, and enough memory actually free. Who holds the memory does not
// matter -- a card another tenant filled is just as unusable as one of ours.
func usableGPUs(stats []v1.GPUStat, res v1.Resources) int {
	needMB := float64(res.MinMemGB) * 1024
	if needMB <= 0 {
		needMB = minFreeMB
	}
	n := 0
	for _, g := range stats {
		if len(g.RunIDs) > 0 || !recipes.GPUTypeMatches(res.GPUType, g.Model) {
			continue
		}
		if g.MemTotMB > 0 && g.FreeMB() < needMB {
			continue
		}
		n++
	}
	return n
}

// minFreeMB is what a card must have free to count as usable when the recipe
// did not say. Below this it is holding somebody's model, not idling.
const minFreeMB = 1024

// hasLabels reports whether a node carries every requested label.
func hasLabels(n *v1.Node, want []string) bool {
	have := make(map[string]bool, len(n.Labels))
	for _, l := range n.Labels {
		have[strings.ToLower(l)] = true
	}
	for _, w := range want {
		if !have[strings.ToLower(w)] {
			return false
		}
	}
	return true
}

// atCapacity reports whether the node is already running its limit.
func atCapacity(n *v1.Node) bool { return n.MaxRuns > 0 && n.ActiveRuns >= n.MaxRuns }

// PoolCandidates lists the nodes a pool target could ever land on, ignoring
// whether they are free right now. Used at submit time so an impossible
// request fails immediately instead of queueing forever.
func (b *NodeBackend) PoolCandidates(ctx context.Context, labels []string, res v1.Resources) ([]v1.Node, error) {
	nodes, err := b.rt.Store.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	var out []v1.Node
	for _, n := range nodes {
		if hasLabels(&n, labels) && fits(&n, res) {
			out = append(out, n)
		}
	}
	return out, nil
}

// pickNode chooses where a pool run should go: an online node with room,
// least loaded first. Returns "" when everything is busy, which leaves the
// run queued for the next reconcile.
func (b *NodeBackend) pickNode(ctx context.Context, labels []string, res v1.Resources) (string, error) {
	candidates, err := b.PoolCandidates(ctx, labels, res)
	if err != nil {
		return "", err
	}
	var free []v1.Node
	for _, n := range candidates {
		if b.Hub.Conn(n.ID) != nil && !atCapacity(&n) {
			free = append(free, n)
		}
	}
	if len(free) == 0 {
		return "", nil
	}
	// Prefer nodes that can actually take the job now. Counting only our
	// own runs used to call a card free while somebody else's process held
	// 25 GiB of it, and the job would land there and die of OOM.
	usable := map[string]int{}
	var ready []v1.Node
	for _, n := range free {
		usable[n.ID] = usableGPUs(b.Hub.GPU(n.ID), res)
		if res.GPUs == 0 || usable[n.ID] >= res.GPUs {
			ready = append(ready, n)
		}
	}
	if len(ready) > 0 {
		free = ready
	}
	// Fewest active runs first, then most usable GPUs, then by id so the
	// choice is stable and does not depend on map iteration order.
	sort.Slice(free, func(i, j int) bool {
		if free[i].ActiveRuns != free[j].ActiveRuns {
			return free[i].ActiveRuns < free[j].ActiveRuns
		}
		if usable[free[i].ID] != usable[free[j].ID] {
			return usable[free[i].ID] > usable[free[j].ID]
		}
		return free[i].ID < free[j].ID
	})
	return free[0].ID, nil
}

// admit reports why a run may not start yet, or "" when it may. Callers
// hold the node lock: the answer is only true for as long as they do.
func (b *NodeBackend) admit(ctx context.Context, run *v1.Run, nodeID string) string {
	n, err := b.rt.Store.GetNode(ctx, nodeID)
	if err != nil {
		return "" // let dispatch produce the real error
	}
	// Only runs that already hold the node count. A queued run holds
	// nothing, and counting it makes the limit self-blocking: with
	// max_runs=1 and two runs queued, ActiveRuns is 2, both are refused,
	// neither can ever start and the node sits idle forever.
	if n.MaxRuns > 0 {
		busy, err := b.rt.Store.NodeBusyRuns(ctx, nodeID)
		if err == nil && busy >= n.MaxRuns {
			return fmt.Sprintf("node %s is at its limit of %d concurrent runs", n.ID, n.MaxRuns)
		}
	}
	p, err := b.rt.Store.GetProject(ctx, run.Project)
	if err == nil && p.MaxConcurrentRuns > 0 {
		busy, err := b.rt.Store.ProjectBusyRuns(ctx, p.Name)
		if err == nil && busy >= p.MaxConcurrentRuns {
			return fmt.Sprintf("project %s is at its limit of %d concurrent runs", p.Name, p.MaxConcurrentRuns)
		}
	}
	if why := b.gpusAvailable(ctx, run, nodeID); why != "" {
		return why
	}
	return ""
}

// gpusAvailable holds a run back until the cards it asked for are actually
// free. min_mem_gb used to be compared against a card's *total* memory, once,
// at submit time -- so a 97 GB card passed even with 90 GB of somebody else's
// model on it, and the run OOMed on arrival. The declared requirement has to
// be checked against what is free, at the moment the run would start.
func (b *NodeBackend) gpusAvailable(ctx context.Context, run *v1.Run, nodeID string) string {
	res := runResources(run)
	if res.GPUs == 0 {
		return ""
	}
	stats := b.Hub.GPU(nodeID)
	if len(stats) == 0 {
		// No nvidia-smi on the node, or no heartbeat yet. Refusing here
		// would strand every run on a CPU node.
		return ""
	}
	pending, _ := b.rt.Store.NodeStartingGPUs(ctx, nodeID)
	return gpuShortfall(nodeID, stats, res, pending)
}

// gpuShortfall says why a node cannot give a run its cards right now, or ""
// when it can. pending is what has been promised to runs that have not
// started yet and so are still invisible to telemetry.
func gpuShortfall(nodeID string, stats []v1.GPUStat, res v1.Resources, pending int) string {
	free := usableGPUs(stats, res) - pending
	if free >= res.GPUs {
		return ""
	}
	need := "free"
	if res.MinMemGB > 0 {
		need = fmt.Sprintf("with %d GB free", res.MinMemGB)
	}
	return fmt.Sprintf("node %s has %d of the %d cards %s that it needs", nodeID, max(free, 0), res.GPUs, need)
}

// StartScheduler retries queued runs on a timer. Before this a run that
// arrived while every node was busy sat there until an agent reconnected --
// which might be never.
func (b *NodeBackend) StartScheduler(ctx context.Context) {
	go func() {
		t := time.NewTicker(reconcileInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				b.Reconcile(ctx)
			}
		}
	}()
}

// Reconcile dispatches whatever queued runs can start now, oldest first.
func (b *NodeBackend) Reconcile(ctx context.Context) {
	runs, err := b.rt.Store.ListRuns(ctx, models.RunFilter{
		BackendKind: "node", Status: v1.PhaseQueued, Sort: "created", Asc: true, Limit: 500,
	})
	if err != nil {
		slog.Warn("reconcile: list queued runs", "err", err)
		return
	}
	for i := range runs {
		run := &runs[i]
		node := run.BackendID
		if node == "" {
			// A pool run with no placement yet.
			t, err := recipes.ParseTarget(run.Target)
			if err != nil {
				continue
			}
			if node, err = b.pickNode(ctx, t.Labels, runResources(run)); err != nil || node == "" {
				continue // nothing free; try again next tick
			}
			if err := b.rt.Store.SetRunBackend(ctx, run.ID, node); err != nil {
				slog.Warn("reconcile: place run", "run", run.ID, "node", node, "err", err)
				continue
			}
			b.Hub.runNode.Store(run.ID, node)
			b.rt.Store.AddEvent(ctx, run.ID, "placed", map[string]any{"node": node})
		}
		if b.Hub.Conn(node) == nil {
			continue // its agent is offline; onOnline will pick it up
		}
		// A cheap pre-check, so a full queue does not spawn a goroutine per
		// run every tick. dispatch re-checks under the node lock, which is
		// the answer that counts.
		if why := b.admit(ctx, run, node); why != "" {
			continue
		}
		go b.dispatch(run.ID)
	}
}

func runResources(run *v1.Run) v1.Resources {
	var res v1.Resources
	if len(run.Resources) > 0 {
		_ = json.Unmarshal(run.Resources, &res)
	}
	return res
}
