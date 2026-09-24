package backends

import (
	"testing"

	v1 "github.com/lovemoon-ai/mldojo/proto/mldojo/v1"
)

// The shapes the lingbot training run actually wrote: four saves, eight
// shards each, under outputs/<run>/checkpoints/global_step_<n>/hf_ckpt.
func TestNewestCheckpointIsTheDirectory(t *testing.T) {
	const base = "node://h20/root/ws/comp/lingbot_competition/outputs/place_empty_cup_b16_w8_2k/checkpoints"
	var arts []v1.Artifact
	for _, step := range []string{"500", "1000", "1500", "2000"} {
		for _, shard := range []string{"model-00001-of-00003.safetensors", "model-00002-of-00003.safetensors"} {
			arts = append(arts, v1.Artifact{Kind: "ckpt",
				URI: base + "/global_step_" + step + "/hf_ckpt/" + shard, SizeBytes: 1 << 30})
		}
	}
	// A log artifact must not be mistaken for a checkpoint.
	arts = append(arts, v1.Artifact{Kind: "log", URI: base + "/global_step_9999/train.log"})

	v, step := newestCheckpoint(arts)
	if v == nil {
		t.Fatal("no checkpoint found")
	}
	if step != 2000 {
		t.Errorf("step = %d, want 2000 (the step lives in the directory, not the file name)", step)
	}
	if want := base + "/global_step_2000/hf_ckpt"; v.URI != want {
		t.Errorf("uri = %q, want %q", v.URI, want)
	}
	if v.SizeBytes != 2<<30 {
		t.Errorf("size = %d, want the sum of the shards", v.SizeBytes)
	}
	if v.SHA256 != "" {
		t.Errorf("a directory has no checksum, got %q", v.SHA256)
	}
}

// One file is one checkpoint: registering its parent directory would hand an
// evaluation the whole output tree.
func TestNewestCheckpointOfASingleFile(t *testing.T) {
	arts := []v1.Artifact{
		{Kind: "ckpt", URI: "node://gpu-a/home/me/out/step_10.ckpt", SHA256: "abc", SizeBytes: 7},
		{Kind: "ckpt", URI: "node://gpu-a/home/me/out/step_20.ckpt", SHA256: "def", SizeBytes: 9},
	}
	v, step := newestCheckpoint(arts)
	if v == nil || step != 20 {
		t.Fatalf("newestCheckpoint = %+v, step %d", v, step)
	}
	if v.URI != arts[1].URI || v.SHA256 != "def" || v.SizeBytes != 9 {
		t.Errorf("version = %+v", v)
	}
}

func TestNewestCheckpointWithoutCheckpoints(t *testing.T) {
	if v, _ := newestCheckpoint([]v1.Artifact{{Kind: "video", URI: "node://n/a.mp4"}}); v != nil {
		t.Errorf("got %+v, want nil", v)
	}
}
