package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/lovemoon-ai/mldojo/api/internal/logstore"
	v1 "github.com/lovemoon-ai/mldojo/proto/mldojo/v1"
)

// CheckOrigin is handled by the caller: gorilla's hook has no access to the
// server's configured origins, so every upgrade goes through s.upgrade below.
var upgrader = websocket.Upgrader{
	ReadBufferSize:  4 << 10,
	WriteBufferSize: 64 << 10,
	CheckOrigin:     func(*http.Request) bool { return true },
}

// upgrade refuses cross-origin WebSocket handshakes. Accepting any Origin let
// a sibling site under the same registrable domain open a socket with the
// session cookie attached and read any run's logs and metrics.
func (s *Server) upgrade(w http.ResponseWriter, r *http.Request) (*websocket.Conn, error) {
	if !s.sameOrigin(r) {
		writeJSON(w, 403, v1.Error{Error: "cross-origin websocket refused", Code: "forbidden"})
		return nil, errCrossOrigin
	}
	return upgrader.Upgrade(w, r, nil)
}

var errCrossOrigin = errors.New("cross-origin websocket refused")

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

// frameWriter serializes Frames on a WebSocket.
type frameWriter struct {
	ws  *websocket.Conn
	mu  sync.Mutex
	seq int64
}

func (f *frameWriter) send(kind string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	f.ws.SetWriteDeadline(time.Now().Add(30 * time.Second))
	return f.ws.WriteJSON(v1.Frame{Seq: f.seq, TS: time.Now(), Kind: kind, Payload: b})
}

// clientGone returns a context cancelled when the peer closes the socket.
func clientGone(ws *websocket.Conn) context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		defer cancel()
		for {
			if _, _, err := ws.ReadMessage(); err != nil {
				return
			}
		}
	}()
	return ctx
}

// runLogsWS streams log frames: history from offset, then live appends.
func (s *Server) runLogsWS(w http.ResponseWriter, r *http.Request) error {
	run, err := s.run(r)
	if err != nil {
		return err
	}
	q := r.URL.Query()
	streams := []string{q.Get("stream")}
	switch streams[0] {
	case "", "all":
		streams = []string{v1.StreamStdout, v1.StreamStderr, v1.StreamSystem}
	default:
		if !validStream(streams[0]) {
			return badRequest("stream must be stdout|stderr|system|all")
		}
	}
	follow := q.Get("follow") != "0" && q.Get("follow") != "false"
	offsets := map[string]int64{}
	for _, st := range streams {
		off, _ := strconv.ParseInt(q.Get("offset"), 10, 64)
		if v := q.Get("offset_" + st); v != "" {
			off, _ = strconv.ParseInt(v, 10, 64)
		}
		if tail, _ := strconv.ParseInt(q.Get("tail"), 10, 64); tail > 0 {
			off = s.RT.Logs.Size(run.ID, st) - tail
			if off < 0 {
				off = 0
			}
		}
		offsets[st] = off
	}
	ws, err := s.upgrade(w, r)
	if err != nil {
		return nil
	}
	defer ws.Close()
	fw := &frameWriter{ws: ws}
	ctx := clientGone(ws)
	logCh, unsub := s.RT.Logs.Broker.Subscribe(logstore.LogTopic(run.ID))
	defer unsub()
	stCh, unsub2 := s.RT.Logs.Broker.Subscribe(logstore.StatusTopic(run.ID))
	defer unsub2()

	flush := func() (bool, error) {
		sent := false
		for _, st := range streams {
			for {
				data, _, err := s.RT.Logs.ReadAt(run.ID, st, offsets[st], 256<<10)
				if err != nil || len(data) == 0 {
					break
				}
				if err := fw.send("log", v1.LogPayload{Stream: st, Offset: offsets[st], Data: string(data)}); err != nil {
					return sent, err
				}
				offsets[st] += int64(len(data))
				sent = true
			}
		}
		return sent, nil
	}
	if _, err := flush(); err != nil {
		return nil
	}
	fw.send("status", run)
	terminal := v1.Terminal(run.Status)
	if !follow {
		fw.send("eof", map[string]any{})
		return nil
	}
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	var drain <-chan time.Time
	if terminal {
		drain = time.After(2 * time.Second)
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-logCh:
			if _, err := flush(); err != nil {
				return nil
			}
		case v := <-stCh:
			if rr, ok := v.(*v1.Run); ok {
				fw.send("status", rr)
				if v1.Terminal(rr.Status) && drain == nil {
					// Give the agent a moment to ship the final bytes.
					drain = time.After(3 * time.Second)
				}
			}
		case <-drain:
			flush()
			fw.send("eof", map[string]any{})
			ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "eof"), time.Now().Add(time.Second))
			return nil
		case <-ping.C:
			fw.mu.Lock()
			err := ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second))
			fw.mu.Unlock()
			if err != nil {
				return nil
			}
		}
	}
}

// replayBudget spreads the same total budget the REST endpoint uses across
// a run's keys, so an unfiltered replay cannot be tens of megabytes either.
func replayBudget(ctx context.Context, s *Server, runID, key string) int {
	if key != "" {
		return 0 // a named curve gets the full per-key budget
	}
	keys, err := s.store().MetricKeys(ctx, runID)
	if err != nil || len(keys) == 0 {
		return 0
	}
	if n := totalMetricBudget / len(keys); n > minPointsPerKey {
		return n
	}
	return minPointsPerKey
}

func (s *Server) runMetricsWS(w http.ResponseWriter, r *http.Request) error {
	run, err := s.run(r)
	if err != nil {
		return err
	}
	since, _ := strconv.ParseInt(strings.TrimPrefix(r.URL.Query().Get("since"), "step:"), 10, 64)
	key := r.URL.Query().Get("key")
	ws, err := s.upgrade(w, r)
	if err != nil {
		return nil
	}
	defer ws.Close()
	fw := &frameWriter{ws: ws}
	ctx := clientGone(ws)
	ch, unsub := s.RT.Logs.Broker.Subscribe(logstore.MetricTopic(run.ID))
	defer unsub()
	// The replay exists so a client that already has a snapshot does not
	// miss what landed between fetching it and subscribing. A client that
	// loads its history another way says so, rather than being sent every
	// point of every key a second time -- 335 keys is ~10 MB of replay.
	if r.URL.Query().Get("live_only") != "1" {
		pts, _, err := s.store().ListMetrics(ctx, run.ID, key, since, replayBudget(ctx, s, run.ID, key))
		if err != nil {
			fw.send("error", map[string]string{"error": err.Error()})
			return nil
		}
		for i := 0; i < len(pts); i += 5000 {
			end := min(i+5000, len(pts))
			if err := fw.send("metric", pts[i:end]); err != nil {
				return nil
			}
		}
	}
	keys := map[string]bool{}
	for _, k := range strings.Split(key, ",") {
		if k != "" {
			keys[k] = true
		}
	}
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case v := <-ch:
			batch, _ := v.([]v1.MetricPoint)
			if len(keys) > 0 {
				var f []v1.MetricPoint
				for _, p := range batch {
					if keys[p.Key] {
						f = append(f, p)
					}
				}
				batch = f
			}
			if len(batch) > 0 {
				if err := fw.send("metric", batch); err != nil {
					return nil
				}
			}
		case <-ping.C:
			fw.mu.Lock()
			err := ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second))
			fw.mu.Unlock()
			if err != nil {
				return nil
			}
		}
	}
}

func (s *Server) nodeGPUWS(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("id")
	if _, err := s.store().GetNode(r.Context(), id); err != nil {
		return err
	}
	ws, err := s.upgrade(w, r)
	if err != nil {
		return nil
	}
	defer ws.Close()
	fw := &frameWriter{ws: ws}
	ctx := clientGone(ws)
	ch, unsub := s.RT.Logs.Broker.Subscribe(logstore.GPUTopic(id))
	defer unsub()
	g := s.Node.Hub.GPU(id)
	if g == nil {
		g = []v1.GPUStat{}
	}
	if err := fw.send("gpu", g); err != nil {
		return nil
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case v := <-ch:
			if err := fw.send("gpu", v); err != nil {
				return nil
			}
		case <-time.After(30 * time.Second):
			fw.mu.Lock()
			err := ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second))
			fw.mu.Unlock()
			if err != nil {
				return nil
			}
		}
	}
}
