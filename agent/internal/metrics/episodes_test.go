package metrics

import (
	"os"
	"path/filepath"
	"testing"

	v1 "github.com/lovemoon-ai/mldojo/proto/mldojo/v1"
)

// The manifest the lingbot evaluation actually wrote, verbatim.
const realManifest = `episode,seed,video_result,original_result,duration_seconds,frames,bytes,filename
0,100000,failure,failure,50.0,500,102223,episode0_failure.mp4
1,100001,failure,failure,50.0,500,122315,episode1_failure.mp4
6,100006,success,success,21.4,214,51000,episode6_success.mp4
`

func TestEpisodesFromRealManifest(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "manifest.csv"), []byte(realManifest), 0o644); err != nil {
		t.Fatal(err)
	}
	// The harness writes the videos under episodes/<task>/, not beside the
	// manifest, and the filename column is relative to that.
	vids := filepath.Join(dir, "episodes", "place_empty_cup")
	if err := os.MkdirAll(vids, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"episode0_failure.mp4", "episode1_failure.mp4", "episode6_success.mp4"} {
		if err := os.WriteFile(filepath.Join(vids, n), []byte("v"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	eps := NewScanner().Episodes(dir, []v1.EpisodesSource{{Path: "manifest.csv"}})
	if len(eps) != 3 {
		t.Fatalf("got %d episodes, want 3", len(eps))
	}
	if eps[0].Seed == nil || *eps[0].Seed != 100000 || eps[0].Success {
		t.Errorf("episode 0 = %+v", eps[0])
	}
	last := eps[2]
	if last.Index != 6 || !last.Success || last.Steps != 500 && last.Steps != 214 {
		t.Errorf("episode 6 = %+v", last)
	}
	if last.DurationMS != 21400 {
		t.Errorf("duration = %d ms, want 21400", last.DurationMS)
	}
	if filepath.Base(last.VideoURI) != "episode6_success.mp4" {
		t.Errorf("video = %q", last.VideoURI)
	}
	if s := v1.Summarize(eps); s.Successes != 1 || s.Total != 3 {
		t.Errorf("summary = %+v", s)
	}
}

// A harness that only saves video, with the result in the filename, is the
// cheapest thing to add to an existing script -- and it is what this one did
// first.
func TestEpisodesFromFilenames(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"episode0_failure.mp4", "episode6_success.mp4", "episode13_success.mp4", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	eps := NewScanner().Episodes(dir, []v1.EpisodesSource{{Type: "filenames", Path: "."}})
	if len(eps) != 3 {
		t.Fatalf("got %d episodes, want 3: %+v", len(eps), eps)
	}
	if eps[0].Index != 0 || eps[0].Success {
		t.Errorf("first = %+v", eps[0])
	}
	if eps[2].Index != 13 || !eps[2].Success {
		t.Errorf("last = %+v", eps[2])
	}
}

func TestEpisodesFromJSONL(t *testing.T) {
	dir := t.TempDir()
	body := `{"episode": 0, "seed": 7, "success": true, "steps": 120, "task": "place_empty_cup"}
{"episode": 1, "seed": 8, "success": false, "duration_s": 50}
garbage
`
	if err := os.WriteFile(filepath.Join(dir, "episodes.jsonl"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	eps := NewScanner().Episodes(dir, []v1.EpisodesSource{{Path: "episodes.jsonl"}})
	if len(eps) != 2 {
		t.Fatalf("got %d episodes, want 2", len(eps))
	}
	if !eps[0].Success || eps[0].Steps != 120 || eps[0].Extra["task"] != "place_empty_cup" {
		t.Errorf("first = %+v", eps[0])
	}
	if eps[1].DurationMS != 50000 {
		t.Errorf("second duration = %d", eps[1].DurationMS)
	}
}

// Two sources describing the same episodes must merge, not duplicate: the
// manifest knows the seed, the directory knows the video.
func TestEpisodeSourcesMerge(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "m.csv"),
		[]byte("episode,seed,result\n0,555,failure\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "episode0_failure.mp4"), []byte("v"), 0o644); err != nil {
		t.Fatal(err)
	}
	eps := NewScanner().Episodes(dir, []v1.EpisodesSource{
		{Path: "m.csv"}, {Type: "filenames", Path: "."},
	})
	if len(eps) != 1 {
		t.Fatalf("got %d episodes, want 1 merged", len(eps))
	}
	if eps[0].Seed == nil || *eps[0].Seed != 555 || eps[0].VideoURI == "" {
		t.Errorf("merged = %+v (seed from the csv, video from the directory)", eps[0])
	}
}

// Harnesses write a fresh timestamped directory per run, so a source can
// only name them with a glob.
func TestEpisodeSourcesGlob(t *testing.T) {
	root := t.TempDir()
	for _, run := range []string{"eval_20260919_141341", "eval_20260919_175030"} {
		d := filepath.Join(root, "eval_results", run, "episodes")
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "episode0_success.mp4"), []byte("v"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Only the second run has a manifest, as happens when a harness is
	// interrupted; the glob must still find it.
	if err := os.WriteFile(filepath.Join(root, "eval_results", "eval_20260919_175030", "manifest.csv"),
		[]byte("episode,seed,result\n0,100000,success\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	eps := NewScanner().Episodes(root, []v1.EpisodesSource{
		{Type: "csv", Path: "eval_results/*/manifest.csv"},
		{Type: "filenames", Path: "eval_results/*/episodes"},
	})
	if len(eps) != 1 {
		t.Fatalf("got %d episodes, want 1 (index 0 from both directories, merged): %+v", len(eps), eps)
	}
	if eps[0].Seed == nil || *eps[0].Seed != 100000 {
		t.Errorf("seed should come from the manifest: %+v", eps[0])
	}
	if eps[0].VideoURI == "" {
		t.Errorf("video should come from the directory: %+v", eps[0])
	}
}

// The real manifest's filename column is relative to where the harness put
// the videos -- episodes/<task>/ -- not to the manifest itself. Recording
// the path that does not exist made every episode look playable and then
// fail to play.
func TestManifestVideosLiveInASubdirectory(t *testing.T) {
	root := t.TempDir()
	run := filepath.Join(root, "eval_results", "r1")
	vids := filepath.Join(run, "episodes", "place_empty_cup")
	if err := os.MkdirAll(vids, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(run, "manifest.csv"),
		[]byte("episode,seed,video_result,filename\n0,100000,failure,episode0_failure.mp4\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(vids, "episode0_failure.mp4"), []byte("v"), 0o644); err != nil {
		t.Fatal(err)
	}
	eps := NewScanner().Episodes(root, []v1.EpisodesSource{{Type: "csv", Path: "eval_results/r1/manifest.csv"}})
	if len(eps) != 1 {
		t.Fatalf("got %d episodes", len(eps))
	}
	if eps[0].VideoURI != filepath.Join(vids, "episode0_failure.mp4") {
		t.Errorf("video = %q, want the file that actually exists", eps[0].VideoURI)
	}
}

// A filename nothing on disk matches must leave the field empty rather than
// inventing a path.
func TestManifestVideoThatIsNotThere(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "m.csv"),
		[]byte("episode,result,filename\n0,failure,gone.mp4\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	eps := NewScanner().Episodes(dir, []v1.EpisodesSource{{Type: "csv", Path: "m.csv"}})
	if len(eps) != 1 || eps[0].VideoURI != "" {
		t.Errorf("video = %q, want empty", eps[0].VideoURI)
	}
}
