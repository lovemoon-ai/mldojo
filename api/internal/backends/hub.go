package backends

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/lovemoon-ai/mldojo/api/internal/logstore"
	"github.com/lovemoon-ai/mldojo/api/internal/models"
	"github.com/lovemoon-ai/mldojo/api/internal/obs"
	"github.com/lovemoon-ai/mldojo/internal/version"
	v1 "github.com/lovemoon-ai/mldojo/proto/mldojo/v1"
)

// SampleInterval is how often a node's telemetry is written down. One
// minute keeps a week of a 29-GPU cluster in the low millions of rows while
// still resolving the things worth seeing: a stalled dataloader, a card held
// at 0%, a checkpoint filling a disk.
var SampleInterval = time.Minute

// Hub owns the reverse connections from agents.
type Hub struct {
	rt *Runtime

	mu      sync.Mutex
	conns   map[string]*AgentConn
	pending map[string]*pendingReg
	gpu     map[string][]v1.GPUStat
	disks   map[string][]v1.DiskStat
	sampled map[string]time.Time
	runNode sync.Map // run id -> node id (authorization cache)

	// OnOnline is called (in a goroutine) after an agent says hello.
	OnOnline func(nodeID string, hello v1.Hello)
}

type pendingReg struct {
	tokenHash string
	hello     chan v1.Hello
}

func NewHub(rt *Runtime) *Hub {
	return &Hub{rt: rt, conns: map[string]*AgentConn{}, pending: map[string]*pendingReg{},
		gpu: map[string][]v1.GPUStat{}, disks: map[string][]v1.DiskStat{}, sampled: map[string]time.Time{}}
}

// AgentConn is one live agent connection.
type AgentConn struct {
	NodeID string
	Hello  v1.Hello
	ws     *websocket.Conn
	wmu    sync.Mutex
	rmu    sync.Mutex
	reqs   map[string]chan v1.Reply
	seq    atomic.Int64
	done   chan struct{}
}

func (c *AgentConn) send(m v1.AgentMsg) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	c.ws.SetWriteDeadline(time.Now().Add(30 * time.Second))
	return c.ws.WriteJSON(m)
}

// Request sends a command and waits for the agent's reply.
func (c *AgentConn) Request(ctx context.Context, typ string, payload any) (json.RawMessage, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	id := fmt.Sprintf("s%d", c.seq.Add(1))
	ch := make(chan v1.Reply, 1)
	c.rmu.Lock()
	c.reqs[id] = ch
	c.rmu.Unlock()
	defer func() {
		c.rmu.Lock()
		delete(c.reqs, id)
		c.rmu.Unlock()
	}()
	if err := c.send(v1.AgentMsg{Type: typ, ID: id, Payload: b}); err != nil {
		return nil, Unreachablef("agent %s: %v", c.NodeID, err)
	}
	select {
	case r := <-ch:
		if !r.OK {
			return nil, fmt.Errorf("agent %s: %s", c.NodeID, r.Error)
		}
		return r.Data, nil
	case <-c.done:
		return nil, Unreachablef("agent %s disconnected", c.NodeID)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Conn returns the live connection of a node, or nil.
func (h *Hub) Conn(nodeID string) *AgentConn {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.conns[nodeID]
}

func (h *Hub) Online() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.conns)
}

// GPU returns the latest GPU stats of a node.
func (h *Hub) GPU(nodeID string) []v1.GPUStat {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]v1.GPUStat(nil), h.gpu[nodeID]...)
}

// Disks returns the node's latest filesystem readings.
func (h *Hub) Disks(nodeID string) []v1.DiskStat {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]v1.DiskStat(nil), h.disks[nodeID]...)
}

// GPUCounts totals the GPUs of every connected agent, and how many of them
// carry an MLDojo run. Busy counts assignment, not utilisation: a card a
// stranger is using outside the platform is not one we handed out.
func (h *Hub) GPUCounts() (total, busy int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, gs := range h.gpu {
		total += len(gs)
		for _, g := range gs {
			if len(g.RunIDs) > 0 {
				busy++
			}
		}
	}
	return total, busy
}

// NewToken returns a random agent token and its hash.
func NewToken() (tok, hash string) {
	b := make([]byte, 32)
	rand.Read(b)
	tok = hex.EncodeToString(b)
	return tok, HashToken(tok)
}

func HashToken(tok string) string {
	s := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(s[:])
}

// ExpectRegistration lets a not-yet-stored node connect with tokenHash; the
// returned channel receives its hello.
func (h *Hub) ExpectRegistration(nodeID, tokenHash string) (<-chan v1.Hello, func()) {
	p := &pendingReg{tokenHash: tokenHash, hello: make(chan v1.Hello, 1)}
	h.mu.Lock()
	h.pending[nodeID] = p
	h.mu.Unlock()
	return p.hello, func() {
		h.mu.Lock()
		if h.pending[nodeID] == p {
			delete(h.pending, nodeID)
		}
		h.mu.Unlock()
	}
}

// Disconnect closes a node's connection (node removal).
func (h *Hub) Disconnect(nodeID string) {
	h.mu.Lock()
	c := h.conns[nodeID]
	h.mu.Unlock()
	if c != nil {
		c.send(v1.AgentMsg{Type: v1.MsgShutdown})
		c.ws.Close()
	}
}

// Agents are not browsers and never send an Origin, so anything that does is
// a page trying to speak the agent protocol with the visitor's cookies.
var upgrader = websocket.Upgrader{
	ReadBufferSize:  64 << 10,
	WriteBufferSize: 64 << 10,
	CheckOrigin:     func(r *http.Request) bool { return r.Header.Get("Origin") == "" },
}

// AuthAgent validates an agent token for a node (stored or pending).
func (h *Hub) AuthAgent(ctx context.Context, nodeID, token string) (pending *pendingReg, ok bool) {
	if nodeID == "" || token == "" {
		return nil, false
	}
	hash := HashToken(token)
	h.mu.Lock()
	p := h.pending[nodeID]
	h.mu.Unlock()
	if p != nil && subtle.ConstantTimeCompare([]byte(p.tokenHash), []byte(hash)) == 1 {
		return p, true
	}
	stored, err := h.rt.Store.NodeTokenHash(ctx, nodeID)
	if err != nil || stored == "" {
		return nil, false
	}
	return nil, subtle.ConstantTimeCompare([]byte(stored), []byte(hash)) == 1
}

// AgentToken extracts the bearer token from a request.
func AgentToken(r *http.Request) string {
	if a := r.Header.Get("Authorization"); strings.HasPrefix(a, "Bearer ") {
		return strings.TrimPrefix(a, "Bearer ")
	}
	return r.URL.Query().Get("token")
}

// HandleConnect is GET /api/v1/agent/connect.
func (h *Hub) HandleConnect(w http.ResponseWriter, r *http.Request) {
	nodeID := r.Header.Get("X-Mldojo-Node")
	pending, ok := h.AuthAgent(r.Context(), nodeID, AgentToken(r))
	if !ok {
		http.Error(w, `{"error":"invalid agent credentials","code":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	ws.SetReadLimit(32 << 20)
	c := &AgentConn{NodeID: nodeID, ws: ws, reqs: map[string]chan v1.Reply{}, done: make(chan struct{})}
	defer ws.Close()

	ws.SetReadDeadline(time.Now().Add(20 * time.Second))
	var first v1.AgentMsg
	if err := ws.ReadJSON(&first); err != nil || first.Type != v1.MsgHello {
		slog.Warn("agent: expected hello", "node", nodeID, "err", err)
		return
	}
	if err := json.Unmarshal(first.Payload, &c.Hello); err != nil {
		return
	}
	ctx := context.Background()

	// Replace any previous connection of this node.
	h.mu.Lock()
	old := h.conns[nodeID]
	h.conns[nodeID] = c
	h.mu.Unlock()
	if old != nil {
		old.ws.Close()
	}
	defer func() {
		close(c.done)
		h.mu.Lock()
		if h.conns[nodeID] == c {
			delete(h.conns, nodeID)
			delete(h.gpu, nodeID)
			delete(h.disks, nodeID)
			h.mu.Unlock()
			h.rt.Store.UpdateNodeAgent(ctx, nodeID, "offline", "", nil, nil)
			h.rt.Logs.Broker.Publish(logstore.AgentTopic(nodeID), "offline")
			slog.Info("agent disconnected", "node", nodeID)
			// Keyed per node, so a flapping link is one message per
			// dedup window and not one per reconnect.
			obs.Alert(obs.Event{Type: obs.EventNodeOffline, Level: "warning",
				Title: "node offline: " + nodeID,
				Text:  "the agent connection dropped; its runs keep going until the reaper's grace window expires",
				Fields: map[string]string{"node": nodeID, "agent_version": c.Hello.Version,
					"gpus": strconv.Itoa(len(c.Hello.Capacity.GPUs))},
				Key: obs.EventNodeOffline + "/" + nodeID})
		} else {
			h.mu.Unlock()
		}
	}()

	welcome := h.welcome(ctx, nodeID, c.Hello)
	wb, _ := json.Marshal(welcome)
	if err := c.send(v1.AgentMsg{Type: v1.MsgWelcome, Payload: wb}); err != nil {
		return
	}
	now := time.Now()
	capa := c.Hello.Capacity
	h.rt.Store.UpdateNodeAgent(ctx, nodeID, "online", c.Hello.Version, &capa, &now)
	h.rt.Store.UpdateNodeRoots(ctx, nodeID, c.Hello.WorkdirRoot, c.Hello.DatasetsRoot)
	slog.Info("agent connected", "node", nodeID, "version", c.Hello.Version, "gpus", len(capa.GPUs), "runs", len(c.Hello.Runs))
	if pending != nil {
		select {
		case pending.hello <- c.Hello:
		default:
		}
	}
	h.rt.Logs.Broker.Publish(logstore.AgentTopic(nodeID), "online")
	if h.OnOnline != nil {
		go h.OnOnline(nodeID, c.Hello)
	}

	// Keepalive: ping every 20s, drop after 60s of silence.
	ws.SetReadDeadline(time.Now().Add(60 * time.Second))
	ws.SetPongHandler(func(string) error { ws.SetReadDeadline(time.Now().Add(60 * time.Second)); return nil })
	go func() {
		t := time.NewTicker(20 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-c.done:
				return
			case <-t.C:
				c.wmu.Lock()
				err := ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second))
				c.wmu.Unlock()
				if err != nil {
					ws.Close()
					return
				}
			}
		}
	}()
	for {
		var m v1.AgentMsg
		if err := ws.ReadJSON(&m); err != nil {
			if !errors.Is(err, websocket.ErrCloseSent) {
				slog.Debug("agent read", "node", nodeID, "err", err)
			}
			return
		}
		ws.SetReadDeadline(time.Now().Add(60 * time.Second))
		h.handle(ctx, c, m)
	}
}

func (h *Hub) welcome(ctx context.Context, nodeID string, hello v1.Hello) v1.Welcome {
	w := v1.Welcome{ServerVersion: version.Version, LogOffsets: map[string]map[string]int64{}, HeartbeatSec: 5}
	for _, rs := range hello.Runs {
		run, err := h.rt.Store.GetRun(ctx, rs.RunID)
		if err != nil || run.BackendKind != "node" || run.BackendID != nodeID {
			continue
		}
		h.runNode.Store(run.ID, nodeID)
		w.LogOffsets[run.ID] = map[string]int64{
			v1.StreamStdout: h.rt.Logs.Size(run.ID, v1.StreamStdout),
			v1.StreamStderr: h.rt.Logs.Size(run.ID, v1.StreamStderr),
		}
		if run.Status == v1.PhaseCancelled && !v1.Terminal(rs.Phase) {
			w.CancelRuns = append(w.CancelRuns, run.ID)
		}
	}
	return w
}

// ownsRun checks that a run belongs to the node (agents may only report on
// their own runs).
func (h *Hub) ownsRun(ctx context.Context, nodeID, runID string) bool {
	if n, ok := h.runNode.Load(runID); ok {
		return n.(string) == nodeID
	}
	run, err := h.rt.Store.GetRun(ctx, runID)
	if err != nil || len(runID) != 36 {
		return false
	}
	owner := ""
	if run.BackendKind == "node" {
		owner = run.BackendID
	}
	h.runNode.Store(run.ID, owner)
	return owner == nodeID
}

func (h *Hub) handle(ctx context.Context, c *AgentConn, m v1.AgentMsg) {
	switch m.Type {
	case v1.MsgReply:
		var r v1.Reply
		json.Unmarshal(m.Payload, &r)
		c.rmu.Lock()
		ch := c.reqs[m.ID]
		c.rmu.Unlock()
		if ch != nil {
			ch <- r
		}
	case v1.MsgHeartbeat:
		var hb v1.Heartbeat
		if json.Unmarshal(m.Payload, &hb) != nil {
			return
		}
		h.mu.Lock()
		h.gpu[c.NodeID] = hb.GPUs
		if len(hb.Disks) > 0 {
			h.disks[c.NodeID] = hb.Disks
		}
		// Heartbeats arrive every few seconds; history does not need that
		// resolution, and storing it at that rate would dwarf the runs.
		store := time.Since(h.sampled[c.NodeID]) >= SampleInterval
		if store {
			h.sampled[c.NodeID] = time.Now()
		}
		h.mu.Unlock()
		now := time.Now()
		h.rt.Store.UpdateNodeAgent(ctx, c.NodeID, "online", "", nil, &now)
		if store {
			if err := h.rt.Store.InsertGPUSamples(ctx, c.NodeID, now, hb.GPUs); err != nil {
				slog.Warn("store gpu samples", "node", c.NodeID, "err", err)
			}
			if err := h.rt.Store.InsertDiskSamples(ctx, c.NodeID, now, hb.Disks); err != nil {
				slog.Warn("store disk samples", "node", c.NodeID, "err", err)
			}
		}
		h.rt.Logs.Broker.Publish(logstore.GPUTopic(c.NodeID), hb.GPUs)
	case v1.MsgRunStatus:
		var s v1.RunStatusMsg
		if json.Unmarshal(m.Payload, &s) != nil || !h.ownsRun(ctx, c.NodeID, s.RunID) {
			return
		}
		if s.Workdir != "" || s.PID != 0 {
			h.rt.Store.SetRunHandle(ctx, s.RunID, map[string]any{"node_id": c.NodeID, "pid": s.PID, "workdir": s.Workdir, "gpus": s.GPUs})
		}
		if s.EnvLock != "" || s.ImageDigest != "" {
			fields := map[string]any{}
			if s.EnvLock != "" {
				fields["env_lock"] = s.EnvLock
			}
			if s.ImageDigest != "" {
				fields["image_digest"] = s.ImageDigest
			}
			if err := h.rt.Store.MergeRunMetadata(ctx, s.RunID, fields); err != nil {
				slog.Warn("record env lock", "run", s.RunID, "err", err)
			}
		}
		h.rt.SetStatus(ctx, s.RunID, models.RunUpdate{Phase: s.Phase, ExitCode: s.ExitCode, StartedAt: s.StartedAt,
			FinishedAt: s.FinishedAt, Message: s.Message, Commit: s.Commit})
	case v1.MsgLog:
		var l v1.LogMsg
		if json.Unmarshal(m.Payload, &l) != nil || !h.ownsRun(ctx, c.NodeID, l.RunID) {
			return
		}
		if l.Stream != v1.StreamStdout && l.Stream != v1.StreamStderr {
			return
		}
		if _, err := h.rt.Logs.AppendAt(l.RunID, l.Stream, l.Offset, l.Data); err != nil {
			slog.Warn("log append", "run", l.RunID, "err", err)
		}
	case v1.MsgMetrics:
		var mm v1.MetricsMsg
		if json.Unmarshal(m.Payload, &mm) != nil || !h.ownsRun(ctx, c.NodeID, mm.RunID) {
			return
		}
		if err := h.rt.Store.UpsertMetrics(ctx, mm.RunID, mm.Points); err != nil {
			slog.Warn("metrics upsert", "run", mm.RunID, "err", err)
			return
		}
		h.rt.Logs.Broker.Publish(logstore.MetricTopic(mm.RunID), mm.Points)
	case v1.MsgArtifacts:
		var am v1.ArtifactsMsg
		if json.Unmarshal(m.Payload, &am) != nil || !h.ownsRun(ctx, c.NodeID, am.RunID) {
			return
		}
		h.rt.Store.UpsertArtifacts(ctx, am.RunID, toArtifacts(c.NodeID, am.Items))
	case v1.MsgEpisodes:
		var em v1.EpisodesMsg
		if json.Unmarshal(m.Payload, &em) != nil || !h.ownsRun(ctx, c.NodeID, em.RunID) {
			return
		}
		// Video paths arrive as paths on the node; store them the way
		// artifacts are stored, so the same fetch path works.
		for i := range em.Episodes {
			if p := em.Episodes[i].VideoURI; p != "" && !strings.Contains(p, "://") {
				em.Episodes[i].VideoURI = NodeURI(c.NodeID, p)
			}
		}
		if err := h.rt.Store.UpsertEpisodes(ctx, em.RunID, em.Episodes); err != nil {
			slog.Warn("store episodes", "run", em.RunID, "err", err)
			return
		}
		// Register the videos as artifacts too. Fetching one goes through
		// the artifact path, so without this an episode's video is listed
		// and then cannot be played unless outputs.videos happened to
		// cover the same files.
		var vids []v1.Artifact
		for _, e := range em.Episodes {
			if e.VideoURI != "" {
				vids = append(vids, v1.Artifact{Kind: "video", URI: e.VideoURI})
			}
		}
		if len(vids) > 0 {
			h.rt.Store.UpsertArtifacts(ctx, em.RunID, vids)
		}
		// Publish the result as a metric too. Ranking, comparison and the
		// AI summaries then work on an evaluation without knowing anything
		// about episodes.
		sum := v1.Summarize(em.Episodes)
		now := time.Now()
		h.rt.Store.UpsertMetrics(ctx, em.RunID, []v1.MetricPoint{
			{Step: int64(sum.Total), Key: v1.MetricSuccessRate, Value: sum.SuccessRate, TS: now},
			{Step: int64(sum.Total), Key: v1.MetricEpisodes, Value: float64(sum.Total), TS: now},
		})
	}
}

func toArtifacts(nodeID string, items []v1.ArtifactItem) []v1.Artifact {
	out := make([]v1.Artifact, 0, len(items))
	for _, it := range items {
		out = append(out, v1.Artifact{Kind: it.Kind, URI: NodeURI(nodeID, it.Path),
			SizeBytes: it.SizeBytes, SHA256: it.SHA256})
	}
	return out
}

// NodeURI renders node://<node>/<abs path>.
func NodeURI(nodeID, path string) string {
	return "node://" + nodeID + "/" + strings.TrimPrefix(path, "/")
}

// ParseNodeURI splits node://<node>/<path> into node and absolute path.
func ParseNodeURI(uri string) (node, path string, ok bool) {
	rest, found := strings.CutPrefix(uri, "node://")
	if !found {
		return "", "", false
	}
	node, p, found := strings.Cut(rest, "/")
	if !found || node == "" {
		return "", "", false
	}
	return node, "/" + p, true
}
