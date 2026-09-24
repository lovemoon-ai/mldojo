package gpu

import (
	"os"
	"path/filepath"
	"testing"
)

const corpus = "../../../testdata/real"

// nvidia-smi as h20 actually prints it. The agent runs in a container that
// does not share the host's pid namespace, so every process on the card comes
// back "[Not Found]" -- including the training run we started ourselves.
func TestRealNvidiaSmi(t *testing.T) {
	stats := parse(read(t, "nvidia-smi-query-gpu.csv"))
	if len(stats) != 1 || stats[0].MemTotMB != 97871 || stats[0].Model != "NVIDIA H20" {
		t.Fatalf("parsed %+v", stats)
	}
	if got := parseProcs(read(t, "nvidia-smi-compute-apps.csv"), stats); len(got) != 2 {
		t.Fatalf("pids = %v, want 2", got)
	}
	for i, p := range stats[0].Procs {
		if p.Name != "" || !p.Unresolved() {
			t.Errorf("proc %d = %+v, want an unresolved process", i, p)
		}
	}
	// Nothing of ours is recorded on the card, so both are strangers.
	if n, mem := stats[0].Foreign(); n != 2 || mem != 3480 {
		t.Errorf("Foreign() = (%d, %.0f), want (2, 3480)", n, mem)
	}
	// Once one of our runs is on it, memory we cannot attribute is most
	// likely that run: calling it foreign warns us about ourselves.
	stats[0].RunIDs = []string{"run-1"}
	if n, mem := stats[0].Foreign(); n != 0 || mem != 0 {
		t.Errorf("Foreign() on our own card = (%d, %.0f), want (0, 0)", n, mem)
	}
}

func read(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(corpus, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
