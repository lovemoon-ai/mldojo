package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestQueuePlugins(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MLDOJO_HOME", home)
	t.Setenv("MLDOJO_QUEUE_PLUGINS", "")
	t.Setenv("MLDOJO_QUEUE_POLL_SEC", "")
	write := func(s string) {
		if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// The pre-plugin spelling keeps working: it is the "aidi" plugin.
	write("api:\n  aidi_sidecar_url: http://127.0.0.1:8766\n  aidi_poll_sec: 7\n")
	f, err := Load()
	if err != nil || f.API.QueuePlugins["aidi"] != "http://127.0.0.1:8766" || f.API.QueuePollSec != 7 {
		t.Fatalf("legacy keys: %+v %v", f.API.QueuePlugins, err)
	}

	write("api:\n  queue_plugins: {a: http://h:1}\n")
	t.Setenv("MLDOJO_QUEUE_PLUGINS", "b=http://h:2, c=http://h:3")
	if f, err = Load(); err != nil || len(f.API.QueuePlugins) != 3 || f.API.QueuePlugins["c"] != "http://h:3" {
		t.Fatalf("queue_plugins + env: %+v %v", f.API.QueuePlugins, err)
	}
}
