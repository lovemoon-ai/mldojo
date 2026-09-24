package gpu

import (
	"testing"

	v1 "github.com/lovemoon-ai/mldojo/proto/mldojo/v1"
)

// Real output shapes from h20 and bastion-b.
func TestParseProcs(t *testing.T) {
	stats := []v1.GPUStat{
		{Index: 0, UUID: "GPU-aaa", MemTotMB: 97871},
		{Index: 1, UUID: "GPU-bbb", MemTotMB: 32607},
	}
	// nvidia-smi writes "[Not Found]" when it cannot resolve the process,
	// which is what a leaked or foreign-namespace process looks like.
	out := `GPU-aaa, 4165722, [Not Found], 530
GPU-aaa, 2696544, python3.10, 4370
GPU-bbb, 91, /usr/bin/train, 19700
GPU-zzz, 5, ghost, 100
`
	pids := parseProcs(out, stats)
	if len(pids) != 3 {
		t.Fatalf("pids = %v, want 3 (the row for an unknown card is dropped)", pids)
	}
	if len(stats[0].Procs) != 2 || len(stats[1].Procs) != 1 {
		t.Fatalf("procs per card = %d/%d, want 2/1", len(stats[0].Procs), len(stats[1].Procs))
	}
	if stats[0].Procs[0].Name != "" {
		t.Errorf("[Not Found] should become an empty name, got %q", stats[0].Procs[0].Name)
	}
	if stats[0].Procs[1].Name != "python3.10" || stats[0].Procs[1].MemUsedMB != 4370 {
		t.Errorf("second proc = %+v", stats[0].Procs[1])
	}

	// Nothing of ours is on card 0, so all of it counts as foreign: that is
	// the 4.9 GiB that used to be invisible.
	n, mem := stats[0].Foreign()
	if n != 2 || mem != 4900 {
		t.Errorf("Foreign() = (%d, %.0f), want (2, 4900)", n, mem)
	}
	if got := stats[0].FreeMB(); got != 97871 {
		t.Errorf("FreeMB with no MemUsedMB reading = %.0f, want the full card", got)
	}

	// Once a proc is attributed to a run it stops being foreign.
	stats[1].Procs[0].RunID = "r1"
	if n, _ := stats[1].Foreign(); n != 0 {
		t.Errorf("an attributed process must not count as foreign, got %d", n)
	}
}

func TestParseGPUWithUUID(t *testing.T) {
	stats := parse("0, NVIDIA H20, 20, 4930, 97871, 35, GPU-abc\n")
	if len(stats) != 1 {
		t.Fatalf("got %d stats", len(stats))
	}
	g := stats[0]
	if g.UUID != "GPU-abc" || g.MemUsedMB != 4930 || g.Util != 20 {
		t.Errorf("parsed %+v", g)
	}
	if got := g.FreeMB(); got != 92941 {
		t.Errorf("FreeMB = %.0f, want 92941", got)
	}
	// An older agent's output has no uuid column and must still parse.
	if old := parse("0, NVIDIA H20, 20, 4930, 97871, 35\n"); len(old) != 1 || old[0].UUID != "" {
		t.Errorf("six-column output should still parse: %+v", old)
	}
}

// On a host where nvidia-smi reports pids from another namespace -- h20
// runs the agent in a container that shares the GPU but not the pid space --
// a run's own training process arrives with no owner and no command line.
// Counting it as foreign double-counts our job and warns us about ourselves.
func TestAnonymousProcessOnOurOwnCard(t *testing.T) {
	g := v1.GPUStat{
		MemTotMB: 97871, MemUsedMB: 92000, RunIDs: []string{"a5dff6a7"},
		Procs: []v1.GPUProc{
			{PID: 982697, MemUsedMB: 86746},                                   // ours, unresolvable
			{PID: 2696544, MemUsedMB: 4782},                                   // a stranger, also unresolvable
			{PID: 11, User: "wei.xu", Cmd: "python train.py", MemUsedMB: 500}, // resolved, not ours
		},
	}
	n, mem := g.Foreign()
	if n != 1 || mem != 500 {
		t.Errorf("Foreign() = (%d, %.0f), want (1, 500): only the process we could resolve and is not ours", n, mem)
	}

	// With nothing of ours on the card, anonymous memory is somebody's and
	// worth reporting -- this is what warned before the run started.
	g.RunIDs = nil
	if n, mem := g.Foreign(); n != 3 || mem != 92028 {
		t.Errorf("Foreign() on an unclaimed card = (%d, %.0f), want (3, 92028)", n, mem)
	}
}
