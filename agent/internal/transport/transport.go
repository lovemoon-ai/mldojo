// Package transport keeps the agent's reverse WebSocket connection to the
// API server alive (exponential backoff + jitter) and dispatches
// server commands.
package transport

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"github.com/lovemoon-ai/mldojo/agent/internal/exec"
	"github.com/lovemoon-ai/mldojo/agent/internal/gpu"
	msync "github.com/lovemoon-ai/mldojo/agent/internal/sync"
	"github.com/lovemoon-ai/mldojo/internal/version"
	v1 "github.com/lovemoon-ai/mldojo/proto/mldojo/v1"
)

type Agent struct {
	ServerURL    string
	Token        string
	NodeID       string
	WorkdirRoot  string
	DatasetsRoot string
	Runs         *exec.Manager
	Client       *msync.Client
	// TLS is non-nil when the API uses an internal CA or asks for mTLS.
	TLS *tls.Config

	mu   sync.Mutex
	ws   *websocket.Conn
	wmu  sync.Mutex
	stop chan struct{}
}

// Emit sends a message if connected.
func (a *Agent) Emit(typ string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return a.send(v1.AgentMsg{Type: typ, Payload: b})
}

func (a *Agent) send(m v1.AgentMsg) error {
	a.mu.Lock()
	ws := a.ws
	a.mu.Unlock()
	if ws == nil {
		return errors.New("not connected")
	}
	a.wmu.Lock()
	defer a.wmu.Unlock()
	ws.SetWriteDeadline(time.Now().Add(30 * time.Second))
	return ws.WriteJSON(m)
}

func (a *Agent) reply(id string, data any, err error) {
	r := v1.Reply{OK: err == nil}
	if err != nil {
		r.Error = err.Error()
	} else if data != nil {
		r.Data, _ = json.Marshal(data)
	}
	b, _ := json.Marshal(r)
	a.send(v1.AgentMsg{Type: v1.MsgReply, ID: id, Payload: b})
}

// Run loops forever: connect, serve, reconnect.
func (a *Agent) Run(ctx context.Context) {
	a.stop = make(chan struct{})
	go a.background(ctx)
	backoff := time.Second
	for ctx.Err() == nil {
		start := time.Now()
		err := a.session(ctx)
		if errors.Is(err, errShutdown) {
			slog.Info("server asked the agent to shut down")
			return
		}
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		wait := backoff + time.Duration(rand.Int63n(int64(backoff)/2+1))
		slog.Warn("disconnected from server", "err", err, "retry_in", wait.Round(time.Millisecond))
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

var errShutdown = errors.New("shutdown")

func wsURL(server string) string {
	u := strings.TrimRight(server, "/") + "/api/v1/agent/connect"
	if strings.HasPrefix(u, "https://") {
		return "wss://" + strings.TrimPrefix(u, "https://")
	}
	return "ws://" + strings.TrimPrefix(u, "http://")
}

func (a *Agent) capacity() v1.Capacity {
	stats := gpu.Stats(context.Background())
	host, _ := os.Hostname()
	c := v1.Capacity{GPUs: gpu.Inventory(stats), CPU: runtime.NumCPU(), Hostname: host, OS: runtime.GOOS, Arch: runtime.GOARCH}
	if b, err := os.ReadFile("/proc/meminfo"); err == nil {
		var kb int64
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "MemTotal:") {
				fmt.Sscanf(strings.TrimSpace(strings.TrimPrefix(line, "MemTotal:")), "%d", &kb)
			}
		}
		c.MemGB = int(kb / 1024 / 1024)
	}
	var fs syscall.Statfs_t
	if syscall.Statfs(a.WorkdirRoot, &fs) == nil {
		c.DiskGB = int(fs.Bavail * uint64(fs.Bsize) / (1 << 30))
	}
	// Where everything that grows actually lives: ~/.mldojo is a symlink
	// into a data disk on nodes whose home is the small one.
	if home, err := os.UserHomeDir(); err == nil {
		c.MldojoDir, _ = filepath.EvalSymlinks(filepath.Join(home, ".mldojo"))
	}
	return c
}

// statfs reports a directory's filesystem, or false when it cannot be read.
func statfs(path string) (v1.DiskStat, bool) {
	var fs syscall.Statfs_t
	if syscall.Statfs(path, &fs) != nil {
		return v1.DiskStat{}, false
	}
	const gb = float64(1 << 30)
	bs := float64(fs.Bsize)
	return v1.DiskStat{
		Path:    path,
		TotalGB: float64(fs.Blocks) * bs / gb,
		FreeGB:  float64(fs.Bavail) * bs / gb,
	}, true
}

// disks samples the filesystems behind every path the runs care about, one
// entry per distinct filesystem so a node with everything on one disk does
// not report it five times.
func (a *Agent) disks() []v1.DiskStat {
	paths := []string{a.WorkdirRoot, a.DatasetsRoot}
	if a.Runs != nil {
		paths = append(paths, a.Runs.DiskPaths()...)
	}
	seen := map[string]bool{}
	var out []v1.DiskStat
	for _, p := range paths {
		if p == "" {
			continue
		}
		d, ok := statfs(p)
		if !ok {
			continue
		}
		// Two directories on the same filesystem report identical totals;
		// keep the first, which is the more meaningful path.
		key := fmt.Sprintf("%.0f/%.0f", d.TotalGB, d.FreeGB)
		if seen[key] {
			continue
		}
		seen[key] = true
		d.Mount = mountOf(p)
		out = append(out, d)
	}
	return out
}

// mountOf finds the longest mount point prefixing path, for the label only.
func mountOf(path string) string {
	// The workdir root is usually reached through ~/.mldojo, a symlink into
	// a data disk; matching the unresolved path labelled the data disk's
	// numbers with the home disk's mount point.
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
	}
	b, err := os.ReadFile("/proc/self/mounts")
	if err != nil {
		return ""
	}
	best := ""
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		mp := f[1]
		if (path == mp || strings.HasPrefix(path, strings.TrimSuffix(mp, "/")+"/")) && len(mp) > len(best) {
			best = mp
		}
	}
	return best
}

func (a *Agent) session(ctx context.Context) error {
	d := websocket.Dialer{HandshakeTimeout: 15 * time.Second, Proxy: nil, TLSClientConfig: a.TLS}
	h := http.Header{}
	h.Set("Authorization", "Bearer "+a.Token)
	h.Set("X-Mldojo-Node", a.NodeID)
	ws, resp, err := d.DialContext(ctx, wsURL(a.ServerURL), h)
	if err != nil {
		if resp != nil {
			return fmt.Errorf("dial %s: %v (HTTP %d)", a.ServerURL, err, resp.StatusCode)
		}
		return fmt.Errorf("dial %s: %w", a.ServerURL, err)
	}
	defer ws.Close()
	// Reads don't observe ctx: close the socket on shutdown so SIGTERM exits
	// promptly (systemd would otherwise wait for TimeoutStopSec).
	stopClose := make(chan struct{})
	defer close(stopClose)
	go func() {
		select {
		case <-ctx.Done():
			ws.Close()
		case <-stopClose:
		}
	}()
	ws.SetReadLimit(64 << 20)
	hello := v1.Hello{NodeID: a.NodeID, Version: version.Version, ProtocolVersion: v1.AgentProtocolVersion,
		Capacity: a.capacity(), Runs: a.Runs.States(), WorkdirRoot: a.WorkdirRoot, DatasetsRoot: a.DatasetsRoot}
	hb, _ := json.Marshal(hello)
	ws.SetWriteDeadline(time.Now().Add(15 * time.Second))
	if err := ws.WriteJSON(v1.AgentMsg{Type: v1.MsgHello, Payload: hb}); err != nil {
		return err
	}
	ws.SetReadDeadline(time.Now().Add(30 * time.Second))
	var m v1.AgentMsg
	if err := ws.ReadJSON(&m); err != nil {
		return fmt.Errorf("waiting for welcome: %w", err)
	}
	if m.Type != v1.MsgWelcome {
		return fmt.Errorf("unexpected %q instead of welcome", m.Type)
	}
	var welcome v1.Welcome
	json.Unmarshal(m.Payload, &welcome)
	slog.Info("connected", "server", a.ServerURL, "server_version", welcome.ServerVersion, "runs", len(hello.Runs))
	a.mu.Lock()
	a.ws = ws
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		a.ws = nil
		a.mu.Unlock()
	}()
	a.Runs.OnWelcome(welcome)
	ws.SetPingHandler(func(data string) error {
		ws.SetReadDeadline(time.Now().Add(90 * time.Second))
		a.wmu.Lock()
		defer a.wmu.Unlock()
		return ws.WriteControl(websocket.PongMessage, []byte(data), time.Now().Add(10*time.Second))
	})
	hbEvery := time.Duration(welcome.HeartbeatSec) * time.Second
	if hbEvery <= 0 {
		hbEvery = 5 * time.Second
	}
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go a.heartbeat(sctx, hbEvery)
	for {
		ws.SetReadDeadline(time.Now().Add(90 * time.Second))
		var m v1.AgentMsg
		if err := ws.ReadJSON(&m); err != nil {
			return err
		}
		if m.Type == v1.MsgShutdown {
			return errShutdown
		}
		go a.handle(ctx, m)
	}
}

func (a *Agent) heartbeat(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		stats := gpu.Stats(ctx)
		assign := a.Runs.GPUAssignments()
		for i := range stats {
			stats[i].RunIDs = assign[stats[i].Index]
		}
		attribute(ctx, stats, a.Runs.RunSessions())
		if a.Emit(v1.MsgHeartbeat, v1.Heartbeat{TS: time.Now(), GPUs: stats, Disks: a.disks()}) != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// attribute marks the GPU processes that belong to MLDojo runs. What is
// left over is the interesting part: memory held on a card by something the
// platform did not start, and cannot account for.
func attribute(ctx context.Context, stats []v1.GPUStat, sessions map[int]string) {
	if len(sessions) == 0 {
		return
	}
	var pids []int
	for i := range stats {
		for _, p := range stats[i].Procs {
			pids = append(pids, p.PID)
		}
	}
	sids := gpu.Sessions(ctx, pids)
	for i := range stats {
		for j := range stats[i].Procs {
			p := &stats[i].Procs[j]
			if id, ok := sessions[sids[p.PID]]; ok {
				p.RunID = id
			}
		}
	}
}

// background ships logs (300ms) and scans metrics (3s).
func (a *Agent) background(ctx context.Context) {
	logs := time.NewTicker(300 * time.Millisecond)
	scan := time.NewTicker(3 * time.Second)
	defer logs.Stop()
	defer scan.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-logs.C:
			a.Runs.ShipLogs()
		case <-scan.C:
			a.Runs.Tick()
		}
	}
}

func decode[T any](m v1.AgentMsg) (T, error) {
	var v T
	err := json.Unmarshal(m.Payload, &v)
	return v, err
}

func (a *Agent) handle(ctx context.Context, m v1.AgentMsg) {
	switch m.Type {
	case v1.MsgPing:
		a.reply(m.ID, map[string]any{"pong": true, "version": version.Version}, nil)
	case v1.MsgSpawn:
		req, err := decode[v1.SpawnRequest](m)
		if err == nil {
			err = a.Runs.Spawn(req)
		}
		a.reply(m.ID, nil, err)
	case v1.MsgCancel:
		req, err := decode[v1.CancelRequest](m)
		if err == nil {
			err = a.Runs.Cancel(req.RunID)
		}
		a.reply(m.ID, nil, err)
	case v1.MsgExec:
		req, err := decode[v1.ExecRequest](m)
		if err != nil {
			a.reply(m.ID, nil, err)
			return
		}
		a.reply(m.ID, exec.Exec(ctx, req), nil)
	case v1.MsgListFiles:
		req, err := decode[v1.ListFilesRequest](m)
		var items []v1.ArtifactItem
		if err == nil {
			items, err = a.Runs.ListFilesReq(req)
		}
		a.reply(m.ID, items, err)
	case v1.MsgDiskUsage:
		a.reply(m.ID, a.Runs.DiskUsage(), nil)
	case v1.MsgPurgeRun:
		req, err := decode[v1.PurgeRunRequest](m)
		if err == nil {
			err = a.Runs.PurgeRun(req.RunID)
		}
		a.reply(m.ID, map[string]bool{"ok": err == nil}, err)
	case v1.MsgSendFile:
		req, err := decode[v1.SendFileRequest](m)
		if err == nil {
			err = a.Client.SendFile(ctx, expandHome(req.Path), req.URL, req.Tar)
		}
		a.reply(m.ID, nil, err)
	case v1.MsgFetchTar:
		req, err := decode[v1.FetchTarRequest](m)
		if err == nil {
			dest := expandHome(req.Dest)
			tmp := dest + ".partial"
			os.RemoveAll(tmp)
			if err = a.Client.FetchTar(ctx, req.URL, tmp); err == nil {
				os.RemoveAll(dest)
				if err = os.Rename(tmp, dest); err == nil && req.Marker != "" {
					err = os.WriteFile(dest+"/"+req.Marker, []byte(time.Now().Format(time.RFC3339)+"\n"), 0o644)
				}
			} else {
				os.RemoveAll(tmp)
			}
		}
		a.reply(m.ID, nil, err)
	default:
		if m.ID != "" {
			a.reply(m.ID, nil, fmt.Errorf("unknown command %q", m.Type))
		}
	}
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		h, _ := os.UserHomeDir()
		return h + p[1:]
	}
	return p
}
