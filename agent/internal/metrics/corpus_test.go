package metrics

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	v1 "github.com/lovemoon-ai/mldojo/proto/mldojo/v1"
)

// corpus is testdata/real at the repo root: artifacts real jobs wrote, kept
// verbatim. See its README for why synthetic samples were not enough.
const corpus = "../../../testdata/real"

// The tfevents file a real 6B VLA training run wrote, cut at 64 KB. The
// synthetic fixture next door writes one tag with one point; this one writes
// 46 tags whose names are the reason PrimaryMetric needed a tie-break.
func TestRealTensorboard(t *testing.T) {
	pts := NewScanner().ScanTensorboard(filepath.Join(corpus, "events.out.tfevents"))
	keys, maxStep := map[string]int{}, int64(0)
	for _, p := range pts {
		keys[p.Key]++
		if p.Step > maxStep {
			maxStep = p.Step
		}
	}
	if len(keys) != 46 || len(pts) != 1036 {
		t.Fatalf("read %d points over %d keys, want 1036 over 46", len(pts), len(keys))
	}
	// The plain loss has to survive next to a dozen decoys that also end in
	// _loss; picking the first match used to land on router_z_loss.
	for _, k := range []string{"training/loss", "training/vla_loss", "training/router_z_loss",
		"align/future_video_mse_loss", "moe_zloss/weighted", "steptime"} {
		if keys[k] == 0 {
			t.Errorf("key %q missing", k)
		}
	}
	if maxStep != 23 {
		t.Errorf("max step = %d, want 23", maxStep)
	}
	// tfevents-keys.txt is the same key set in a form a package that cannot
	// import this one can read. It is derived, so it has to be checked.
	want, err := os.ReadFile(filepath.Join(corpus, "tfevents-keys.txt"))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for k := range keys {
		got = append(got, k)
	}
	sort.Strings(got)
	if strings.Join(got, "\n")+"\n" != string(want) {
		t.Error("tfevents-keys.txt no longer matches the tfevents file it was taken from")
	}
	// The file ends mid-record, which is what tailing a live run looks like.
	// Reading it twice must not replay or drop anything.
	s := NewScanner()
	first := len(s.ScanTensorboard(filepath.Join(corpus, "events.out.tfevents")))
	if again := len(s.ScanTensorboard(filepath.Join(corpus, "events.out.tfevents"))); again != 0 {
		t.Errorf("rescanning an unchanged file returned %d points (first pass %d)", again, first)
	}
}

// The manifest a real evaluation wrote. Episode index, row number and seed
// are three different things here: the seeds skip 100016 and 100018.
func TestRealEpisodeManifest(t *testing.T) {
	dir := t.TempDir()
	raw, err := os.ReadFile(filepath.Join(corpus, "manifest.csv"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.csv"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	// The harness puts the videos under episodes/, not beside the manifest,
	// and drops a _result.txt in there that is not an episode.
	vids := filepath.Join(dir, "episodes")
	if err := os.MkdirAll(vids, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(vids, "_result.txt"), []byte("6/20\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, e := range []struct {
		i  int
		ok string
	}{{0, "failure"}, {6, "success"}, {8, "success"}, {9, "success"}, {13, "success"}, {14, "success"}, {19, "failure"}} {
		name := filepath.Join(vids, "episode"+strconv.Itoa(e.i)+"_"+e.ok+".mp4")
		if err := os.WriteFile(name, []byte("v"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	eps := NewScanner().Episodes(dir, []v1.EpisodesSource{
		{Type: "csv", Path: "manifest.csv"},
		{Type: "filenames", Path: "episodes"},
	})
	if len(eps) != 20 {
		t.Fatalf("got %d episodes, want 20", len(eps))
	}
	sum := v1.Summarize(eps)
	if sum.Successes != 5 {
		t.Errorf("successes = %d, want 5 (the manifest's video_result column)", sum.Successes)
	}
	// Seed 100016 and 100018 were never run: episode 16 is seed 100017.
	if eps[16].Seed == nil || *eps[16].Seed != 100017 {
		t.Errorf("episode 16 seed = %v, want 100017 -- index is not seed", eps[16].Seed)
	}
	// _result.txt sits next to the videos and must not become a trial.
	for _, e := range eps {
		if e.VideoURI != "" && filepath.Ext(e.VideoURI) != ".mp4" {
			t.Errorf("episode %d took a non-video as its video: %q", e.Index, e.VideoURI)
		}
	}
}
