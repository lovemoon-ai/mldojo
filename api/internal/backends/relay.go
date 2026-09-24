package backends

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"
)

// Relay pipes byte streams between agents through the server (dataset
// sync: source agent PUTs a tar, target agent GETs it) and receives uploads
// from agents (artifact fetch). Both sides dial the server, so this works
// for nodes behind bastions that cannot reach each other.
type Relay struct {
	mu      sync.Mutex
	pipes   map[string]*pipe
	uploads map[string]*upload
}

type pipe struct {
	pr     *io.PipeReader
	pw     *io.PipeWriter
	nodes  [2]string // allowed source, target
	putErr chan error
	getErr chan error
}

type upload struct {
	node string
	dest string
	done chan error
}

func NewRelay() *Relay {
	return &Relay{pipes: map[string]*pipe{}, uploads: map[string]*upload{}}
}

func newID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// NewPipe registers a relay from source node to target node.
func (r *Relay) NewPipe(source, target string) (id string, wait func(ctx context.Context) error) {
	pr, pw := io.Pipe()
	p := &pipe{pr: pr, pw: pw, nodes: [2]string{source, target}, putErr: make(chan error, 1), getErr: make(chan error, 1)}
	id = newID()
	r.mu.Lock()
	r.pipes[id] = p
	r.mu.Unlock()
	return id, func(ctx context.Context) error {
		defer func() {
			r.mu.Lock()
			delete(r.pipes, id)
			r.mu.Unlock()
			pr.Close()
			pw.Close()
		}()
		var errs []error
		for i := 0; i < 2; i++ {
			select {
			case err := <-p.putErr:
				if err != nil {
					errs = append(errs, fmt.Errorf("send: %w", err))
					pr.CloseWithError(err)
				}
			case err := <-p.getErr:
				if err != nil {
					errs = append(errs, fmt.Errorf("receive: %w", err))
					pw.CloseWithError(err)
				}
			case <-ctx.Done():
				pr.CloseWithError(ctx.Err())
				pw.CloseWithError(ctx.Err())
				return ctx.Err()
			}
		}
		if len(errs) > 0 {
			return errs[0]
		}
		return nil
	}
}

// HandleRelay serves GET (target) and PUT (source) /agent/relay/{id}.
func (r *Relay) HandleRelay(w http.ResponseWriter, req *http.Request, nodeID, id string) {
	r.mu.Lock()
	p := r.pipes[id]
	r.mu.Unlock()
	if p == nil {
		http.Error(w, "unknown relay", http.StatusNotFound)
		return
	}
	switch req.Method {
	case http.MethodPut:
		if nodeID != p.nodes[0] {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		_, err := io.Copy(p.pw, req.Body)
		p.pw.CloseWithError(err)
		p.putErr <- err
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case http.MethodGet:
		if nodeID != p.nodes[1] {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/x-tar")
		_, err := io.Copy(w, p.pr)
		p.getErr <- err
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// NewUpload registers an upload slot written to dest.
func (r *Relay) NewUpload(node, dest string) (id string, wait func(ctx context.Context) error) {
	u := &upload{node: node, dest: dest, done: make(chan error, 1)}
	id = newID()
	r.mu.Lock()
	r.uploads[id] = u
	r.mu.Unlock()
	return id, func(ctx context.Context) error {
		defer func() {
			r.mu.Lock()
			delete(r.uploads, id)
			r.mu.Unlock()
		}()
		select {
		case err := <-u.done:
			return err
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(30 * time.Minute):
			return fmt.Errorf("upload timed out")
		}
	}
}

// HandleUpload serves PUT /agent/upload/{id}.
func (r *Relay) HandleUpload(w http.ResponseWriter, req *http.Request, nodeID, id string) {
	r.mu.Lock()
	u := r.uploads[id]
	r.mu.Unlock()
	if u == nil || req.Method != http.MethodPut {
		http.Error(w, "unknown upload", http.StatusNotFound)
		return
	}
	if u.node != nodeID {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	tmp := u.dest + ".part"
	f, err := os.Create(tmp)
	if err == nil {
		_, err = io.Copy(f, req.Body)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}
	if err == nil {
		err = os.Rename(tmp, u.dest)
	} else {
		os.Remove(tmp)
	}
	select {
	case u.done <- err:
	default:
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
