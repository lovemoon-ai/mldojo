// Package backends implements the unified Backend contract with
// two shapes: NodeBackend (reverse-connected agents) and queue backends
// (SidecarBackend, one per configured queue plugin). Logs and metrics are
// push-based: every backend feeds the shared Runtime, and API clients read
// from it.
package backends

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/lovemoon-ai/mldojo/api/internal/logstore"
	"github.com/lovemoon-ai/mldojo/api/internal/models"
	"github.com/lovemoon-ai/mldojo/api/internal/obs"
	"github.com/lovemoon-ai/mldojo/api/internal/secrets"
	v1 "github.com/lovemoon-ai/mldojo/proto/mldojo/v1"
	"github.com/lovemoon-ai/mldojo/recipes"
)

// SubmitSpec is everything a backend needs to launch one run.
type SubmitSpec struct {
	Run       *v1.Run
	Target    recipes.Target
	Env       v1.EnvSpec
	Resources v1.Resources
}

// Backend is the contract every adapter implements:
//
//	submit  -> Submit (async dispatch; phase/handle updates go to Runtime)
//	status  -> pushed by agents / polled by queue pollers into Runtime
//	logs    -> pushed into Runtime.Logs (NodeBackend: real tail; queue plugins: poll+diff)
//	metrics -> pushed into Runtime (file scan / SDK ingest)
//	gpu_stats, cancel, artifacts_list, artifacts_fetch -> methods below
type Backend interface {
	Kind() string
	Validate(ctx context.Context, spec *SubmitSpec) error
	Submit(ctx context.Context, spec *SubmitSpec) error
	Cancel(ctx context.Context, run *v1.Run) error
	ArtifactsList(ctx context.Context, run *v1.Run) ([]v1.Artifact, error)
	// ArtifactsFetch returns a local file path holding the artifact content.
	ArtifactsFetch(ctx context.Context, run *v1.Run, uri string) (string, error)
	GPUStats(ctx context.Context, run *v1.Run) ([]v1.GPUStat, error)
}

// UserError marks errors caused by bad input (HTTP 400, CLI exit 2).
type UserError struct{ Msg string }

func (e *UserError) Error() string { return e.Msg }

func Userf(format string, a ...any) error { return &UserError{Msg: fmt.Sprintf(format, a...)} }

// UnreachableError marks unreachable resources (HTTP 503, CLI exit 3).
type UnreachableError struct{ Msg string }

func (e *UnreachableError) Error() string { return e.Msg }

func Unreachablef(format string, a ...any) error {
	return &UnreachableError{Msg: fmt.Sprintf(format, a...)}
}

// Config is the backend-relevant part of the server configuration.
type Config struct {
	PublicURL      string // URL remote agents dial
	LocalURL       string // URL a local agent / sidecar dials
	AgentDist      string
	KnownHostsFile string
	RunTokenKey    []byte
	APIToken       string
	QueuePoll      time.Duration
}

// Runtime is the shared state all backends write into.
type Runtime struct {
	Store   *models.Store
	Logs    *logstore.Store
	Secrets *secrets.Manager
	Cfg     Config
}

// RunToken is the per-run token given to training code (MLDOJO_RUN_TOKEN) so
// the SDK can ingest metrics for exactly that run.
func (rt *Runtime) RunToken(runID string) string {
	m := hmac.New(sha256.New, rt.Cfg.RunTokenKey)
	m.Write([]byte("run:" + runID))
	return hex.EncodeToString(m.Sum(nil))
}

// CheckRunToken validates a run token in constant time.
func (rt *Runtime) CheckRunToken(runID, tok string) bool {
	return hmac.Equal([]byte(rt.RunToken(runID)), []byte(tok))
}

// SysLog appends a platform message to the run's system stream.
func (rt *Runtime) SysLog(runID, format string, a ...any) {
	line := fmt.Sprintf("[%s] %s\n", time.Now().Format("2006-01-02 15:04:05"), fmt.Sprintf(format, a...))
	rt.Logs.Append(runID, v1.StreamSystem, []byte(line))
}

// SetStatus updates the run phase, records an event and notifies watchers.
func (rt *Runtime) SetStatus(ctx context.Context, runID string, u models.RunUpdate) (*v1.Run, error) {
	run, changed, err := rt.Store.UpdateRunStatus(ctx, runID, u)
	if err != nil {
		return nil, err
	}
	if changed {
		payload := map[string]any{"phase": run.Status}
		if u.Message != "" {
			payload["message"] = u.Message
		}
		if run.ExitCode != nil {
			payload["exit_code"] = *run.ExitCode
		}
		rt.Store.AddEvent(ctx, runID, "status", payload)
		msg := "status -> " + run.Status
		if u.Message != "" {
			msg += ": " + u.Message
		}
		if run.ExitCode != nil && v1.Terminal(run.Status) {
			msg += fmt.Sprintf(" (exit code %d)", *run.ExitCode)
		}
		rt.SysLog(runID, "%s", msg)
		slog.Info("run status", "run", runID, "phase", run.Status, "msg", u.Message)
		observeStatus(run, u.Message)
		if run.Status == v1.PhaseFailed {
			rt.maybeRetry(ctx, run, u.FailureKind)
		}
		if run.Status == v1.PhaseSucceeded {
			rt.registerModel(ctx, run)
		}
	}
	rt.Logs.Broker.Publish(logstore.StatusTopic(runID), run)
	return run, nil
}

// observeStatus is the one place a run's fate becomes a metric and, for a
// failure, an alert. Every backend goes through SetStatus, so nothing that
// changes a phase can bypass it.
func observeStatus(run *v1.Run, message string) {
	switch {
	case run.Status == v1.PhaseStarting:
		// Runs are created queued, so created_at is when the wait began.
		obs.RunDispatch.With(run.BackendKind).Observe(time.Since(run.CreatedAt).Seconds())
	case v1.Terminal(run.Status):
		obs.RunsFinished.With(run.Status).Inc()
	}
	if run.Status != v1.PhaseFailed {
		return
	}
	obs.Alert(obs.Event{
		Type:  obs.EventRunFailed,
		Level: "error",
		Title: fmt.Sprintf("run failed: %s/%s %s", run.Project, run.Experiment, shortID(run.ID)),
		Text:  message,
		// Identifiers only: this leaves the network. No env, no command line.
		Fields: map[string]string{"run": run.ID, "project": run.Project,
			"experiment": run.Experiment, "target": run.Target, "exit_code": exitCode(run)},
		Key: obs.EventRunFailed + "/" + run.ID,
	})
}

// exitCode renders the run's exit code, or "-" while it has none.
func exitCode(run *v1.Run) string {
	if run.ExitCode == nil {
		return "-"
	}
	return strconv.Itoa(*run.ExitCode)
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// Fail marks a run failed with a message.
func (rt *Runtime) Fail(ctx context.Context, runID string, err error) {
	rt.SetStatus(ctx, runID, models.RunUpdate{Phase: v1.PhaseFailed, Message: err.Error()})
}

// Registry maps targets to backends.
type Registry struct {
	Node     *NodeBackend
	External *ExternalBackend
	Queues   map[string]Backend // by queue plugin name
}

func (r *Registry) For(t recipes.Target) (Backend, error) {
	switch t.Kind {
	case "node", "pool":
		return r.Node, nil
	case "external":
		return r.External, nil
	case "queue":
		if b, ok := r.Queues[t.Backend]; ok {
			return b, nil
		}
		return nil, Userf("unknown queue plugin %q (configured: %s)", t.Backend, r.queueNames())
	}
	return nil, Userf("unsupported target kind %q (want node, pool, queue or external)", t.Kind)
}

func (r *Registry) queueNames() string {
	names := make([]string, 0, len(r.Queues))
	for n := range r.Queues {
		names = append(names, n)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return "none; see api.queue_plugins"
	}
	return strings.Join(names, ", ")
}

// ForRun returns the backend of an existing run.
func (r *Registry) ForRun(run *v1.Run) (Backend, error) {
	t, err := recipes.ParseTarget(run.Target)
	if err != nil {
		return nil, err
	}
	return r.For(t)
}
