package logstore

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func write(t *testing.T, path, body string, age time.Duration) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if age > 0 {
		old := time.Now().Add(-age)
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestGCDropsOnlyUnreferenced(t *testing.T) {
	root := t.TempDir()
	s, err := New(root)
	if err != nil {
		t.Fatal(err)
	}

	write(t, filepath.Join(root, "runs", "live-run", "stdout.log"), "keep me", 0)
	write(t, filepath.Join(root, "runs", "dead-run", "stdout.log"), "drop me", 0)
	write(t, filepath.Join(root, "blobs", "aaaa"), "referenced bundle", 2*time.Hour)
	write(t, filepath.Join(root, "blobs", "bbbb"), "orphan bundle", 2*time.Hour)
	// A blob uploaded moments ago has no run pointing at it yet.
	write(t, filepath.Join(root, "blobs", "cccc"), "just uploaded", 0)
	write(t, filepath.Join(root, "cache", "cold"), "stale artifact", 30*24*time.Hour)
	write(t, filepath.Join(root, "cache", "warm"), "fresh artifact", 0)
	write(t, filepath.Join(root, "tmp", "leftover"), "partial upload", 48*time.Hour)

	live := map[string]bool{"live-run": true}
	blobs := map[string]bool{"aaaa": true}
	st, err := s.GC(func(id string) bool { return live[id] },
		func(sha string) bool { return blobs[sha] }, 7*24*time.Hour, 24*time.Hour)
	if err != nil {
		t.Fatalf("gc: %v", err)
	}

	for _, keep := range []string{
		filepath.Join(root, "runs", "live-run", "stdout.log"),
		filepath.Join(root, "blobs", "aaaa"),
		filepath.Join(root, "blobs", "cccc"),
		filepath.Join(root, "cache", "warm"),
	} {
		if !exists(keep) {
			t.Errorf("%s was removed but is still referenced or too recent", keep)
		}
	}
	for _, gone := range []string{
		filepath.Join(root, "runs", "dead-run"),
		filepath.Join(root, "blobs", "bbbb"),
		filepath.Join(root, "cache", "cold"),
		filepath.Join(root, "tmp", "leftover"),
	} {
		if exists(gone) {
			t.Errorf("%s survived GC but nothing refers to it", gone)
		}
	}
	if st.Runs != 1 || st.Blobs != 1 || st.Cache != 1 || st.Tmp != 1 {
		t.Errorf("stats = %+v, want one of each", st)
	}
	if st.Bytes == 0 {
		t.Error("reclaimed 0 bytes, want the size of what was dropped")
	}
}

func TestRemoveRunRejectsTraversal(t *testing.T) {
	root := t.TempDir()
	s, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(root, "runs", "keep", "stdout.log"), "x", 0)
	if err := s.RemoveRun("../runs"); err == nil {
		t.Fatal("RemoveRun accepted a traversing id")
	}
	if !exists(filepath.Join(root, "runs", "keep", "stdout.log")) {
		t.Fatal("a traversing id deleted real data")
	}
	if err := s.RemoveRun("keep"); err != nil {
		t.Fatalf("RemoveRun: %v", err)
	}
	if exists(filepath.Join(root, "runs", "keep")) {
		t.Error("RemoveRun left the directory behind")
	}
}
