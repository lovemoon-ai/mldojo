package exec

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	v1 "github.com/lovemoon-ai/mldojo/proto/mldojo/v1"
)

func run(workdir string) *Run {
	return &Run{dir: "/runs/abc", st: State{Spec: v1.SpawnRequest{Workdir: workdir}}}
}

func TestWorkDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix paths")
	}
	cases := []struct {
		name    string
		workdir string
		want    string
		inPlace bool
	}{
		{"default", ".", "/runs/abc/code", false},
		{"subdir of the bundle", "src", "/runs/abc/code/src", false},
		// Absolute means the project is already on the node, next to
		// gigabytes of weights and a prebuilt virtualenv.
		{"in place", "/root/ws/comp/lingbot_competition", "/root/ws/comp/lingbot_competition", true},
		{"in place, cleaned", "/root/ws/comp/../comp/x/", "/root/ws/comp/x", true},
	}
	for _, c := range cases {
		r := run(c.workdir)
		if got := r.workDir(); got != c.want {
			t.Errorf("%s: workDir = %q, want %q", c.name, got, c.want)
		}
		if got := r.inPlace(); got != c.inPlace {
			t.Errorf("%s: inPlace = %v, want %v", c.name, got, c.inPlace)
		}
	}
}

func TestGlobRoot(t *testing.T) {
	cases := []struct{ glob, root, rel string }{
		{"/a/b/outputs/*.ckpt", "/a/b/outputs", "*.ckpt"},
		{"/a/b/outputs/**/*.mp4", "/a/b/outputs", "**/*.mp4"},
		{"/a/*/c.txt", "/a", "*/c.txt"},
		{"/a/b/c.txt", "/a/b", "c.txt"},
		// A pattern in the first segment must not turn into a walk of "/".
		{"/*/x", "/", "*/x"},
	}
	for _, c := range cases {
		root, rel := globRoot(c.glob)
		if root != c.root || rel != c.rel {
			t.Errorf("globRoot(%q) = (%q, %q), want (%q, %q)", c.glob, root, rel, c.root, c.rel)
		}
	}
}

// An absolute glob collects files the job wrote outside the run directory,
// which is the only way to see the outputs of an in-place run.
func TestListFilesAbsoluteGlob(t *testing.T) {
	proj := t.TempDir()
	ckpt := filepath.Join(proj, "outputs", "exp1", "checkpoints")
	if err := os.MkdirAll(ckpt, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"step_500.ckpt", "step_1000.ckpt", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(ckpt, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	vids := filepath.Join(proj, "eval_results", "r1", "episodes")
	if err := os.MkdirAll(vids, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(vids, "episode0_failure.mp4"), []byte("v"), 0o644); err != nil {
		t.Fatal(err)
	}

	m := &Manager{Root: t.TempDir()}
	r := run(proj) // in place: workdir is the project itself
	got := m.ListFiles(r, []kindGlob{
		{"ckpt", filepath.Join(proj, "outputs/**/*.ckpt")},
		{"video", filepath.Join(proj, "eval_results/**/*.mp4")},
	})
	kinds := map[string]int{}
	for _, a := range got {
		kinds[a.Kind]++
	}
	if kinds["ckpt"] != 2 {
		t.Errorf("ckpt matches = %d, want 2 (notes.txt must not match)", kinds["ckpt"])
	}
	if kinds["video"] != 1 {
		t.Errorf("video matches = %d, want 1", kinds["video"])
	}
	for _, a := range got {
		if !filepath.IsAbs(a.Path) {
			t.Errorf("artifact path %q is not absolute", a.Path)
		}
	}
}

// Relative globs still resolve against the workdir, absolute ones against
// their own prefix, in the same request.
func TestListFilesMixedGlobs(t *testing.T) {
	work := t.TempDir()
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(work, "train.log"), []byte("l"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "final.ckpt"), []byte("c"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := &Manager{Root: t.TempDir()}
	got := m.ListFiles(run(work), []kindGlob{
		{"log", "*.log"},
		{"ckpt", filepath.Join(other, "*.ckpt")},
	})
	if len(got) != 2 {
		t.Fatalf("got %d artifacts, want 2: %+v", len(got), got)
	}
}

// The filesystem a job writes checkpoints to is usually not the one the
// agent lives on, and the question "will the next run fit" is asked after
// the last one ended -- so a finished run still contributes its paths.
func TestDiskPathsIncludesFinishedRuns(t *testing.T) {
	m := &Manager{Root: t.TempDir(), runs: map[string]*Run{}}
	done := run("/vepfs/proj")
	done.st.RunID = "a"
	done.st.Phase = v1.PhaseSucceeded
	done.st.Spec.Outputs = &v1.Outputs{Checkpoints: "/vepfs/proj/outputs/**/*.safetensors"}
	m.runs["a"] = done

	paths := m.DiskPaths()
	want := map[string]bool{m.Root: false, "/vepfs/proj": false, "/vepfs/proj/outputs": false}
	for _, p := range paths {
		if _, ok := want[p]; ok {
			want[p] = true
		}
	}
	for p, found := range want {
		if !found {
			t.Errorf("DiskPaths missing %q; got %v", p, paths)
		}
	}
}
