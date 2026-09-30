package backends

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/lovemoon-ai/mldojo/adapters/node_local"
	"github.com/lovemoon-ai/mldojo/adapters/node_ssh"
	"github.com/lovemoon-ai/mldojo/api/internal/models"
	"github.com/lovemoon-ai/mldojo/datasets"
	v1 "github.com/lovemoon-ai/mldojo/proto/mldojo/v1"
	"github.com/lovemoon-ai/mldojo/recipes"
)

// NodeBackend runs work on nodes through their agents.
type NodeBackend struct {
	rt     *Runtime
	Hub    *Hub
	Relay  *Relay
	Dialer *node_ssh.Dialer

	mu          sync.Mutex
	dispatching map[string]bool
	tunnels     *tunnelKeeper
}

func NewNodeBackend(rt *Runtime) *NodeBackend {
	b := &NodeBackend{rt: rt, Hub: NewHub(rt), Relay: NewRelay(), dispatching: map[string]bool{}}
	b.Dialer = &node_ssh.Dialer{Resolver: b, KnownHostsFile: rt.Cfg.KnownHostsFile}
	b.tunnels = newTunnelKeeper(b)
	b.Hub.OnOnline = b.onOnline
	return b
}

func (b *NodeBackend) Kind() string { return "node" }

// NodeConnection implements node_ssh.Resolver.
func (b *NodeBackend) NodeConnection(ctx context.Context, id string) (v1.NodeConnection, error) {
	n, err := b.rt.Store.GetNode(ctx, id)
	if err != nil {
		if id == "local" {
			return v1.NodeConnection{Type: "local"}, nil
		}
		return v1.NodeConnection{}, err
	}
	return n.Connection, nil
}

// Secret implements node_ssh.Resolver.
func (b *NodeBackend) Secret(ctx context.Context, ref string) ([]byte, error) {
	return b.rt.Secrets.Get(ctx, ref)
}

// executor opens a command channel to a node (local shell or SSH).
func (b *NodeBackend) executor(ctx context.Context, id string, conn v1.NodeConnection) (node_local.Executor, func(), error) {
	if conn.Type == "local" {
		return node_local.Local{}, func() {}, nil
	}
	c, err := b.Dialer.Dial(ctx, id, conn)
	if err != nil {
		return nil, nil, Unreachablef("%v", err)
	}
	return node_ssh.Exec{C: c}, func() { c.Close() }, nil
}

// NodeAddRequest is the body of POST /nodes.
type NodeAddRequest struct {
	DryRun            bool              `json:"dry_run"`
	ID                string            `json:"id"`
	DisplayName       string            `json:"display_name"`
	Labels            []string          `json:"labels"`
	Connection        v1.NodeConnection `json:"connection"`
	Proxy             *v1.Proxy         `json:"proxy"`
	WorkdirRoot       string            `json:"workdir_root"`
	DatasetsCacheRoot string            `json:"datasets_cache_root"`
}

// AddNode deploys an agent and stores the node once the agent dials back.
// Any failure returns an error and stores nothing.
func (b *NodeBackend) AddNode(ctx context.Context, req NodeAddRequest) (*v1.Node, error) {
	if !recipes.ValidName(req.ID) {
		return nil, Userf("invalid node id %q", req.ID)
	}
	if req.Connection.Type == "" {
		req.Connection.Type = "ssh"
	}
	if req.Connection.Type != "ssh" && req.Connection.Type != "local" {
		return nil, Userf("connection.type must be ssh or local")
	}
	if req.Connection.Type == "ssh" && req.Connection.Host == "" {
		return nil, Userf("connection.host is required for ssh nodes")
	}
	if _, err := b.rt.Store.GetNode(ctx, req.ID); err == nil {
		return nil, models.Conflict("node %q already exists", req.ID)
	}
	for _, v := range req.Connection.Via {
		if v.Node == req.ID {
			return nil, Userf("node cannot be its own via")
		}
	}
	ex, closeEx, err := b.executor(ctx, req.ID, req.Connection)
	if err != nil {
		return nil, err
	}
	defer closeEx()

	serverURL, err := b.agentServerURL(ctx, req.ID, req.Connection)
	if err != nil {
		return nil, err
	}
	tok, hash := NewToken()
	helloCh, cancel := b.Hub.ExpectRegistration(req.ID, hash)
	defer cancel()
	res, err := node_local.Deploy(ctx, ex, node_local.DeployOptions{
		NodeID: req.ID, ServerURL: serverURL, Token: tok, WorkdirRoot: req.WorkdirRoot,
		DatasetsRoot: req.DatasetsCacheRoot, Proxy: req.Proxy, AgentDist: b.rt.Cfg.AgentDist,
		PreferSystemd: req.Connection.Type == "local",
	})
	if err != nil {
		b.tunnels.stop(req.ID)
		return nil, fmt.Errorf("deploy agent on %s: %w", ex.Describe(), err)
	}
	slog.Info("agent deployed; waiting for it to dial back", "node", req.ID, "method", res.Method, "server_url", serverURL,
		"data_home", res.DataHome.Dir, "data_home_reason", res.DataHome.Reason, "start_output", res.StartOutput)
	var hello v1.Hello
	select {
	case hello = <-helloCh:
	case <-time.After(60 * time.Second):
		tail := node_local.LogTail(ctx, ex, req.ID)
		node_local.Stop(context.Background(), ex, req.ID)
		b.tunnels.stop(req.ID)
		return nil, Unreachablef("agent on %s did not connect back to %s within 60s (is the server reachable from the node? set connection.agent_server_url or reverse_tunnel). agent log:\n%s", req.ID, serverURL, tail)
	case <-ctx.Done():
		node_local.Stop(context.Background(), ex, req.ID)
		b.tunnels.stop(req.ID)
		return nil, ctx.Err()
	}
	now := time.Now()
	n := &v1.Node{
		ID: req.ID, DisplayName: req.DisplayName, Labels: req.Labels, Connection: req.Connection, Proxy: req.Proxy,
		Capacity: &hello.Capacity, AgentStatus: "online", AgentVersion: hello.Version, LastHeartbeat: &now,
		WorkdirRoot: hello.WorkdirRoot, DatasetsCacheRoot: hello.DatasetsRoot,
	}
	if n.DisplayName == "" {
		n.DisplayName = req.ID
	}
	if err := b.rt.Store.InsertNode(ctx, n, hash); err != nil {
		node_local.Stop(context.Background(), ex, req.ID)
		b.Hub.Disconnect(req.ID)
		b.tunnels.stop(req.ID)
		return nil, err
	}
	slog.Info("node added", "node", req.ID, "route", ex.Describe(), "method", res.Method, "arch", res.OS+"/"+res.Arch)
	return b.rt.Store.GetNode(ctx, req.ID)
}

// agentServerURL is the URL the node's agent dials back to.
func (b *NodeBackend) agentServerURL(ctx context.Context, id string, conn v1.NodeConnection) (string, error) {
	u := b.rt.Cfg.PublicURL
	if conn.Type == "local" {
		u = b.rt.Cfg.LocalURL
	}
	if conn.AgentServerURL != "" {
		u = conn.AgentServerURL
	}
	if conn.ReverseTunnel {
		if conn.Type != "ssh" {
			return "", Userf("reverse_tunnel requires an ssh node")
		}
		if m := conn.ReverseTunnelMode; m != "" && m != "forward" && m != "stdio" {
			return "", Userf("reverse_tunnel_mode must be forward or stdio, not %q", m)
		}
		tu, err := b.tunnels.start(ctx, id, conn)
		if err != nil {
			return "", Unreachablef("reverse tunnel: %v", err)
		}
		u = tu
	}
	if u == "" {
		return "", Userf("server public_url is not configured; set api.public_url (MLDOJO_PUBLIC_URL) to a URL agents can reach")
	}
	return u, nil
}

// ConnPatch moves a node to a new SSH address -- say a container that came
// back on another port. Zero fields are kept.
type ConnPatch struct {
	Host string `json:"host,omitempty"`
	Port int    `json:"port,omitempty"`
	User string `json:"user,omitempty"`
}

// UpgradeNode redeploys the agent (current binary, fresh token) on an
// existing node. The workdir root is kept so running jobs are re-adopted.
// A non-empty patch deploys to the new address and is stored only once the
// agent has connected from there.
func (b *NodeBackend) UpgradeNode(ctx context.Context, id string, patch ConnPatch) (_ *v1.Node, err error) {
	n, err := b.rt.Store.GetNode(ctx, id)
	if err != nil {
		return nil, err
	}
	moved := patch != ConnPatch{}
	if moved {
		if n.Connection.Type != "ssh" {
			return nil, Userf("only ssh nodes have an address to change")
		}
		old := n.Connection
		n.Connection.Host = cmp.Or(patch.Host, old.Host)
		n.Connection.Port = cmp.Or(patch.Port, old.Port)
		n.Connection.User = cmp.Or(patch.User, old.User)
		b.tunnels.stop(id) // it still dials the old address
		defer func() {
			if err == nil {
				err = b.rt.Store.SetNodeConnection(ctx, id, n.Connection)
				return
			}
			b.tunnels.stop(id)
			if old.ReverseTunnel {
				b.tunnels.start(context.Background(), id, old)
			}
		}()
	}
	ex, closeEx, err := b.executor(ctx, id, n.Connection)
	if err != nil {
		return nil, err
	}
	defer closeEx()
	serverURL, err := b.agentServerURL(ctx, id, n.Connection)
	if err != nil {
		return nil, err
	}
	tok, hash := NewToken()
	helloCh, cancel := b.Hub.ExpectRegistration(id, hash)
	defer cancel()
	if _, err := node_local.Deploy(ctx, ex, node_local.DeployOptions{
		NodeID: id, ServerURL: serverURL, Token: tok, WorkdirRoot: n.WorkdirRoot, DatasetsRoot: n.DatasetsCacheRoot,
		Proxy: n.Proxy, AgentDist: b.rt.Cfg.AgentDist, PreferSystemd: n.Connection.Type == "local",
	}); err != nil {
		return nil, fmt.Errorf("redeploy agent on %s: %w", ex.Describe(), err)
	}
	select {
	case <-helloCh:
	case <-time.After(60 * time.Second):
		return nil, Unreachablef("upgraded agent on %s did not connect back within 60s. agent log:\n%s", id, node_local.LogTail(ctx, ex, id))
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if err := b.rt.Store.SetNodeTokenHash(ctx, id, hash); err != nil {
		return nil, err
	}
	slog.Info("node upgraded", "node", id, "route", ex.Describe())
	return b.rt.Store.GetNode(ctx, id)
}

// ProbeNode connects like AddNode but only runs read-only commands
// (`mldojo node add --dry-run`): nothing is deployed or stored.
func (b *NodeBackend) ProbeNode(ctx context.Context, req NodeAddRequest) (map[string]any, error) {
	if req.Connection.Type == "" {
		req.Connection.Type = "ssh"
	}
	start := time.Now()
	ex, closeEx, err := b.executor(ctx, req.ID, req.Connection)
	if err != nil {
		return nil, err
	}
	defer closeEx()
	p, err := node_local.Probe(ctx, ex)
	if err != nil {
		return nil, Unreachablef("%s: %v", ex.Describe(), err)
	}
	res := map[string]any{"ok": true, "route": ex.Describe(), "latency_ms": time.Since(start).Milliseconds(), "probe": p}
	if req.Connection.ReverseTunnel && stdioRelay(req.Connection) {
		res["reverse_tunnel"] = "stdio relay over exec (needs no port forwarding)"
	} else if se, ok := ex.(node_ssh.Exec); ok && req.Connection.ReverseTunnel {
		// Check that the SSH server allows remote forwarding (-R), which
		// reverse_tunnel needs; bastions often forbid it.
		if l, err := se.C.Listen("tcp", "127.0.0.1:0"); err != nil {
			res["reverse_tunnel"] = "not allowed: " + err.Error() + " (try --reverse-tunnel-mode stdio)"
		} else {
			res["reverse_tunnel"] = "ok"
			l.Close()
		}
	}
	return res, nil
}

// TestResult is returned by POST /nodes/{id}/test.
type TestResult struct {
	OK          bool   `json:"ok"`
	Reachable   bool   `json:"reachable"`
	AgentOnline bool   `json:"agent_online"`
	LatencyMS   int64  `json:"latency_ms"`
	Via         string `json:"via"`
	Message     string `json:"message"`
}

func (b *NodeBackend) TestNode(ctx context.Context, id string) (*TestResult, error) {
	n, err := b.rt.Store.GetNode(ctx, id)
	if err != nil {
		return nil, err
	}
	res := &TestResult{}
	start := time.Now()
	ex, closeEx, err := b.executor(ctx, id, n.Connection)
	if err != nil {
		res.Message = err.Error()
	} else {
		res.Via = ex.Describe()
		if _, err := ex.Run(ctx, "true", nil); err != nil {
			res.Message = err.Error()
		} else {
			res.Reachable = true
		}
		closeEx()
	}
	res.LatencyMS = time.Since(start).Milliseconds()
	if c := b.Hub.Conn(id); c != nil {
		pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		_, err := c.Request(pctx, v1.MsgPing, map[string]any{})
		cancel()
		res.AgentOnline = err == nil
		if err != nil && res.Message == "" {
			res.Message = "agent ping failed: " + err.Error()
		}
	} else if res.Message == "" {
		res.Message = "agent is not connected"
	}
	res.OK = res.Reachable && res.AgentOnline
	if res.OK {
		res.Message = "ok"
	}
	return res, nil
}

func (b *NodeBackend) RemoveNode(ctx context.Context, id string, stopAgent, force bool) error {
	n, err := b.rt.Store.GetNode(ctx, id)
	if err != nil {
		return err
	}
	if n.ActiveRuns > 0 && !force {
		return models.Conflict("node %s has %d active runs; cancel them or pass force", id, n.ActiveRuns)
	}
	if stopAgent {
		if ex, closeEx, err := b.executor(ctx, id, n.Connection); err == nil {
			if err := node_local.Stop(ctx, ex, id); err != nil {
				slog.Warn("stop agent", "node", id, "err", err)
			}
			closeEx()
		} else {
			slog.Warn("stop agent: node unreachable", "node", id, "err", err)
		}
	}
	b.Hub.Disconnect(id)
	b.tunnels.stop(id)
	return b.rt.Store.DeleteNode(ctx, id)
}

// Validate checks the node exists and the requested resources fit.
func (b *NodeBackend) Validate(ctx context.Context, spec *SubmitSpec) error {
	if spec.Target.Kind == "pool" {
		// Fail now if no node could ever satisfy this, rather than leaving
		// the run queued forever waiting for a machine that does not exist.
		cands, err := b.PoolCandidates(ctx, spec.Target.Labels, spec.Resources)
		if err != nil {
			return err
		}
		if len(cands) == 0 {
			return Userf("no node carries labels %s with gpus=%d gpu_type=%q min_mem_gb=%d (see `mldojo node ls`)",
				strings.Join(spec.Target.Labels, ","), spec.Resources.GPUs, spec.Resources.GPUType, spec.Resources.MinMemGB)
		}
		return nil
	}
	n, err := b.rt.Store.GetNode(ctx, spec.Target.ID)
	if models.IsNotFound(err) {
		return Userf("unknown node %q (see `mldojo node ls`)", spec.Target.ID)
	}
	if err != nil {
		return err
	}
	if spec.Env.Type == "docker" && spec.Env.Image == "" {
		return Userf("docker env requires an image")
	}
	res := spec.Resources
	if n.Capacity == nil || res.GPUs == 0 {
		return nil
	}
	var ok []v1.GPUInfo
	for _, g := range n.Capacity.GPUs {
		if !recipes.GPUTypeMatches(res.GPUType, g.Model) {
			continue
		}
		if res.MinMemGB > 0 && g.MemGB+1 < res.MinMemGB {
			continue
		}
		ok = append(ok, g)
	}
	if len(ok) < res.GPUs {
		models := []string{}
		for _, g := range n.Capacity.GPUs {
			models = append(models, fmt.Sprintf("%s %dGB", g.Model, g.MemGB))
		}
		return Userf("node %s cannot satisfy gpus=%d gpu_type=%q min_mem_gb=%d (node has: %s)",
			n.ID, res.GPUs, res.GPUType, res.MinMemGB, strings.Join(models, ", "))
	}
	return nil
}

// Submit dispatches asynchronously.
func (b *NodeBackend) Submit(ctx context.Context, spec *SubmitSpec) error {
	if spec.Target.Kind == "pool" {
		// Try to place it now so a free cluster dispatches immediately;
		// otherwise the reconcile loop picks it up when something frees up.
		go b.Reconcile(context.WithoutCancel(ctx))
		return nil
	}
	b.Hub.runNode.Store(spec.Run.ID, spec.Target.ID)
	go b.dispatch(spec.Run.ID)
	return nil
}

func (b *NodeBackend) onOnline(nodeID string, hello v1.Hello) {
	ctx := context.Background()
	// Reconcile: runs the server thinks are active but the agent does not know.
	known := map[string]bool{}
	for _, r := range hello.Runs {
		known[r.RunID] = true
	}
	runs, err := b.rt.Store.ListRuns(ctx, models.RunFilter{BackendKind: "node", BackendID: nodeID, Active: true, Limit: 1000})
	if err != nil {
		return
	}
	for _, r := range runs {
		switch {
		case r.Status == v1.PhaseQueued:
			go b.dispatch(r.ID)
		case !known[r.ID] && r.Status == v1.PhaseRunning:
			b.rt.FailInfra(ctx, r.ID, errors.New("the agent restarted and no longer knows this run (workdir removed?)"))
		case !known[r.ID] && r.Status == v1.PhaseStarting:
			b.rt.SetStatus(ctx, r.ID, models.RunUpdate{Phase: v1.PhaseQueued, Message: "re-dispatching after agent reconnect"})
			go b.dispatch(r.ID)
		}
	}
	b.stopStrayRuns(ctx, nodeID, hello)
}

// stopStrayRuns kills processes the agent is still running for runs the
// server already gave up on -- the reaper fails a run when its agent stays
// unreachable, and without this the process would keep holding GPUs with
// nobody watching it.
func (b *NodeBackend) stopStrayRuns(ctx context.Context, nodeID string, hello v1.Hello) {
	c := b.Hub.Conn(nodeID)
	if c == nil {
		return
	}
	for _, ar := range hello.Runs {
		if v1.Terminal(ar.Phase) {
			continue
		}
		r, err := b.rt.Store.GetRun(ctx, ar.RunID)
		if err != nil || !v1.Terminal(r.Status) {
			continue
		}
		slog.Warn("stopping stray run", "run", r.ID, "node", nodeID, "server_status", r.Status, "agent_phase", ar.Phase)
		if _, err := c.Request(ctx, v1.MsgCancel, v1.CancelRequest{RunID: r.ID}); err != nil {
			slog.Warn("stop stray run", "run", r.ID, "node", nodeID, "err", err)
		}
	}
}

func (b *NodeBackend) dispatch(runID string) {
	b.mu.Lock()
	if b.dispatching[runID] {
		b.mu.Unlock()
		return
	}
	b.dispatching[runID] = true
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		delete(b.dispatching, runID)
		b.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Hour)
	defer cancel()
	run, err := b.rt.Store.GetRun(ctx, runID)
	if err != nil || run.Status != v1.PhaseQueued {
		return
	}
	c := b.Hub.Conn(run.BackendID)
	if c == nil {
		b.rt.SysLog(runID, "node %s agent is offline; the run stays queued until it reconnects", run.BackendID)
		b.rt.Store.MergeRunMetadata(ctx, runID, map[string]any{"message": "waiting for agent on " + run.BackendID})
		return
	}
	// Concurrency limits are checked here rather than at submit time, so a
	// run waits its turn instead of being rejected. Deciding and recording
	// the decision must be atomic: four runs submitted together each counted
	// the node before any of them had marked itself starting, and a node
	// limited to one run started three.
	var why string
	if err := b.rt.Store.WithNodeLock(ctx, run.BackendID, func(ctx context.Context) error {
		if why = b.admit(ctx, run, run.BackendID); why != "" {
			return nil
		}
		_, err := b.rt.SetStatus(ctx, runID, models.RunUpdate{Phase: v1.PhaseStarting, Message: "dispatching to " + run.BackendID})
		return err
	}); err != nil {
		b.rt.FailInfra(ctx, runID, fmt.Errorf("admit: %w", err))
		return
	}
	if why != "" {
		b.rt.SysLog(runID, "waiting: %s", why)
		b.rt.Store.MergeRunMetadata(ctx, runID, map[string]any{"message": "waiting: " + why})
		return
	}
	req, err := b.spawnRequest(ctx, run)
	if err != nil {
		b.rt.Fail(ctx, runID, err)
		return
	}
	if _, err := c.Request(ctx, v1.MsgSpawn, req); err != nil {
		b.rt.FailInfra(ctx, runID, fmt.Errorf("spawn: %w", err))
		return
	}
	b.rt.Store.AddEvent(ctx, runID, "spawned", map[string]any{"node": run.BackendID})
}

func (b *NodeBackend) spawnRequest(ctx context.Context, run *v1.Run) (*v1.SpawnRequest, error) {
	var env v1.EnvSpec
	var res v1.Resources
	json.Unmarshal(run.Env, &env)
	json.Unmarshal(run.Resources, &res)
	md := run.Metadata
	vars := map[string]string{}
	for k, v := range md.ExtraEnv {
		vars[k] = v
	}
	for k, v := range env.Vars {
		vars[k] = v
	}
	n, err := b.rt.Store.GetNode(ctx, run.BackendID)
	if err != nil {
		return nil, err
	}
	for k, v := range n.Proxy.Env() {
		vars[k] = v
	}
	vars["MLDOJO_RUN_ID"] = run.ID
	vars["MLDOJO_RUN_TOKEN"] = b.rt.RunToken(run.ID)
	vars["MLDOJO_PROJECT"] = run.Project
	vars["MLDOJO_EXPERIMENT"] = run.Experiment
	vars["MLDOJO_TARGET"] = run.Target
	vars["MLDOJO_API_URL"] = b.rt.Cfg.PublicURL
	for k, v := range md.Params {
		vars["MLDOJO_PARAM_"+envName(k)] = fmt.Sprint(v)
	}
	req := &v1.SpawnRequest{
		RunID: run.ID, Project: run.Project, Experiment: run.Experiment, Cmd: md.Cmd, Workdir: md.Workdir,
		Env: env, Vars: vars, GPUs: res.GPUs, Outputs: md.Outputs, WandbShim: md.Wandb == "shim",
		WallTimeMin: res.WallTimeMin,
	}
	if md.Setup != "" {
		req.Cmd = md.Setup + "\n" + md.Cmd
	}
	switch {
	case md.CodeBundleURI != "":
		req.Code = v1.SpawnCode{Source: "bundle", BundleURL: "/api/v1/agent/blobs/" + strings.TrimPrefix(md.CodeBundleURI, "blob://"), Commit: run.CodeCommit}
	case md.CodeRepo != "":
		req.Code = v1.SpawnCode{Source: "git", Repo: md.CodeRepo, Ref: md.CodeRef, Commit: run.CodeCommit}
		if run.CodePatchURI != "" {
			req.Code.PatchURL = "/api/v1/agent/blobs/" + strings.TrimPrefix(run.CodePatchURI, "blob://")
		}
	default:
		req.Code = v1.SpawnCode{Source: "none"}
	}
	for _, d := range md.Datasets {
		p, err := b.EnsureDataset(ctx, run.ID, n, d.Name, d.Version)
		if err != nil {
			return nil, fmt.Errorf("dataset %s@%s: %w", d.Name, d.Version, err)
		}
		req.Datasets = append(req.Datasets, v1.SpawnDataset{Name: d.Name, Version: d.Version, Mount: d.Mount, Path: p})
	}
	return req, nil
}

func envName(k string) string {
	var sb strings.Builder
	for _, r := range strings.ToUpper(k) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			sb.WriteRune(r)
		} else {
			sb.WriteByte('_')
		}
	}
	return sb.String()
}

func (b *NodeBackend) exec(ctx context.Context, c *AgentConn, cmd string) (*v1.ExecResult, error) {
	data, err := c.Request(ctx, v1.MsgExec, v1.ExecRequest{Cmd: cmd, TimeoutSec: 60})
	if err != nil {
		return nil, err
	}
	var r v1.ExecResult
	return &r, json.Unmarshal(data, &r)
}

// EnsureDataset makes name@version available on node n and returns its path
// there.
func (b *NodeBackend) EnsureDataset(ctx context.Context, runID string, n *v1.Node, name, version string) (string, error) {
	logf := func(format string, a ...any) {
		if runID != "" {
			b.rt.SysLog(runID, format, a...)
		}
		slog.Info(fmt.Sprintf(format, a...), "node", n.ID)
	}
	ds, err := b.rt.Store.GetDataset(ctx, name, version)
	if err != nil {
		return "", err
	}
	c := b.Hub.Conn(n.ID)
	if c == nil {
		return "", Unreachablef("agent on %s is offline", n.ID)
	}
	q := func(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
	// 1. The node already holds a registered copy.
	for _, l := range datasets.OnNode(ds, n.ID) {
		r, err := b.exec(ctx, c, "test -e "+q(l.Path))
		if err == nil && r.ExitCode == 0 {
			logf("dataset %s@%s: using %s on %s", ds.Name, ds.Version, l.Path, n.ID)
			return l.Path, nil
		}
	}
	// 2. Node cache.
	cache := datasets.CachePath(n.DatasetsCacheRoot, ds.Name, ds.Version)
	marker := datasets.CompleteMarker
	if r, err := b.exec(ctx, c, "test -f "+q(path.Join(cache, marker))); err == nil && r.ExitCode == 0 {
		logf("dataset %s@%s: cache hit %s", ds.Name, ds.Version, cache)
		return cache, nil
	}
	// 3. Pull from an authoritative node_path on another online node.
	src := datasets.PickSource(ds, n.ID, func(node string) bool { return b.Hub.Conn(node) != nil })
	if src == nil {
		return "", Userf("no online node holds dataset %s@%s (bucket locations are not synced to nodes in v1); register a node_path or bring its node online", ds.Name, ds.Version)
	}
	srcConn := b.Hub.Conn(src.Node)
	logf("dataset %s@%s: syncing %s:%s -> %s:%s (first use)", ds.Name, ds.Version, src.Node, src.Path, n.ID, cache)
	start := time.Now()
	id, wait := b.Relay.NewPipe(src.Node, n.ID)
	url := "/api/v1/agent/relay/" + id
	errc := make(chan error, 2)
	go func() {
		_, err := srcConn.Request(ctx, v1.MsgSendFile, v1.SendFileRequest{Path: src.Path, URL: url, Tar: true})
		errc <- err
	}()
	go func() {
		_, err := c.Request(ctx, v1.MsgFetchTar, v1.FetchTarRequest{URL: url, Dest: cache, Marker: marker})
		errc <- err
	}()
	werr := wait(ctx)
	for i := 0; i < 2; i++ {
		if err := <-errc; err != nil && werr == nil {
			werr = err
		}
	}
	if werr != nil {
		return "", fmt.Errorf("sync from %s failed: %w", src.Node, werr)
	}
	logf("dataset %s@%s: synced in %s", ds.Name, ds.Version, time.Since(start).Round(time.Second))
	if strings.HasPrefix(cache, "~/") {
		if r, err := b.exec(ctx, c, `printf %s "$HOME"`); err == nil && r.ExitCode == 0 {
			cache = strings.TrimSpace(r.Output) + cache[1:]
		}
	}
	b.rt.Store.AddDatasetLocation(ctx, ds.ID, v1.DatasetLocation{Kind: "node_path", Node: n.ID, Path: cache})
	return cache, nil
}

func (b *NodeBackend) Cancel(ctx context.Context, run *v1.Run) error {
	if v1.Terminal(run.Status) {
		return nil
	}
	c := b.Hub.Conn(run.BackendID)
	if run.Status == v1.PhaseQueued || c == nil {
		_, err := b.rt.SetStatus(ctx, run.ID, models.RunUpdate{Phase: v1.PhaseCancelled, Message: "cancelled by user"})
		return err
	}
	if _, err := c.Request(ctx, v1.MsgCancel, v1.CancelRequest{RunID: run.ID}); err != nil {
		return err
	}
	_, err := b.rt.SetStatus(ctx, run.ID, models.RunUpdate{Phase: v1.PhaseCancelled, Message: "cancelled by user"})
	return err
}

func (b *NodeBackend) ArtifactsList(ctx context.Context, run *v1.Run) ([]v1.Artifact, error) {
	c := b.Hub.Conn(run.BackendID)
	if c != nil && run.Metadata.Outputs != nil {
		globs := outputGlobs(run.Metadata.Outputs)
		data, err := c.Request(ctx, v1.MsgListFiles, v1.ListFilesRequest{RunID: run.ID, Globs: globs})
		if err == nil {
			var items []v1.ArtifactItem
			json.Unmarshal(data, &items)
			b.rt.Store.UpsertArtifacts(ctx, run.ID, toArtifacts(run.BackendID, items))
		}
	}
	return b.rt.Store.ListArtifacts(ctx, run.ID)
}

func outputGlobs(o *v1.Outputs) []string {
	var g []string
	for _, s := range []string{o.Logs, o.Checkpoints, o.Videos, o.Images} {
		if s != "" {
			g = append(g, s)
		}
	}
	return g
}

func (b *NodeBackend) ArtifactsFetch(ctx context.Context, run *v1.Run, uri string) (string, error) {
	node, p, ok := ParseNodeURI(uri)
	if !ok || node != run.BackendID {
		return "", Userf("not a node artifact of this run: %s", uri)
	}
	arts, err := b.rt.Store.ListArtifacts(ctx, run.ID)
	if err != nil {
		return "", err
	}
	var art *v1.Artifact
	for i := range arts {
		if arts[i].URI == uri {
			art = &arts[i]
		}
	}
	if art == nil {
		return "", models.NotFound("artifact %s", uri)
	}
	dest := b.rt.Logs.CachePath(run.ID + "|" + uri)
	if st, err := os.Stat(dest); err == nil && st.Size() == art.SizeBytes {
		return dest, nil
	}
	c := b.Hub.Conn(node)
	if c == nil {
		return "", Unreachablef("agent on %s is offline", node)
	}
	id, wait := b.Relay.NewUpload(node, dest)
	errc := make(chan error, 1)
	go func() {
		_, err := c.Request(ctx, v1.MsgSendFile, v1.SendFileRequest{Path: p, URL: "/api/v1/agent/upload/" + id})
		errc <- err
	}()
	if err := wait(ctx); err != nil {
		return "", err
	}
	if err := <-errc; err != nil {
		return "", err
	}
	return dest, nil
}

func (b *NodeBackend) GPUStats(ctx context.Context, run *v1.Run) ([]v1.GPUStat, error) {
	var h struct {
		GPUs []int `json:"gpus"`
	}
	json.Unmarshal(run.BackendHandle, &h)
	all := b.Hub.GPU(run.BackendID)
	if len(h.GPUs) == 0 {
		return all, nil
	}
	var out []v1.GPUStat
	for _, g := range all {
		for _, i := range h.GPUs {
			if g.Index == i {
				out = append(out, g)
			}
		}
	}
	return out, nil
}

// DiskUsage asks a node what MLDojo is holding on it.
func (b *NodeBackend) DiskUsage(ctx context.Context, nodeID string) (*v1.DiskUsage, error) {
	c := b.Hub.Conn(nodeID)
	if c == nil {
		return nil, Unreachablef("agent on %s is offline", nodeID)
	}
	data, err := c.Request(ctx, v1.MsgDiskUsage, struct{}{})
	if err != nil {
		return nil, err
	}
	var du v1.DiskUsage
	if err := json.Unmarshal(data, &du); err != nil {
		return nil, err
	}
	return &du, nil
}

// PurgeRunDir deletes a finished run's directory on its node. Deleting the
// run row frees the server's copy of the logs; the node's copy of the
// checkpoints is usually the larger half and nothing used to reclaim it.
func (b *NodeBackend) PurgeRunDir(ctx context.Context, run *v1.Run) error {
	if run.BackendKind != "node" || run.BackendID == "" {
		return nil
	}
	c := b.Hub.Conn(run.BackendID)
	if c == nil {
		return Unreachablef("agent on %s is offline", run.BackendID)
	}
	_, err := c.Request(ctx, v1.MsgPurgeRun, v1.PurgeRunRequest{RunID: run.ID})
	return err
}

// PushDataset pre-warms a node cache (mldojo dataset push).
func (b *NodeBackend) PushDataset(ctx context.Context, name, version, nodeID string) (string, error) {
	n, err := b.rt.Store.GetNode(ctx, nodeID)
	if err != nil {
		return "", err
	}
	return b.EnsureDataset(ctx, "", n, name, version)
}

// StartTunnels re-establishes reverse tunnels for nodes that use them.
func (b *NodeBackend) StartTunnels(ctx context.Context) {
	nodes, err := b.rt.Store.ListNodes(ctx)
	if err != nil {
		return
	}
	for _, n := range nodes {
		if n.Connection.ReverseTunnel {
			go func(n v1.Node) {
				if _, err := b.tunnels.start(ctx, n.ID, n.Connection); err != nil {
					slog.Warn("reverse tunnel", "node", n.ID, "err", err)
				}
			}(n)
		}
	}
}
