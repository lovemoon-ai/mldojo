package backends

import (
	"strings"
	"testing"

	v1 "github.com/lovemoon-ai/mldojo/proto/mldojo/v1"
)

func node(id string, labels []string, gpus ...v1.GPUInfo) *v1.Node {
	n := &v1.Node{ID: id, Labels: labels}
	if len(gpus) > 0 {
		n.Capacity = &v1.Capacity{GPUs: gpus}
	}
	return n
}

func TestFits(t *testing.T) {
	two5090 := node("a", nil, v1.GPUInfo{Model: "NVIDIA GeForce RTX 5090", MemGB: 32},
		v1.GPUInfo{Model: "NVIDIA GeForce RTX 5090", MemGB: 32})
	cases := []struct {
		name string
		n    *v1.Node
		res  v1.Resources
		want bool
	}{
		{"no gpus requested fits anything", node("cpu", nil), v1.Resources{}, true},
		{"gpus requested but node has none", node("cpu", nil), v1.Resources{GPUs: 1}, false},
		{"enough gpus", two5090, v1.Resources{GPUs: 2}, true},
		{"more gpus than the node has", two5090, v1.Resources{GPUs: 3}, false},
		{"matching model", two5090, v1.Resources{GPUs: 1, GPUType: "5090"}, true},
		{"other model", two5090, v1.Resources{GPUs: 1, GPUType: "h100"}, false},
		{"enough memory", two5090, v1.Resources{GPUs: 1, MinMemGB: 32}, true},
		{"not enough memory", two5090, v1.Resources{GPUs: 1, MinMemGB: 80}, false},
	}
	for _, c := range cases {
		if got := fits(c.n, c.res); got != c.want {
			t.Errorf("%s: fits = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestHasLabels(t *testing.T) {
	n := node("bastion-a", []string{"5090", "8gpu", "DC1"})
	cases := []struct {
		name string
		want []string
		ok   bool
	}{
		{"no requirement", nil, true},
		{"one label", []string{"5090"}, true},
		{"all labels", []string{"5090", "8gpu"}, true},
		{"case insensitive", []string{"dc1"}, true},
		{"one missing label fails the whole set", []string{"5090", "h100"}, false},
	}
	for _, c := range cases {
		if got := hasLabels(n, c.want); got != c.ok {
			t.Errorf("%s: hasLabels(%v) = %v, want %v", c.name, c.want, got, c.ok)
		}
	}
}

func TestAtCapacity(t *testing.T) {
	cases := []struct {
		name        string
		max, active int
		want        bool
	}{
		// 0 is what every node had before limits existed, so it must stay
		// unlimited or upgrading would stall every cluster.
		{"unlimited by default", 0, 99, false},
		{"under the limit", 4, 3, false},
		{"at the limit", 4, 4, true},
		{"over the limit", 4, 9, true},
	}
	for _, c := range cases {
		n := &v1.Node{MaxRuns: c.max, ActiveRuns: c.active}
		if got := atCapacity(n); got != c.want {
			t.Errorf("%s: atCapacity = %v, want %v", c.name, got, c.want)
		}
	}
}

// A card another tenant filled is just as unusable as one of ours. Counting
// only our own runs sent jobs to cards with 25 GiB already held, where they
// died of OOM.
func TestUsableGPUs(t *testing.T) {
	// Modelled on bastion-b: eight cards, most with somebody's memory on them.
	card := func(usedMB float64, runs ...string) v1.GPUStat {
		return v1.GPUStat{Model: "NVIDIA GeForce RTX 5090", MemTotMB: 32607, MemUsedMB: usedMB, RunIDs: runs}
	}
	stats := []v1.GPUStat{
		card(0),          // genuinely free
		card(25299),      // 25 GiB held by a stranger, 0% util
		card(300),        // a display server; still nearly empty
		card(0, "run-1"), // ours
		card(32500),      // full
	}
	// With no stated requirement, anything with room left is fair game --
	// but a full card is not, and neither is one of ours.
	if got := usableGPUs(stats, v1.Resources{GPUs: 1}); got != 3 {
		t.Errorf("usableGPUs = %d, want 3 (full card and our own card excluded)", got)
	}
	// A job that says how much memory it needs gets held to that, which is
	// the case the old "count our runs" rule got wrong.
	if got := usableGPUs(stats, v1.Resources{GPUs: 1, MinMemGB: 24}); got != 2 {
		t.Errorf("usableGPUs with min_mem_gb=24 = %d, want 2 (the 25 GiB card no longer fits)", got)
	}
	if got := usableGPUs(stats, v1.Resources{GPUs: 1, MinMemGB: 80}); got != 0 {
		t.Errorf("usableGPUs with min_mem_gb=80 = %d, want 0", got)
	}
	// Model filter still applies.
	if got := usableGPUs(stats, v1.Resources{GPUs: 1, GPUType: "h100"}); got != 0 {
		t.Errorf("usableGPUs for the wrong model = %d, want 0", got)
	}
	// A node that reports no stats at all must not be assumed empty.
	if got := usableGPUs(nil, v1.Resources{GPUs: 1}); got != 0 {
		t.Errorf("usableGPUs with no stats = %d, want 0", got)
	}
}

// min_mem_gb was a comment, not a constraint: on a node: target it was only
// ever compared against a card's total memory, so `min_mem_gb: 80` passed on
// a 97 GB H20 that a colleague had 90 GB of.
func TestGPUShortfallHoldsARunBackUntilTheCardIsFree(t *testing.T) {
	// h20: one 97871 MB card, and somebody else is on it.
	held := []v1.GPUStat{{Model: "NVIDIA H20", MemTotMB: 97871, MemUsedMB: 90000}}
	free := []v1.GPUStat{{Model: "NVIDIA H20", MemTotMB: 97871, MemUsedMB: 512}}
	res := v1.Resources{GPUs: 1, MinMemGB: 80}

	if why := gpuShortfall("h20", held, res, 0); why == "" {
		t.Error("a card with 7 GB free must not admit a run that asked for 80")
	} else if !strings.Contains(why, "80 GB free") {
		t.Errorf("the reason should say what was asked for, got %q", why)
	}
	if why := gpuShortfall("h20", free, res, 0); why != "" {
		t.Errorf("an empty card must admit the run, got %q", why)
	}
	// The run admitted a second ago has not started its process, so the card
	// still reads free. Without counting it, both runs start and the second
	// dies of OOM -- the same read-then-write shape as the concurrency limit.
	if why := gpuShortfall("h20", free, res, 1); why == "" {
		t.Error("a card already promised to a starting run must not be promised twice")
	}
	// A run that asked for no GPUs is not this function's business, but a
	// node with no requirement stated still needs a card with room.
	if why := gpuShortfall("h20", held, v1.Resources{GPUs: 1}, 0); why != "" {
		t.Errorf("7 GB free is enough when nothing was asked for, got %q", why)
	}
}
