// Package logstore is the data plane on the API host: append-only run log
// files, content-addressed blobs (code bundles, patches, fetched artifacts)
// and an in-memory pub/sub broker used by the WebSocket endpoints.
package logstore

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"
)

type Store struct {
	root   string
	mu     sync.Mutex
	locks  map[string]*sync.Mutex
	Broker *Broker
	// OnSegment is called when a new 1 MiB segment of a log starts
	// (used to fill run_logs_index).
	OnSegment func(runID, stream string, offset int64, uri string)
}

const segment = 1 << 20

func New(root string) (*Store, error) {
	for _, d := range []string{"runs", "blobs", "cache", "tmp"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			return nil, err
		}
	}
	return &Store{root: root, locks: map[string]*sync.Mutex{}, Broker: NewBroker()}, nil
}

func (s *Store) Root() string { return s.root }

var safeRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

func (s *Store) logPath(runID, stream string) (string, error) {
	if !safeRe.MatchString(runID) || !safeRe.MatchString(stream) {
		return "", fmt.Errorf("invalid run/stream")
	}
	return filepath.Join(s.root, "runs", runID, stream+".log"), nil
}

func (s *Store) lock(key string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.locks[key]
	if !ok {
		l = &sync.Mutex{}
		s.locks[key] = l
	}
	return l
}

// Size returns the number of bytes stored for a stream.
func (s *Store) Size(runID, stream string) int64 {
	p, err := s.logPath(runID, stream)
	if err != nil {
		return 0
	}
	st, err := os.Stat(p)
	if err != nil {
		return 0
	}
	return st.Size()
}

// AppendAt writes data that starts at byte offset off of the stream. Bytes
// already stored are skipped (idempotent re-delivery after reconnects); a gap
// is recorded with a marker. It returns the new size.
func (s *Store) AppendAt(runID, stream string, off int64, data []byte) (int64, error) {
	p, err := s.logPath(runID, stream)
	if err != nil {
		return 0, err
	}
	l := s.lock(runID + "/" + stream)
	l.Lock()
	defer l.Unlock()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return 0, err
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	st, _ := f.Stat()
	size := st.Size()
	if off >= 0 {
		if off+int64(len(data)) <= size {
			return size, nil // duplicate
		}
		if off < size {
			data = data[size-off:]
		} else if off > size {
			marker := fmt.Sprintf("\n[mldojo] ... %d bytes missing ...\n", off-size)
			data = append([]byte(marker), data...)
		}
	}
	if len(data) == 0 {
		return size, nil
	}
	if _, err := f.Write(data); err != nil {
		return size, err
	}
	newSize := size + int64(len(data))
	if s.OnSegment != nil && (size == 0 || size/segment != newSize/segment) {
		segOff := (newSize / segment) * segment
		if size == 0 {
			segOff = 0
		}
		s.OnSegment(runID, stream, segOff, fmt.Sprintf("file://%s#%d", p, segOff))
	}
	s.Broker.Publish(LogTopic(runID), nil)
	return newSize, nil
}

// Append appends at the current end.
func (s *Store) Append(runID, stream string, data []byte) (int64, error) {
	return s.AppendAt(runID, stream, -1, data)
}

// ReadAt reads up to limit bytes from off.
func (s *Store) ReadAt(runID, stream string, off, limit int64) ([]byte, int64, error) {
	p, err := s.logPath(runID, stream)
	if err != nil {
		return nil, 0, err
	}
	f, err := os.Open(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	st, _ := f.Stat()
	size := st.Size()
	if off < 0 {
		off = 0
	}
	if off >= size {
		return nil, size, nil
	}
	n := size - off
	if limit > 0 && n > limit {
		n = limit
	}
	buf := make([]byte, n)
	_, err = f.ReadAt(buf, off)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, size, err
	}
	return buf, size, nil
}

// Tail returns the last n bytes of a stream.
func (s *Store) Tail(runID, stream string, n int64) ([]byte, error) {
	size := s.Size(runID, stream)
	off := size - n
	if off < 0 {
		off = 0
	}
	b, _, err := s.ReadAt(runID, stream, off, n)
	return b, err
}

// Blobs -------------------------------------------------------------------

var shaRe = regexp.MustCompile(`^[a-f0-9]{64}$`)

func (s *Store) BlobPath(sha string) (string, error) {
	if !shaRe.MatchString(sha) {
		return "", fmt.Errorf("invalid blob id")
	}
	return filepath.Join(s.root, "blobs", sha[:2], sha), nil
}

// PutBlob stores r content-addressed and returns (sha256, size).
func (s *Store) PutBlob(r io.Reader, maxBytes int64) (string, int64, error) {
	tmp, err := os.CreateTemp(filepath.Join(s.root, "tmp"), "blob-*")
	if err != nil {
		return "", 0, err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	lr := io.LimitReader(r, maxBytes+1)
	n, err := io.Copy(io.MultiWriter(tmp, h), lr)
	tmp.Close()
	if err != nil {
		return "", 0, err
	}
	if n > maxBytes {
		return "", 0, fmt.Errorf("blob larger than %d bytes", maxBytes)
	}
	sha := hex.EncodeToString(h.Sum(nil))
	dst, _ := s.BlobPath(sha)
	if _, err := os.Stat(dst); err == nil {
		return sha, n, nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", 0, err
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		return "", 0, err
	}
	return sha, n, nil
}

// OpenBlob opens a blob by sha or "blob://sha".
func (s *Store) OpenBlob(ref string) (*os.File, error) {
	sha := BlobSHA(ref)
	p, err := s.BlobPath(sha)
	if err != nil {
		return nil, err
	}
	return os.Open(p)
}

// BlobSHA strips the blob:// scheme.
func BlobSHA(ref string) string {
	if len(ref) > 7 && ref[:7] == "blob://" {
		return ref[7:]
	}
	return ref
}

// CachePath returns a path in the artifact cache for a key.
func (s *Store) CachePath(key string) string {
	h := sha256.Sum256([]byte(key))
	return filepath.Join(s.root, "cache", hex.EncodeToString(h[:]))
}

// TempFile creates a temp file in the store's tmp dir.
func (s *Store) TempFile(pattern string) (*os.File, error) {
	return os.CreateTemp(filepath.Join(s.root, "tmp"), pattern)
}

// ---- reclaiming disk ------------------------------------------------------

// RemoveRun deletes a run's log directory. Callers delete the database row
// first; a leftover directory would be reclaimed by GC anyway.
func (s *Store) RemoveRun(runID string) error {
	if !safeRe.MatchString(runID) {
		return fmt.Errorf("invalid run id")
	}
	return os.RemoveAll(filepath.Join(s.root, "runs", runID))
}

// GCStats reports what a GC pass reclaimed.
type GCStats struct {
	Runs, Blobs, Cache, Tmp int
	Bytes                   int64
}

// GC reclaims disk that nothing refers to any more: log directories of runs
// that no longer exist, blobs no run points at, and stale cache/tmp entries.
// Nothing here used to be cleaned up, so the data directory only ever grew
// and a full disk took the whole platform down.
//
// liveRun and liveBlob answer "is this still referenced?"; both are consulted
// for every candidate, so they should be backed by a set, not a query.
func (s *Store) GC(liveRun, liveBlob func(string) bool, cacheMaxAge, tmpMaxAge time.Duration) (GCStats, error) {
	var st GCStats
	now := time.Now()

	drop := func(path string, isDir bool) {
		size := int64(0)
		if isDir {
			filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
				if err == nil && !d.IsDir() {
					if fi, err := d.Info(); err == nil {
						size += fi.Size()
					}
				}
				return nil
			})
		} else if fi, err := os.Stat(path); err == nil {
			size = fi.Size()
		}
		if err := os.RemoveAll(path); err != nil {
			slog.Warn("gc: remove", "path", path, "err", err)
			return
		}
		st.Bytes += size
	}

	if ents, err := os.ReadDir(filepath.Join(s.root, "runs")); err == nil {
		for _, e := range ents {
			if !e.IsDir() || liveRun(e.Name()) {
				continue
			}
			drop(filepath.Join(s.root, "runs", e.Name()), true)
			st.Runs++
		}
	}
	if ents, err := os.ReadDir(filepath.Join(s.root, "blobs")); err == nil {
		for _, e := range ents {
			if e.IsDir() || liveBlob(e.Name()) {
				continue
			}
			// A blob is uploaded before the run row that points at it, so
			// keep recent ones regardless of what the database says.
			if fi, err := e.Info(); err == nil && now.Sub(fi.ModTime()) < time.Hour {
				continue
			}
			drop(filepath.Join(s.root, "blobs", e.Name()), false)
			st.Blobs++
		}
	}
	// cache/ holds artifacts re-fetchable from their node, tmp/ holds
	// in-flight uploads: both are safe to drop once cold.
	for _, d := range []struct {
		name string
		age  time.Duration
		n    *int
	}{{"cache", cacheMaxAge, &st.Cache}, {"tmp", tmpMaxAge, &st.Tmp}} {
		if d.age <= 0 {
			continue
		}
		ents, err := os.ReadDir(filepath.Join(s.root, d.name))
		if err != nil {
			continue
		}
		for _, e := range ents {
			fi, err := e.Info()
			if err != nil || now.Sub(fi.ModTime()) < d.age {
				continue
			}
			drop(filepath.Join(s.root, d.name, e.Name()), e.IsDir())
			*d.n++
		}
	}
	return st, nil
}

// Usage reports the total size of the data directory, for disk watermarks.
func (s *Store) Usage() int64 {
	var total int64
	filepath.WalkDir(s.root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if fi, err := d.Info(); err == nil {
				total += fi.Size()
			}
		}
		return nil
	})
	return total
}
