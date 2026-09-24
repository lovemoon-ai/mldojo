package logging

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func TestFormats(t *testing.T) {
	var buf bytes.Buffer
	Setup(&buf)
	slog.Info("hello", "node", "gpu-a")
	if got := buf.String(); !strings.Contains(got, "msg=hello node=gpu-a") {
		t.Errorf("default format should stay text, got %q", got)
	}

	buf.Reset()
	t.Setenv("MLDOJO_LOG_FORMAT", "json")
	Setup(&buf)
	slog.Info("hello", "node", "gpu-a")
	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("MLDOJO_LOG_FORMAT=json did not produce JSON: %v (%q)", err, buf.String())
	}
	if rec["msg"] != "hello" || rec["node"] != "gpu-a" {
		t.Errorf("fields lost: %v", rec)
	}
}

func TestDebugLevel(t *testing.T) {
	var buf bytes.Buffer
	Setup(&buf)
	slog.Debug("quiet")
	if buf.Len() != 0 {
		t.Errorf("debug should be off by default, got %q", buf.String())
	}
	t.Setenv("MLDOJO_LOG_LEVEL", "debug")
	Setup(&buf)
	slog.Debug("loud")
	if !strings.Contains(buf.String(), "loud") {
		t.Error("MLDOJO_LOG_LEVEL=debug should let debug lines through")
	}
}
