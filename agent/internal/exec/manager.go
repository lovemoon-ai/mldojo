// Package exec runs training processes on the node: code checkout, env
// activation (none/venv/conda/docker), GPU pinning, dataset mounts, log
// files, exit codes and cancellation. Runs live in their own session so they
// survive agent restarts; the agent re-adopts them from state.json.
package exec

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/lovemoon-ai/mldojo/agent/internal/gpu"
	"github.com/lovemoon-ai/mldojo/agent/internal/metrics"
	msync "github.com/lovemoon-ai/mldojo/agent/internal/sync"
	v1 "github.com/lovemoon-ai/mldojo/proto/mldojo/v1"
	pysdk "github.com/lovemoon-ai/mldojo/sdk/python"
)

// Emitter sends a message to the server; it fails when disconnected.
type Emitter func(typ string, payload any) error

type Manager struct {
	Root     string // workdir root
	AgentDir string // ~/.mldojo/agent
	Client   *msync.Client
	Emit     Emitter

	mu   sync.Mutex
	runs map[string]*Run

	// gpuMu makes "pick GPUs, then record them" atomic. Without it two runs
	// launching at once both read the same busy set and land on one card.
	gpuMu sync.Mutex

	hashMu sync.Mutex
	hashes map[hashKey]string
}

type State struct {
	RunID       string          `json:"run_id"`
	Phase       string          `json:"phase"`
	PID         int             `json:"pid"`
	GPUs        []int           `json:"gpus"`
	StartedAt   *time.Time      `json:"started_at"`
	FinishedAt  *time.Time      `json:"finished_at"`
	ExitCode    *int            `json:"exit_code"`
	Message     string          `json:"message"`
	Commit      string          `json:"commit"`
	Container   string          `json:"container,omitempty"`
	Cancelled   bool            `json:"cancelled"`
	TimedOut    bool            `json:"timed_out,omitempty"`
	EnvLock     string          `json:"env_lock,omitempty"`
	ImageDigest string          `json:"image_digest,omitempty"`
	Spec        v1.SpawnRequest `json:"spec"`
}

type Run struct {
	mu       sync.Mutex
	st       State
	dir      string
	scanner  *metrics.Scanner
	logOff   map[string]int64
	lastArts time.Time
	done     chan struct{}
}

func (r *Run) meta(name string) string { return filepath.Join(r.dir, "_mldojo", name) }
func (r *Run) codeDir() string         { return filepath.Join(r.dir, "code") }

// workDir is where the command runs. An absolute workdir means the job runs
// in place, in a directory that already exists on the node: some projects
// cannot be shipped at all, because the code sits next to tens of gigabytes
// of base weights, prebuilt virtualenvs and simulator assets.
func (r *Run) workDir() string {
	if w := r.st.Spec.Workdir; filepath.IsAbs(w) {
		return filepath.Clean(filepath.FromSlash(w))
	}
	return filepath.Join(r.codeDir(), filepath.FromSlash(r.st.Spec.Workdir))
}

// inPlace reports whether the run uses a directory it did not create.
func (r *Run) inPlace() bool { return filepath.IsAbs(r.st.Spec.Workdir) }

func NewManager(root, agentDir string, c *msync.Client, emit Emitter) *Manager {
	return &Manager{Root: root, AgentDir: agentDir, Client: c, Emit: emit, runs: map[string]*Run{}}
}

var idRe = regexp.MustCompile(`^[a-f0-9-]{36}$`)

func (r *Run) save() error {
	r.mu.Lock()
	b, _ := json.MarshalIndent(r.st, "", "  ")
	r.mu.Unlock()
	tmp := r.meta("state.json.tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, r.meta("state.json"))
}

func (r *Run) snapshot() State {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.st
}

// Load adopts runs found under Root (after an agent restart).
func (m *Manager) Load() {
	entries, _ := os.ReadDir(m.Root)
	cutoff := time.Now().Add(-7 * 24 * time.Hour)
	for _, e := range entries {
		if !e.IsDir() || !idRe.MatchString(e.Name()) {
			continue
		}
		dir := filepath.Join(m.Root, e.Name())
		b, err := os.ReadFile(filepath.Join(dir, "_mldojo", "state.json"))
		if err != nil {
			continue
		}
		var st State
		if json.Unmarshal(b, &st) != nil || st.RunID != e.Name() {
			continue
		}
		if info, err := os.Stat(filepath.Join(dir, "_mldojo", "state.json")); err == nil && info.ModTime().Before(cutoff) && v1.Terminal(st.Phase) {
			continue
		}
		r := &Run{st: st, dir: dir, scanner: metrics.NewScanner(), logOff: map[string]int64{}, done: make(chan struct{})}
		m.mu.Lock()
		m.runs[st.RunID] = r
		m.mu.Unlock()
		if v1.Terminal(st.Phase) {
			close(r.done)
			continue
		}
		if st.Phase == v1.PhaseRunning && st.PID > 0 {
			slog.Info("adopting run", "run", st.RunID, "pid", st.PID)
			go m.watch(r, nil)
			go m.enforceWallTime(r, st.RunID, st.Spec.WallTimeMin)
		} else {
			m.finish(r, v1.PhaseFailed, nil, "agent restarted while the run was being prepared")
		}
	}
}

// DiskPaths lists the directories whose free space is worth reporting: the
// workdir root, plus, for every live run, its workdir and the directory each
// output glob writes into. A run can keep its checkpoints on a filesystem
// nobody would otherwise look at.
func (m *Manager) DiskPaths() []string {
	m.mu.Lock()
	runs := make([]*Run, 0, len(m.runs))
	for _, r := range m.runs {
		runs = append(runs, r)
	}
	m.mu.Unlock()
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		if p == "" || seen[p] {
			return
		}
		seen[p] = true
		out = append(out, p)
	}
	add(m.Root)
	// Finished runs count too. The filesystem a job writes its checkpoints
	// to is usually not the one the agent lives on, and you want to know
	// whether the next one will fit *before* submitting it -- which means
	// after the last run ended, not during.
	for _, r := range runs {
		st := r.snapshot()
		add(r.workDir())
		if o := st.Spec.Outputs; o != nil {
			for _, kg := range outputKinds(o) {
				if filepath.IsAbs(kg.glob) {
					root, _ := globRoot(kg.glob)
					add(root)
				}
			}
		}
		// Every heartbeat stats these, so keep the list bounded. Distinct
		// filesystems are few; distinct paths on them are not.
		if len(out) >= 12 {
			break
		}
	}
	return out
}

// States reports every known run (for hello).
func (m *Manager) States() []v1.AgentRunState {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []v1.AgentRunState{}
	for id, r := range m.runs {
		out = append(out, v1.AgentRunState{RunID: id, Phase: r.snapshot().Phase})
	}
	return out
}

// OnWelcome resets log offsets to what the server stored and re-sends state.
func (m *Manager) OnWelcome(w v1.Welcome) {
	m.mu.Lock()
	runs := make([]*Run, 0, len(m.runs))
	for _, r := range m.runs {
		runs = append(runs, r)
	}
	m.mu.Unlock()
	for _, r := range runs {
		st := r.snapshot()
		r.mu.Lock()
		r.logOff = map[string]int64{}
		if offs, ok := w.LogOffsets[st.RunID]; ok {
			for k, v := range offs {
				r.logOff[k] = v
			}
		} else if v1.Terminal(st.Phase) {
			// The server no longer tracks this run; don't resend.
			for _, s := range []string{v1.StreamStdout, v1.StreamStderr} {
				if fi, err := os.Stat(r.meta(s + ".log")); err == nil {
					r.logOff[s] = fi.Size()
				}
			}
		}
		r.scanner = metrics.NewScanner() // re-ship metrics (server upserts)
		r.mu.Unlock()
		m.emitStatus(r)
	}
	for _, id := range w.CancelRuns {
		go m.Cancel(id)
	}
}

func (m *Manager) emitStatus(r *Run) {
	st := r.snapshot()
	m.Emit(v1.MsgRunStatus, v1.RunStatusMsg{RunID: st.RunID, Phase: st.Phase, ExitCode: st.ExitCode, Message: st.Message,
		PID: st.PID, Workdir: r.workDir(), Commit: st.Commit, GPUs: st.GPUs, StartedAt: st.StartedAt, FinishedAt: st.FinishedAt,
		EnvLock: st.EnvLock, ImageDigest: st.ImageDigest})
}

func (m *Manager) setPhase(r *Run, phase, msg string) {
	r.mu.Lock()
	r.st.Phase, r.st.Message = phase, msg
	r.mu.Unlock()
	r.save()
	m.emitStatus(r)
}

// GPUAssignments maps GPU index -> run ids (for heartbeats).
func (m *Manager) GPUAssignments() map[int][]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[int][]string{}
	for id, r := range m.runs {
		st := r.snapshot()
		if v1.Terminal(st.Phase) {
			continue
		}
		for _, g := range st.GPUs {
			out[g] = append(out[g], id)
		}
	}
	return out
}

// RunSessions maps each live run's launcher pid to its run id. Runs start
// with setsid, so every process they spawn -- including the python process
// nvidia-smi sees on a card -- shares that pid as its session id.
func (m *Manager) RunSessions() map[int]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[int]string{}
	for id, r := range m.runs {
		st := r.snapshot()
		if v1.Terminal(st.Phase) || st.PID == 0 {
			continue
		}
		out[st.PID] = id
	}
	return out
}

// Spawn validates the request, registers the run and prepares/launches it
// in the background. The reply only means "accepted".
func (m *Manager) Spawn(req v1.SpawnRequest) error {
	if !idRe.MatchString(req.RunID) {
		return fmt.Errorf("invalid run id")
	}
	m.mu.Lock()
	if old, ok := m.runs[req.RunID]; ok && !v1.Terminal(old.snapshot().Phase) {
		m.mu.Unlock()
		return nil // duplicate dispatch
	}
	dir := filepath.Join(m.Root, req.RunID)
	r := &Run{dir: dir, scanner: metrics.NewScanner(), logOff: map[string]int64{}, done: make(chan struct{}),
		st: State{RunID: req.RunID, Phase: v1.PhaseStarting, Spec: req}}
	m.runs[req.RunID] = r
	m.mu.Unlock()
	os.RemoveAll(dir)
	if err := os.MkdirAll(filepath.Join(dir, "_mldojo"), 0o755); err != nil {
		return err
	}
	for _, s := range []string{"stdout.log", "stderr.log"} {
		os.WriteFile(r.meta(s), nil, 0o644)
	}
	r.save()
	go func() {
		if err := m.launch(r); err != nil {
			slog.Warn("launch failed", "run", req.RunID, "err", err)
			m.appendLog(r, "stderr.log", fmt.Sprintf("[mldojo] launch failed: %v\n", err))
			m.finish(r, v1.PhaseFailed, nil, err.Error())
		}
	}()
	return nil
}

func (m *Manager) appendLog(r *Run, name, s string) {
	f, err := os.OpenFile(r.meta(name), os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err == nil {
		f.WriteString(s)
		f.Close()
	}
}

func (m *Manager) launch(r *Run) error {
	ctx := context.Background()
	req := r.st.Spec
	m.setPhase(r, v1.PhaseStarting, "fetching code")
	code := r.codeDir()
	switch req.Code.Source {
	case "bundle":
		bundle := r.meta("bundle.tar.gz")
		if err := m.Client.Download(ctx, req.Code.BundleURL, bundle); err != nil {
			return fmt.Errorf("download code: %w", err)
		}
		f, err := os.Open(bundle)
		if err != nil {
			return err
		}
		err = msync.ExtractTar(f, code, true)
		f.Close()
		if err != nil {
			return fmt.Errorf("extract code: %w", err)
		}
		os.Remove(bundle)
		r.mu.Lock()
		r.st.Commit = req.Code.Commit
		r.mu.Unlock()
	case "git":
		commit, err := msync.GitCheckout(ctx, req.Code.Repo, req.Code.Ref, req.Code.Commit, code, envList(req.Vars, "http_proxy", "https_proxy", "no_proxy", "HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY"))
		if err != nil {
			return err
		}
		if req.Code.PatchURL != "" {
			p := r.meta("dirty.patch")
			if err := m.Client.Download(ctx, req.Code.PatchURL, p); err != nil {
				return fmt.Errorf("download patch: %w", err)
			}
			if err := msync.GitApply(ctx, code, p); err != nil {
				return err
			}
		}
		r.mu.Lock()
		r.st.Commit = commit
		r.mu.Unlock()
	default:
		os.MkdirAll(code, 0o755)
	}
	if r.inPlace() {
		// Do not create it: the point of an in-place run is that the
		// directory, its environment and its data are already there. A typo
		// should say so, not silently run in an empty new directory.
		if st, err := os.Stat(r.workDir()); err != nil || !st.IsDir() {
			return fmt.Errorf("workdir %s does not exist on this node", r.workDir())
		}
	} else if err := os.MkdirAll(r.workDir(), 0o755); err != nil {
		return err
	}
	shim, err := m.writePyShim()
	if err != nil {
		slog.Warn("python shim", "err", err)
	}
	m.gpuMu.Lock()
	gpus, warn := m.pickGPUs(req.GPUs, req.RunID)
	r.mu.Lock()
	r.st.GPUs = gpus
	r.mu.Unlock()
	m.gpuMu.Unlock()
	if len(gpus) > 0 {
		m.appendLog(r, "stdout.log", fmt.Sprintf(
			"[mldojo] assigned GPU %s (CUDA_VISIBLE_DEVICES and MLDOJO_GPUS are set)\n", joinInts(gpus)))
	}
	if warn != "" {
		m.appendLog(r, "stdout.log", "[mldojo] "+warn+"\n")
	}
	script, container, err := m.script(r, gpus, shim)
	if err != nil {
		return err
	}
	if err := os.WriteFile(r.meta("run.sh"), []byte(script), 0o755); err != nil {
		return err
	}
	// Append mode keeps agent-written header lines; the wrapper records the
	// exit code so it survives agent restarts.
	wrapper := `bash "$1" >> "$2" 2>> "$3"; rc=$?; echo $rc > "$4.tmp"; mv "$4.tmp" "$4"`
	cmd := exec.Command("bash", "-c", wrapper, "mldojo-run", r.meta("run.sh"), r.meta("stdout.log"), r.meta("stderr.log"), r.meta("exit_code"))
	cmd.Dir = r.workDir()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Env = os.Environ()
	r.mu.Lock()
	if r.st.Cancelled {
		r.mu.Unlock()
		m.finish(r, v1.PhaseCancelled, nil, "cancelled before start")
		return nil
	}
	if err := cmd.Start(); err != nil {
		r.mu.Unlock()
		return err
	}
	now := time.Now()
	r.st.PID, r.st.StartedAt, r.st.Container, r.st.Phase = cmd.Process.Pid, &now, container, v1.PhaseRunning
	r.st.Message = ""
	if len(gpus) > 0 {
		r.st.Message = fmt.Sprintf("CUDA_VISIBLE_DEVICES=%s", joinInts(gpus))
	}
	r.mu.Unlock()
	r.save()
	m.emitStatus(r)
	slog.Info("run started", "run", req.RunID, "pid", cmd.Process.Pid, "gpus", gpus)
	go m.watch(r, cmd)
	go m.enforceWallTime(r, req.RunID, req.WallTimeMin)
	go m.captureEnv(r, req, container)
	return nil
}

// maxEnvLock bounds what a pathological environment can push into the run's
// metadata; a normal pip freeze is a few KiB.
const maxEnvLock = 256 << 10

// captureEnv reports what the environment resolved to, once it exists: the
// dependency list the run script writes, and the image digest behind a
// mutable docker tag.
func (m *Manager) captureEnv(r *Run, req v1.SpawnRequest, container string) {
	digest := ""
	if req.Env.Type == "docker" && req.Env.Image != "" {
		digest = imageDigest(req.Env.Image)
	}
	path := r.meta("env.lock")
	read := func() bool {
		b, err := os.ReadFile(path)
		if err != nil || len(b) == 0 {
			return false
		}
		lock := string(b)
		if len(lock) > maxEnvLock {
			lock = lock[:maxEnvLock] + "\n... truncated by mldojo-agent\n"
		}
		m.recordEnv(r, lock, digest)
		return true
	}
	deadline := time.Now().Add(5 * time.Minute)
	for {
		if read() {
			return
		}
		if time.Now().After(deadline) {
			// Docker runs never write it (the file lives inside the
			// container), so the digest alone is still worth reporting.
			if digest != "" {
				m.recordEnv(r, "", digest)
			}
			return
		}
		select {
		case <-r.done:
			// A short run can finish before the backgrounded pip freeze has
			// flushed, so give it a moment rather than giving up here.
			for range 5 {
				time.Sleep(time.Second)
				if read() {
					return
				}
			}
			if digest != "" {
				m.recordEnv(r, "", digest)
			}
			return
		case <-time.After(2 * time.Second):
		}
	}
}

func (m *Manager) recordEnv(r *Run, lock, digest string) {
	r.mu.Lock()
	if lock != "" {
		r.st.EnvLock = lock
	}
	if digest != "" {
		r.st.ImageDigest = digest
	}
	r.mu.Unlock()
	r.save()
	m.emitStatus(r)
}

// imageDigest resolves a tag to the immutable digest it currently points at.
func imageDigest(image string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "image", "inspect",
		"--format", "{{if .RepoDigests}}{{index .RepoDigests 0}}{{else}}{{.Id}}{{end}}", image).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// enforceWallTime stops a run that outlives resources.wall_time_min. Node
// runs had no limit at all, so a job that hung kept its GPUs forever. The
// deadline is measured from the run's start, so it survives an agent restart.
func (m *Manager) enforceWallTime(r *Run, id string, min int) {
	if min <= 0 {
		return
	}
	st := r.snapshot()
	start := time.Now()
	if st.StartedAt != nil {
		start = *st.StartedAt
	}
	t := time.NewTimer(max(time.Until(start.Add(time.Duration(min)*time.Minute)), 0))
	defer t.Stop()
	select {
	case <-r.done:
	case <-t.C:
		r.mu.Lock()
		if v1.Terminal(r.st.Phase) {
			r.mu.Unlock()
			return
		}
		r.st.TimedOut = true
		r.mu.Unlock()
		m.appendLog(r, "stdout.log", fmt.Sprintf("[mldojo] wall time of %d min reached; stopping the run\n", min))
		if err := m.Cancel(id); err != nil {
			slog.Warn("wall time stop", "run", id, "err", err)
		}
	}
}

func joinInts(v []int) string {
	s := make([]string, len(v))
	for i, x := range v {
		s[i] = strconv.Itoa(x)
	}
	return strings.Join(s, ",")
}

func envList(vars map[string]string, keys ...string) []string {
	var out []string
	for _, k := range keys {
		if v, ok := vars[k]; ok {
			out = append(out, k+"="+v)
		}
	}
	return out
}

// pickGPUs chooses n GPUs, preferring ones no mldojo run uses and with the
// least memory in use. There is no scheduler: if fewer are
// free, it still starts and says so.
func (m *Manager) pickGPUs(n int, self string) ([]int, string) {
	if n <= 0 {
		return nil, ""
	}
	stats := gpu.Stats(context.Background())
	if len(stats) == 0 {
		return nil, fmt.Sprintf("warning: %d GPU(s) requested but no NVIDIA GPU was found on this node", n)
	}
	busy := m.GPUAssignments()
	sort.SliceStable(stats, func(i, j int) bool {
		bi, bj := len(busy[stats[i].Index]), len(busy[stats[j].Index])
		if bi != bj {
			return bi < bj
		}
		return stats[i].MemUsedMB < stats[j].MemUsedMB
	})
	warn := ""
	if n > len(stats) {
		warn = fmt.Sprintf("warning: %d GPUs requested, node has %d", n, len(stats))
		n = len(stats)
	}
	var out []int
	shared := 0
	var held []string
	for _, s := range stats[:n] {
		out = append(out, s.Index)
		if len(busy[s.Index]) > 0 {
			shared++
		}
		// Memory held by something MLDojo did not start is the usual cause
		// of an OOM three minutes into a job. Say so before it happens,
		// rather than leaving it to be discovered in the traceback.
		if procs, mem := s.Foreign(); procs > 0 {
			held = append(held, fmt.Sprintf("GPU %d has %.0f MiB held by %d process(es) not started by mldojo, %.0f MiB free",
				s.Index, mem, procs, s.FreeMB()))
		}
	}
	if shared > 0 && warn == "" {
		warn = fmt.Sprintf("warning: %d of the assigned GPUs are also used by other mldojo runs", shared)
	}
	if len(held) > 0 {
		warn = strings.TrimSpace(warn + "\nwarning: " + strings.Join(held, "\nwarning: "))
	}
	sort.Ints(out)
	return out, warn
}

func (m *Manager) writePyShim() (string, error) {
	dir := filepath.Join(m.AgentDir, "pyshim")
	err := fs.WalkDir(pysdk.FS, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := pysdk.FS.ReadFile(p)
		if err != nil {
			return err
		}
		dst := filepath.Join(dir, filepath.FromSlash(p))
		if old, err := os.ReadFile(dst); err == nil && string(old) == string(b) {
			return nil
		}
		os.MkdirAll(filepath.Dir(dst), 0o755)
		return os.WriteFile(dst, b, 0o644)
	})
	return dir, err
}

func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

var envKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func fileHash(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])[:16]
}

// script renders run.sh for the env type.
func (m *Manager) script(r *Run, gpus []int, shim string) (string, string, error) {
	req := r.st.Spec
	vars := map[string]string{}
	for k, v := range req.Vars {
		vars[k] = v
	}
	vars["PYTHONUNBUFFERED"] = "1"
	vars["MLDOJO_METRICS_FILE"] = r.meta("sdk_metrics.jsonl")
	vars["MLDOJO_EPISODES_FILE"] = r.meta("sdk_episodes.jsonl")
	vars["MLDOJO_WORKDIR"] = r.workDir()
	if len(gpus) > 0 {
		vars["CUDA_VISIBLE_DEVICES"] = joinInts(gpus)
		vars["NVIDIA_VISIBLE_DEVICES"] = joinInts(gpus)
		// Plenty of training scripts set CUDA_VISIBLE_DEVICES themselves and
		// overwrite the assignment. MLDOJO_GPUS carries it under a name
		// nobody else writes, so such a script can be pointed at the right
		// cards without being rewritten.
		vars["MLDOJO_GPUS"] = joinInts(gpus)
	}
	pypath := shim
	if req.WandbShim {
		pypath = shim + "/wandb_shim:" + shim
	}
	var mountLines []string
	for _, d := range req.Datasets {
		vars["MLDOJO_DATASET_"+envName(d.Name)] = d.Path
		if d.Mount != "" && req.Env.Type != "docker" {
			mountLines = append(mountLines, fmt.Sprintf(`mldojo_mount %s %s`, shq(d.Path), shq(d.Mount)))
		}
	}
	var sb strings.Builder
	sb.WriteString("#!/usr/bin/env bash\n# generated by mldojo-agent\n")
	keys := make([]string, 0, len(vars))
	for k := range vars {
		if envKeyRe.MatchString(k) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&sb, "export %s=%s\n", k, shq(vars[k]))
	}
	sb.WriteString(`mldojo_mount() {
  if [ -L "$2" ] && [ "$(readlink "$2")" = "$1" ]; then return 0; fi
  if [ -e "$2" ]; then echo "[mldojo] mount $2 already exists; dataset is also at $1 (\$MLDOJO_DATASET_*)"; return 0; fi
  if mkdir -p "$(dirname "$2")" 2>/dev/null && ln -s "$1" "$2" 2>/dev/null; then echo "[mldojo] mounted $1 at $2";
  else echo "[mldojo] cannot create $2 (permission?); use \$MLDOJO_DATASET_* = $1"; fi
}
# Record what the environment actually resolved to. Storing only the name of
# an environment.yaml is not reproducible: the same file resolves to
# different versions a month later. The interpreter version is part of that
# and is always recorded -- a fresh venv has no packages at all, so a bare
# pip freeze would write an empty file and look like a failure.
# Runs in the background and never fails the run.
mldojo_env_lock() {
  {
    echo "# python: $(python3 -V 2>&1 || echo unknown)"
    echo "# packages:"
    pip freeze 2>/dev/null || python3 -m pip freeze 2>/dev/null || conda list --export 2>/dev/null ||
      echo "# (no package manager on PATH)"
  } >"$1".tmp 2>/dev/null && mv "$1".tmp "$1" 2>/dev/null || true
}
`)
	for _, l := range mountLines {
		sb.WriteString(l + "\n")
	}
	container := ""
	switch req.Env.Type {
	case "", "none":
		fmt.Fprintf(&sb, "export PYTHONPATH=%s${PYTHONPATH:+:$PYTHONPATH}\n", shq(pypath))
		fmt.Fprintf(&sb, "cd %s || exit 96\n", shq(r.workDir()))
		fmt.Fprintf(&sb, "mldojo_env_lock %s &\n", shq(r.meta("env.lock")))
		sb.WriteString(req.Cmd + "\n")
	case "venv":
		fmt.Fprintf(&sb, "cd %s || exit 96\n", shq(r.workDir()))
		if req.Env.Spec != "" {
			spec := filepath.Join(r.codeDir(), req.Env.Spec)
			h := fileHash(spec)
			if h == "" {
				return "", "", fmt.Errorf("venv spec %s not found in the code", req.Env.Spec)
			}
			prefix := filepath.Join(filepath.Dir(m.AgentDir), "envs", "venv-"+h)
			fmt.Fprintf(&sb, `P=%s; mkdir -p "$(dirname "$P")"
( flock 9
  if [ ! -x "$P/bin/python" ]; then
    echo "[mldojo] creating venv $P"
    # Ubuntu 24.04 has no python3-venv and installing it needs root, so fall
    # back to uv, which builds the venv (and seeds pip) on its own.
    { python3 -m venv "$P" 2>/dev/null || uv venv --seed "$P" 2>/dev/null; } &&
      "$P/bin/pip" install -q -r %s || { rm -rf "$P"; exit 97; }
  fi ) 9>"$P.lock" || { echo "[mldojo] venv setup failed" >&2; exit 97; }
source "$P/bin/activate"
`, shq(prefix), shq(spec))
		} else {
			sb.WriteString(`if [ ! -x .venv/bin/python ]; then
  python3 -m venv .venv 2>/dev/null || uv venv --seed .venv 2>/dev/null || exit 97
fi
source .venv/bin/activate
`)
		}
		fmt.Fprintf(&sb, "export PYTHONPATH=%s${PYTHONPATH:+:$PYTHONPATH}\n", shq(pypath))
		fmt.Fprintf(&sb, "mldojo_env_lock %s &\n", shq(r.meta("env.lock")))
		sb.WriteString(req.Cmd + "\n")
	case "conda":
		sb.WriteString(`CONDA_BIN="$(command -v conda || true)"
for c in "$HOME/miniconda3/bin/conda" "$HOME/miniforge3/bin/conda" "$HOME/anaconda3/bin/conda" /opt/conda/bin/conda; do
  if [ -z "$CONDA_BIN" ] && [ -x "$c" ]; then CONDA_BIN="$c"; fi
done
[ -n "$CONDA_BIN" ] || { echo "[mldojo] conda not found on this node" >&2; exit 97; }
eval "$("$CONDA_BIN" shell.bash hook)"
`)
		spec := req.Env.Spec
		specPath := filepath.Join(r.codeDir(), spec)
		if st, err := os.Stat(specPath); err == nil && !st.IsDir() {
			prefix := filepath.Join(filepath.Dir(m.AgentDir), "envs", "conda-"+fileHash(specPath))
			fmt.Fprintf(&sb, `P=%s; mkdir -p "$(dirname "$P")"
( flock 9
  if [ ! -d "$P/conda-meta" ]; then
    echo "[mldojo] creating conda env $P"; "$CONDA_BIN" env create -q -p "$P" -f %s || { rm -rf "$P"; exit 97; }
  fi ) 9>"$P.lock" || { echo "[mldojo] conda env setup failed" >&2; exit 97; }
conda activate "$P" || exit 97
`, shq(prefix), shq(specPath))
		} else if spec != "" {
			fmt.Fprintf(&sb, "conda activate %s || { echo '[mldojo] conda env %s not found' >&2; exit 97; }\n", shq(spec), spec)
		}
		fmt.Fprintf(&sb, "export PYTHONPATH=%s${PYTHONPATH:+:$PYTHONPATH}\n", shq(pypath))
		fmt.Fprintf(&sb, "cd %s || exit 96\n", shq(r.workDir()))
		fmt.Fprintf(&sb, "mldojo_env_lock %s &\n", shq(r.meta("env.lock")))
		sb.WriteString(req.Cmd + "\n")
	case "docker":
		container = "mldojo-" + req.RunID[:8]
		envFile := r.meta("docker.env")
		var ef strings.Builder
		for _, k := range keys {
			v := vars[k]
			if strings.ContainsAny(v, "\n") {
				continue
			}
			switch k {
			case "MLDOJO_METRICS_FILE":
				v = "/mldojo/sdk_metrics.jsonl"
			case "MLDOJO_WORKDIR":
				v = "/workspace/" + strings.TrimPrefix(filepath.ToSlash(req.Workdir), "./")
			}
			fmt.Fprintf(&ef, "%s=%s\n", k, v)
		}
		ef.WriteString("PYTHONPATH=/opt/mldojo/pyshim" + map[bool]string{true: "/wandb_shim:/opt/mldojo/pyshim", false: ""}[req.WandbShim] + "\n")
		if err := os.WriteFile(envFile, []byte(ef.String()), 0o600); err != nil {
			return "", "", err
		}
		args := []string{"docker", "run", "--rm", "--init", "--name", container, "--network", "host", "--ipc", "host",
			"--env-file", envFile, "-v", r.codeDir() + ":/workspace", "-v", filepath.Join(r.dir, "_mldojo") + ":/mldojo",
			"-v", shim + ":/opt/mldojo/pyshim:ro", "-w", "/workspace/" + strings.TrimPrefix(filepath.ToSlash(req.Workdir), "./")}
		if len(gpus) > 0 {
			args = append(args, "--gpus", `"device=`+joinInts(gpus)+`"`)
		}
		for _, d := range req.Datasets {
			if d.Mount != "" {
				args = append(args, "-v", d.Path+":"+d.Mount+":ro")
			}
		}
		args = append(args, req.Env.Image, "bash", "-lc", req.Cmd)
		q := make([]string, len(args))
		for i, a := range args {
			q[i] = shq(a)
		}
		fmt.Fprintf(&sb, "docker rm -f %s >/dev/null 2>&1 || true\nexec %s\n", container, strings.Join(q, " "))
	default:
		return "", "", fmt.Errorf("unsupported env type %q", req.Env.Type)
	}
	return sb.String(), container, nil
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

func alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// watch waits for the run to exit (own child or adopted pid).
func (m *Manager) watch(r *Run, cmd *exec.Cmd) {
	if cmd != nil {
		cmd.Wait()
	} else {
		for {
			if _, err := os.Stat(r.meta("exit_code")); err == nil {
				break
			}
			if !alive(r.snapshot().PID) {
				time.Sleep(time.Second) // the wrapper writes exit_code right before exiting
				break
			}
			time.Sleep(time.Second)
		}
	}
	var code *int
	if b, err := os.ReadFile(r.meta("exit_code")); err == nil {
		if n, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
			code = &n
		}
	}
	st := r.snapshot()
	switch {
	case st.TimedOut:
		m.finish(r, v1.PhaseFailed, code, fmt.Sprintf("wall time of %d min exceeded", st.Spec.WallTimeMin))
	case st.Cancelled:
		m.finish(r, v1.PhaseCancelled, code, "cancelled")
	case code == nil:
		m.finish(r, v1.PhaseFailed, nil, "process disappeared without an exit code")
	case *code == 0:
		m.finish(r, v1.PhaseSucceeded, code, "")
	case *code == 96:
		m.finish(r, v1.PhaseFailed, code, "workdir not found")
	case *code == 97:
		m.finish(r, v1.PhaseFailed, code, "environment setup failed (see stderr)")
	default:
		m.finish(r, v1.PhaseFailed, code, fmt.Sprintf("exit code %d", *code))
	}
}

func (m *Manager) finish(r *Run, phase string, code *int, msg string) {
	// Final flush before reporting the terminal phase. Episodes especially:
	// most harnesses write their manifest as the last thing they do.
	m.scanMetrics(r)
	m.scanArtifacts(r)
	m.scanEpisodes(r)
	now := time.Now()
	r.mu.Lock()
	if v1.Terminal(r.st.Phase) {
		r.mu.Unlock()
		return
	}
	r.st.Phase, r.st.ExitCode, r.st.Message, r.st.FinishedAt = phase, code, msg, &now
	r.mu.Unlock()
	r.save()
	// Let the log shipper send the tail before the terminal status.
	m.ShipLogs()
	m.emitStatus(r)
	select {
	case <-r.done:
	default:
		close(r.done)
	}
	exit := "-"
	if code != nil {
		exit = strconv.Itoa(*code)
	}
	slog.Info("run finished", "run", r.st.RunID, "phase", phase, "exit", exit)
}

// Cancel stops a run: SIGTERM to the process group, SIGKILL after 15s.
func (m *Manager) Cancel(id string) error {
	m.mu.Lock()
	r := m.runs[id]
	m.mu.Unlock()
	if r == nil {
		return fmt.Errorf("unknown run %s", id)
	}
	r.mu.Lock()
	if v1.Terminal(r.st.Phase) {
		r.mu.Unlock()
		return nil
	}
	r.st.Cancelled = true
	pid, container := r.st.PID, r.st.Container
	r.mu.Unlock()
	r.save()
	if container != "" {
		exec.Command("docker", "kill", container).Run()
	}
	if pid > 0 {
		syscall.Kill(-pid, syscall.SIGTERM)
		go func() {
			select {
			case <-r.done:
			case <-time.After(15 * time.Second):
				syscall.Kill(-pid, syscall.SIGKILL)
			}
		}()
	} else {
		m.finish(r, v1.PhaseCancelled, nil, "cancelled before start")
	}
	return nil
}

// ShipLogs sends new log bytes of every run (called periodically).
func (m *Manager) ShipLogs() {
	m.mu.Lock()
	runs := make([]*Run, 0, len(m.runs))
	for _, r := range m.runs {
		runs = append(runs, r)
	}
	m.mu.Unlock()
	for _, r := range runs {
		for _, s := range []string{v1.StreamStdout, v1.StreamStderr} {
			for i := 0; i < 64; i++ { // at most 16 MiB per tick per stream
				r.mu.Lock()
				off := r.logOff[s]
				r.mu.Unlock()
				data, err := readAt(r.meta(s+".log"), off, 256<<10)
				if err != nil || len(data) == 0 {
					break
				}
				if err := m.Emit(v1.MsgLog, v1.LogMsg{RunID: r.st.RunID, Stream: s, Offset: off, Data: data}); err != nil {
					return
				}
				r.mu.Lock()
				r.logOff[s] = off + int64(len(data))
				r.mu.Unlock()
			}
		}
	}
}

func readAt(p string, off int64, n int) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.Size() <= off {
		return nil, err
	}
	if rem := st.Size() - off; rem < int64(n) {
		n = int(rem)
	}
	buf := make([]byte, n)
	k, err := f.ReadAt(buf, off)
	if err != nil && !errors.Is(err, io.EOF) {
		// A short read at EOF is normal (the writer is still appending);
		// a real I/O error must not silently truncate the log instead.
		return buf[:k], err
	}
	return buf[:k], nil
}

// Tick scans metrics (and occasionally artifacts) of active runs.
func (m *Manager) Tick() {
	m.mu.Lock()
	runs := make([]*Run, 0, len(m.runs))
	for _, r := range m.runs {
		runs = append(runs, r)
	}
	m.mu.Unlock()
	for _, r := range runs {
		if v1.Terminal(r.snapshot().Phase) {
			continue
		}
		m.scanMetrics(r)
		if time.Since(r.lastArts) > 30*time.Second {
			m.scanArtifacts(r)
			m.scanEpisodes(r)
		}
	}
}

func (m *Manager) scanMetrics(r *Run) {
	st := r.snapshot()
	srcs := []v1.MetricsSource{{Type: "jsonl", Path: r.meta("sdk_metrics.jsonl")}}
	if st.Spec.Outputs != nil {
		srcs = append(srcs, st.Spec.Outputs.Metrics...)
	}
	r.mu.Lock()
	sc := r.scanner
	r.mu.Unlock()
	pts := sc.Scan(r.workDir(), srcs)
	for i := 0; i < len(pts); i += 2000 {
		end := min(i+2000, len(pts))
		m.Emit(v1.MsgMetrics, v1.MetricsMsg{RunID: st.RunID, Points: pts[i:end]})
	}
}

// scanEpisodes reports evaluation results. It re-sends the whole list each
// time, which the server upserts: a harness usually writes its manifest once,
// at the end, and often rewrites earlier rows when it does.
func (m *Manager) scanEpisodes(r *Run) {
	st := r.snapshot()
	srcs := []v1.EpisodesSource{{Type: "jsonl", Path: r.meta("sdk_episodes.jsonl")}}
	if st.Spec.Outputs != nil {
		srcs = append(srcs, st.Spec.Outputs.Episodes...)
	}
	r.mu.Lock()
	sc := r.scanner
	r.mu.Unlock()
	eps := sc.Episodes(r.workDir(), srcs)
	if len(eps) > 0 {
		m.Emit(v1.MsgEpisodes, v1.EpisodesMsg{RunID: st.RunID, Episodes: eps})
	}
}

func (m *Manager) scanArtifacts(r *Run) {
	r.lastArts = time.Now()
	st := r.snapshot()
	if st.Spec.Outputs == nil {
		return
	}
	items := m.ListFiles(r, outputKinds(st.Spec.Outputs))
	if len(items) > 0 {
		m.Emit(v1.MsgArtifacts, v1.ArtifactsMsg{RunID: st.RunID, Items: items})
	}
}

type kindGlob struct{ kind, glob string }

func outputKinds(o *v1.Outputs) []kindGlob {
	var out []kindGlob
	for _, kg := range []kindGlob{{"log", o.Logs}, {"ckpt", o.Checkpoints}, {"video", o.Videos}, {"image", o.Images}} {
		if kg.glob != "" {
			out = append(out, kg)
		}
	}
	return out
}

// DiskUsage reports what each run directory under the agent's root is
// holding. A cluster fills up one abandoned run at a time, and from outside
// the node there was no way to see which.
//
// In-place runs contribute almost nothing here, correctly: their outputs
// live in a project directory MLDojo does not own and must not offer to
// delete.
func (m *Manager) DiskUsage() v1.DiskUsage {
	out := v1.DiskUsage{Root: m.Root}
	entries, _ := os.ReadDir(m.Root)
	budget := 400000
	for _, e := range entries {
		if !e.IsDir() || !idRe.MatchString(e.Name()) {
			continue
		}
		dir := filepath.Join(m.Root, e.Name())
		d := v1.DiskEntry{RunID: e.Name(), Path: dir}
		filepath.WalkDir(dir, func(p string, x fs.DirEntry, err error) error {
			if err != nil || x.IsDir() {
				return nil
			}
			if budget--; budget <= 0 {
				out.Truncated = true
				return filepath.SkipAll
			}
			if info, err := x.Info(); err == nil {
				d.Bytes += info.Size()
				d.Files++
				if info.ModTime().After(d.Modified) {
					d.Modified = info.ModTime()
				}
			}
			return nil
		})
		out.Entries = append(out.Entries, d)
		if out.Truncated {
			break
		}
	}
	sort.Slice(out.Entries, func(i, j int) bool { return out.Entries[i].Bytes > out.Entries[j].Bytes })
	return out
}

// PurgeRun deletes a run's directory on the node. The run must be finished:
// deleting the workdir of something still writing to it turns a running job
// into a confusing failure.
func (m *Manager) PurgeRun(runID string) error {
	if !idRe.MatchString(runID) {
		return fmt.Errorf("invalid run id")
	}
	m.mu.Lock()
	r := m.runs[runID]
	m.mu.Unlock()
	if r != nil && !v1.Terminal(r.snapshot().Phase) {
		return fmt.Errorf("run %s is still %s", runID, r.snapshot().Phase)
	}
	if err := os.RemoveAll(filepath.Join(m.Root, runID)); err != nil {
		return err
	}
	m.mu.Lock()
	delete(m.runs, runID)
	m.mu.Unlock()
	return nil
}

// ListFilesReq handles list_files requests (globs relative to the workdir).
func (m *Manager) ListFilesReq(req v1.ListFilesRequest) ([]v1.ArtifactItem, error) {
	m.mu.Lock()
	r := m.runs[req.RunID]
	m.mu.Unlock()
	if r == nil {
		return nil, fmt.Errorf("unknown run %s", req.RunID)
	}
	var kgs []kindGlob
	if o := r.snapshot().Spec.Outputs; o != nil {
		kgs = outputKinds(o)
	}
	for _, g := range req.Globs {
		found := false
		for _, kg := range kgs {
			if kg.glob == g {
				found = true
			}
		}
		if !found {
			kgs = append(kgs, kindGlob{guessKind(g), g})
		}
	}
	return m.ListFiles(r, kgs), nil
}

func guessKind(p string) string {
	switch strings.ToLower(filepath.Ext(strings.TrimSuffix(p, "*"))) {
	case ".mp4", ".webm", ".mov", ".gif":
		return "video"
	case ".png", ".jpg", ".jpeg", ".webp":
		return "image"
	case ".ckpt", ".pt", ".pth", ".safetensors", ".bin":
		return "ckpt"
	case ".log", ".txt":
		return "log"
	}
	return "other"
}

var skipDirs = map[string]bool{".git": true, "node_modules": true, "__pycache__": true, ".venv": true, "wandb": false}

// globRoot splits an absolute glob into the deepest directory that contains
// no wildcard, and the pattern relative to it. Walking from there instead of
// from "/" is the difference between listing a run's outputs and walking a
// 200 TB shared filesystem.
func globRoot(g string) (root, rel string) {
	g = filepath.ToSlash(g)
	parts := strings.Split(g, "/")
	i := 0
	for ; i < len(parts)-1; i++ {
		if strings.ContainsAny(parts[i], "*?{") {
			break
		}
	}
	root = strings.Join(parts[:i], "/")
	if root == "" {
		root = "/"
	}
	return root, strings.Join(parts[i:], "/")
}

// ListFiles matches every glob, walking each root once. Relative globs are
// anchored at the workdir; absolute ones are anchored at their own deepest
// wildcard-free prefix, so a recipe can collect outputs a job wrote outside
// the run directory.
func (m *Manager) ListFiles(r *Run, kgs []kindGlob) []v1.ArtifactItem {
	type matcher struct {
		re   *regexp.Regexp
		kind string
	}
	roots := map[string][]matcher{}
	var order []string
	for _, kg := range kgs {
		root, rel := r.workDir(), kg.glob
		if filepath.IsAbs(kg.glob) {
			root, rel = globRoot(kg.glob)
		}
		if _, ok := roots[root]; !ok {
			order = append(order, root)
		}
		roots[root] = append(roots[root], matcher{globRegexp(rel), kg.kind})
	}
	var out []v1.ArtifactItem
	n := 0
	for _, root := range order {
		ms := roots[root]
		filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				if skipDirs[d.Name()] && p != root {
					return filepath.SkipDir
				}
				return nil
			}
			if n++; n > 200000 {
				return filepath.SkipAll
			}
			rel, _ := filepath.Rel(root, p)
			rel = filepath.ToSlash(rel)
			for _, mt := range ms {
				if mt.re.MatchString(rel) {
					info, err := d.Info()
					if err != nil {
						break
					}
					out = append(out, v1.ArtifactItem{Kind: mt.kind, Path: p, SizeBytes: info.Size(),
						SHA256: m.fileSHA(p, info.Size(), info.ModTime())})
					break
				}
			}
			return nil
		})
	}
	return out
}

// globRegexp supports *, ?, ** and {a,b}.
func globRegexp(g string) *regexp.Regexp {
	g = strings.TrimPrefix(filepath.ToSlash(g), "./")
	var sb strings.Builder
	sb.WriteString("^")
	for i := 0; i < len(g); i++ {
		c := g[i]
		switch c {
		case '*':
			if i+1 < len(g) && g[i+1] == '*' {
				if i+2 < len(g) && g[i+2] == '/' {
					sb.WriteString("(?:.*/)?")
					i += 2
				} else {
					sb.WriteString(".*")
					i++
				}
			} else {
				sb.WriteString("[^/]*")
			}
		case '?':
			sb.WriteString("[^/]")
		case '{':
			sb.WriteString("(?:")
		case '}':
			sb.WriteString(")")
		case ',':
			sb.WriteString("|")
		default:
			sb.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	sb.WriteString("$")
	re, err := regexp.Compile(sb.String())
	if err != nil {
		return regexp.MustCompile(`^$`)
	}
	return re
}

// Exec runs a short shell command for the server.
func Exec(ctx context.Context, req v1.ExecRequest) v1.ExecResult {
	t := time.Duration(req.TimeoutSec) * time.Second
	if t <= 0 || t > 10*time.Minute {
		t = time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, t)
	defer cancel()
	out, err := exec.CommandContext(ctx, "bash", "-lc", req.Cmd).CombinedOutput()
	if len(out) > 64<<10 {
		out = out[len(out)-64<<10:]
	}
	res := v1.ExecResult{Output: string(out)}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		res.ExitCode = ee.ExitCode()
	} else if err != nil {
		res.ExitCode, res.Output = 127, err.Error()
	}
	return res
}

// Content hashing for artifacts. The sha256 column existed from the start
// but nothing ever filled it, so there was no integrity check, no dedupe and
// no way to tell whether two runs produced the same checkpoint.

// maxHashBytes bounds what is hashed inline: a 200 GB checkpoint would stall
// the scan, and its size and path already identify it well enough.
const maxHashBytes = 4 << 30

type hashKey struct {
	path  string
	size  int64
	mtime int64
}

// fileSHA hashes a file, reusing the previous result while size and mtime are
// unchanged -- artifacts are rescanned every 30 seconds and a checkpoint that
// has not moved must not be re-read each time.
func (m *Manager) fileSHA(path string, size int64, mod time.Time) string {
	if size > maxHashBytes {
		return ""
	}
	key := hashKey{path, size, mod.UnixNano()}
	m.hashMu.Lock()
	if sum, ok := m.hashes[key]; ok {
		m.hashMu.Unlock()
		return sum
	}
	m.hashMu.Unlock()

	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	sum := hex.EncodeToString(h.Sum(nil))

	m.hashMu.Lock()
	if len(m.hashes) > 4096 { // bounded: a long run can produce many files
		m.hashes = map[hashKey]string{}
	}
	if m.hashes == nil {
		m.hashes = map[hashKey]string{}
	}
	m.hashes[key] = sum
	m.hashMu.Unlock()
	return sum
}
