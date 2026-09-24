package backends

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"

	"github.com/lovemoon-ai/mldojo/api/internal/models"
	v1 "github.com/lovemoon-ai/mldojo/proto/mldojo/v1"
)

// Retry.
//
// A run used to be a single attempt. When an agent was unreachable for
// thirty minutes the reaper failed its run, and a multi-day training job was
// simply gone -- the resume from the last checkpoint was done by hand, by a
// person watching. Nothing about that is a decision a human needs to make.
//
// What is a decision is *which* failures to retry. A job that failed on its
// own exit code will fail the same way again, so the default only covers
// failures the platform caused: a lost agent, a node removed, a spawn that
// never landed.

// FailInfra marks a failure as the platform's rather than the job's, which
// is what makes it eligible for a retry.
func (rt *Runtime) FailInfra(ctx context.Context, runID string, err error) {
	rt.SetStatus(ctx, runID, models.RunUpdate{Phase: v1.PhaseFailed, Message: err.Error(), FailureKind: "infra"})
}

// maybeRetry queues the next attempt of a failed run, and reports whether it
// did. Called from SetStatus, so no backend can fail a run past its policy.
func (rt *Runtime) maybeRetry(ctx context.Context, run *v1.Run, kind string) bool {
	p := run.Metadata.Retry
	if p.Retries() <= 0 {
		return false
	}
	attempt := run.Metadata.Attempt
	if attempt == 0 {
		attempt = 1
	}
	if attempt > p.Retries() {
		rt.SysLog(run.ID, "not retrying: attempt %d of %d already used", attempt, p.Retries()+1)
		return false
	}
	if kind != "infra" && !p.RetryOnAny() {
		// Saying so matters: otherwise "retry: {max: 2}" looks broken to
		// somebody whose script is exiting non-zero.
		rt.SysLog(run.ID, "not retrying: the job failed on its own (retry.on is %q, not \"any\")", orDefault(p.On, "infra"))
		return false
	}

	next := *run
	next.ID = ""
	next.Status = v1.PhaseQueued
	next.ExitCode, next.StartedAt, next.FinishedAt = nil, nil, nil
	next.BackendHandle = nil
	next.Metadata.Attempt = attempt + 1
	next.Metadata.Message = ""
	next.Metadata.RetryOf = orDefault(run.Metadata.RetryOf, run.ID)
	next.Name = attemptName(run.Name, attempt+1)
	// A pool run goes back in the pool: the node it was on may be the
	// reason it failed.
	if strings.HasPrefix(next.Target, "pool:") {
		next.BackendID = ""
	}

	if uri := rt.resumePoint(ctx, run); uri != "" {
		if next.Metadata.ExtraEnv == nil {
			next.Metadata.ExtraEnv = map[string]string{}
		}
		if _, _, ok := ParseNodeURI(uri); ok {
			_, path, _ := ParseNodeURI(uri)
			next.Metadata.ExtraEnv["MLDOJO_RESUME_FROM"] = path
		} else {
			next.Metadata.ExtraEnv["MLDOJO_RESUME_FROM"] = uri
		}
		next.Metadata.ResumedFrom = uri
	}

	if err := rt.Store.InsertRun(ctx, &next); err != nil {
		slog.Warn("retry: insert run", "run", run.ID, "err", err)
		return false
	}
	from := next.Metadata.ResumedFrom
	if from == "" {
		from = "no checkpoint found, starting over"
	}
	rt.Store.AddEvent(ctx, run.ID, "retried", map[string]any{"next": next.ID, "attempt": next.Metadata.Attempt})
	rt.Store.AddEvent(ctx, next.ID, "retry_of", map[string]any{"previous": run.ID, "resume_from": next.Metadata.ResumedFrom})
	rt.SysLog(run.ID, "retrying as %s (attempt %d of %d)", next.ID, next.Metadata.Attempt, p.Retries()+1)
	rt.SysLog(next.ID, "attempt %d of %d, after %s failed: %s",
		next.Metadata.Attempt, p.Retries()+1, run.ID, from)
	slog.Info("retrying run", "run", run.ID, "next", next.ID, "attempt", next.Metadata.Attempt)
	return true
}

// resumePoint picks the checkpoint the next attempt should continue from:
// the newest of the failed attempt's checkpoints, by step where the name
// carries one, and by recency otherwise.
func (rt *Runtime) resumePoint(ctx context.Context, run *v1.Run) string {
	p := run.Metadata.Retry
	if p == nil || p.ResumeFrom == "" {
		return ""
	}
	arts, err := rt.Store.ListArtifacts(ctx, run.ID)
	if err != nil {
		return ""
	}
	re := globToRegexp(p.ResumeFrom)
	best, bestStep := "", int64(-1)
	for _, a := range arts {
		path := a.URI
		if _, p, ok := ParseNodeURI(a.URI); ok {
			path = p
		}
		if !re.MatchString(path) {
			continue
		}
		step := checkpointStep(path)
		// Artifacts come back ordered, so equal steps keep the last seen,
		// which is the later path.
		if step >= bestStep {
			best, bestStep = a.URI, step
		}
	}
	return best
}

var ckptStepRe = regexp.MustCompile(`(?i)(?:step|iter|epoch|ckpt|checkpoint)[_-]?(\d+)`)

func checkpointStep(path string) int64 {
	m := ckptStepRe.FindStringSubmatch(path)
	if m == nil {
		return 0
	}
	n, _ := strconv.ParseInt(m[1], 10, 64)
	return n
}

// globToRegexp converts a resume_from glob. It is deliberately anchored at
// the end only: the glob usually names the checkpoint layout, not the whole
// absolute path.
func globToRegexp(g string) *regexp.Regexp {
	var sb strings.Builder
	sb.WriteString("(^|/)")
	for i := 0; i < len(g); i++ {
		switch c := g[i]; c {
		case '*':
			if i+1 < len(g) && g[i+1] == '*' {
				sb.WriteString(".*")
				i++
			} else {
				sb.WriteString("[^/]*")
			}
		case '?':
			sb.WriteString("[^/]")
		default:
			sb.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	sb.WriteString("$")
	re, err := regexp.Compile(sb.String())
	if err != nil {
		return regexp.MustCompile(`$^`) // matches nothing
	}
	return re
}

// attemptName keeps attempts distinguishable in a run list without letting
// the suffix accumulate.
func attemptName(name string, attempt int) string {
	if i := strings.LastIndex(name, " (attempt "); i > 0 {
		name = name[:i]
	}
	return fmt.Sprintf("%s (attempt %d)", name, attempt)
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
