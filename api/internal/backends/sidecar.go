package backends

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/lovemoon-ai/mldojo/adapters/queue_sidecar"
	"github.com/lovemoon-ai/mldojo/api/internal/logstore"
	"github.com/lovemoon-ai/mldojo/api/internal/models"
	"github.com/lovemoon-ai/mldojo/datasets"
	v1 "github.com/lovemoon-ai/mldojo/proto/mldojo/v1"
)

// SidecarBackend is a QueueBackend provided by a queue plugin: an HTTP
// sidecar (docs/queue-plugins.md) that fronts some external scheduler.
// Submit through the sidecar, poll status, poll full log snapshots and diff
// them locally, scan metrics files in the output bucket. Queue ids, run
// backend ids and job artifact URIs are all prefixed with the plugin name.
type SidecarBackend struct {
	name   string
	url    string
	rt     *Runtime
	client *queue_sidecar.Client

	mu    sync.Mutex
	state map[string]*sidecarLogState
}

type sidecarLogState struct {
	size       int64    // bytes already stored
	prefixHash [32]byte // sha256 of the stored prefix
	verified   bool
	lastMetric time.Time
	noMetrics  bool
}

type sidecarHandle struct {
	JobID           string  `json:"job_id"`
	WorkspaceFolder string  `json:"workspace_folder"`
	DagID           *string `json:"dag_id"`
	URL             string  `json:"url,omitempty"`
	Queue           string  `json:"queue"`
}

func NewSidecarBackend(rt *Runtime, name, url string) *SidecarBackend {
	return &SidecarBackend{name: name, url: url, rt: rt, client: queue_sidecar.New(url), state: map[string]*sidecarLogState{}}
}

func (a *SidecarBackend) Name() string { return a.name }

func (a *SidecarBackend) Kind() string { return "queue" }

func (a *SidecarBackend) Ready(ctx context.Context) bool {
	if a.url == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	h, err := a.client.Health(ctx)
	return err == nil && h.OK
}

// Resources asks the plugin for live queue capacity with its default identity.
// A plugin without GET /resources (404/501) reports nothing.
func (a *SidecarBackend) Resources(ctx context.Context) ([]queue_sidecar.QueueResource, []string, error) {
	qs, warnings, err := a.client.Resources(ctx, "")
	var he *queue_sidecar.HTTPError
	if errors.As(err, &he) && (he.Status == 404 || he.Status == 501) {
		return nil, nil, nil
	}
	return qs, warnings, err
}

func (a *SidecarBackend) queue(ctx context.Context, id string) (*v1.Queue, error) {
	q, err := a.rt.Store.GetQueue(ctx, id)
	if models.IsNotFound(err) {
		return nil, Userf("unknown queue %q (see `mldojo queue ls`)", id)
	}
	return q, err
}

func defInt(d map[string]any, k string) int {
	switch v := d[k].(type) {
	case float64:
		return int(v)
	case int:
		return v
	}
	return 0
}

func defStr(d map[string]any, k string) string {
	if v, ok := d[k].(string); ok {
		return v
	}
	return ""
}

func (a *SidecarBackend) Validate(ctx context.Context, spec *SubmitSpec) error {
	if a.url == "" {
		return Userf("queue plugin %q has no sidecar url (api.queue_plugins)", a.name)
	}
	q, err := a.queue(ctx, spec.Target.ID)
	if err != nil {
		return err
	}
	if spec.Env.Type != "docker" && defStr(q.Defaults, "docker_image") == "" {
		return Userf("%s queues run docker images: add an env override `when: {backend_kind: queue, backend: %s}` with type docker + image, or set defaults.docker_image on the queue", a.name, a.name)
	}
	if spec.Run != nil && spec.Run.Metadata.CodeBundleURI == "" {
		return Userf("%s runs need a code bundle; submit with the mldojo CLI from your code directory", a.name)
	}
	return nil
}

func (a *SidecarBackend) Submit(ctx context.Context, spec *SubmitSpec) error {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		if err := a.submit(ctx, spec); err != nil {
			a.rt.Fail(ctx, spec.Run.ID, fmt.Errorf("%s submit: %w", a.name, err))
		}
	}()
	return nil
}

func (a *SidecarBackend) submit(ctx context.Context, spec *SubmitSpec) error {
	run := spec.Run
	q, err := a.queue(ctx, spec.Target.ID)
	if err != nil {
		return err
	}
	a.rt.SetStatus(ctx, run.ID, models.RunUpdate{Phase: v1.PhaseStarting, Message: "submitting to " + a.name + " " + spec.Target.Queue})
	creds, err := a.rt.Secrets.Resolve(ctx, q.Client.Credentials)
	if err != nil {
		return err
	}
	jobPw, err := a.rt.Secrets.Resolve(ctx, q.Client.JobPassword)
	if err != nil {
		return err
	}
	res, env, d := spec.Resources, spec.Env, q.Defaults
	image := env.Image
	if env.Type != "docker" || image == "" {
		image = defStr(d, "docker_image")
	}
	first := func(vals ...int) int {
		for _, v := range vals {
			if v > 0 {
				return v
			}
		}
		return 0
	}
	vars := map[string]string{}
	for k, v := range env.Vars {
		vars[k] = v
	}
	for k, v := range run.Metadata.ExtraEnv {
		vars[k] = v
	}
	for k, v := range q.Proxy.Env() {
		vars[k] = v
	}
	for k, v := range run.Metadata.Params {
		vars["MLDOJO_PARAM_"+envName(k)] = fmt.Sprint(v)
	}
	vars["MLDOJO_RUN_ID"] = run.ID
	vars["MLDOJO_RUN_TOKEN"] = a.rt.RunToken(run.ID)
	vars["MLDOJO_API_URL"] = a.rt.Cfg.PublicURL
	vars["MLDOJO_PROJECT"] = run.Project
	vars["MLDOJO_EXPERIMENT"] = run.Experiment
	outBucket := defStr(d, "output_bucket")
	var mounts []queue_sidecar.Mount
	for _, rd := range run.Metadata.Datasets {
		ds, err := a.rt.Store.GetDataset(ctx, rd.Name, rd.Version)
		if err != nil {
			return err
		}
		l := datasets.BucketLocation(ds)
		if l == nil {
			return Userf("dataset %s@%s has no bucket location; %s jobs can only mount buckets", rd.Name, rd.Version, a.name)
		}
		mounts = append(mounts, queue_sidecar.Mount{Bucket: l.Bucket, Path: l.Path, Mount: rd.Mount})
	}
	cmd := run.Metadata.Cmd
	if run.Metadata.Setup != "" {
		cmd = run.Metadata.Setup + "\n" + cmd
	}
	short := run.ID[:8]
	req := &queue_sidecar.SubmitRequest{
		RunID:        run.ID,
		JobName:      "mldojo-" + sanitize(run.Experiment) + "-" + short,
		QueueName:    spec.Target.Queue,
		ProjectID:    q.Client.ProjectID,
		DockerImage:  image,
		NumWorkers:   first(res.Workers, 1),
		GPUPerWorker: first(res.GPUPerWorker, res.GPUs, defInt(d, "gpu_per_worker")),
		CPUPerWorker: first(res.CPUPerWorker, defInt(d, "cpu_per_worker"), 4),
		CPUMemRatio:  first(res.CPUMemRatio, defInt(d, "cpu_mem_ratio"), 4),
		WallTimeMin:  first(res.WallTimeMin, defInt(d, "wall_time_min"), 60),
		Cmd:          cmd,
		Workdir:      run.Metadata.Workdir,
		BundleURL:    strings.TrimRight(a.rt.Cfg.LocalURL, "/") + "/api/v1/blobs/" + strings.TrimPrefix(run.Metadata.CodeBundleURI, "blob://"),
		BundleToken:  a.rt.Cfg.APIToken,
		Env:          vars,
		InputBucket:  defStr(d, "input_bucket"),
		OutputBucket: outBucket,
		Mounts:       mounts,
		JobPassword:  jobPw,
		Credentials:  creds,
	}
	if extra, ok := d["extra"].(map[string]any); ok {
		req.Extra = extra
	}
	resp, err := a.client.Submit(ctx, req)
	if err != nil {
		return err
	}
	h := sidecarHandle{JobID: resp.JobID, WorkspaceFolder: resp.WorkspaceFolder, DagID: resp.DagID, URL: resp.URL, Queue: spec.Target.Queue}
	if err := a.rt.Store.SetRunHandle(ctx, run.ID, h); err != nil {
		return err
	}
	a.rt.Store.MergeRunMetadata(ctx, run.ID, map[string]any{"external_url": resp.URL, "log_mode": "near-realtime"})
	a.rt.Store.AddEvent(ctx, run.ID, "submitted", map[string]any{"backend": a.name, "job_id": resp.JobID, "url": resp.URL})
	a.rt.SysLog(run.ID, "%s job %s submitted to %s (logs are polled every %s)", a.name, resp.JobID, spec.Target.Queue, a.rt.Cfg.QueuePoll)
	a.rt.SetStatus(ctx, run.ID, models.RunUpdate{Phase: v1.PhaseQueued, Message: a.name + " job " + resp.JobID})
	return nil
}

func sanitize(s string) string {
	var sb strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			sb.WriteRune(r)
		} else {
			sb.WriteByte('-')
		}
	}
	out := sb.String()
	if len(out) > 40 {
		out = out[:40]
	}
	return out
}

func handleOf(run *v1.Run) sidecarHandle {
	var h sidecarHandle
	json.Unmarshal(run.BackendHandle, &h)
	return h
}

func (a *SidecarBackend) creds(ctx context.Context, run *v1.Run) string {
	q, err := a.rt.Store.GetQueue(ctx, run.BackendID)
	if err != nil {
		return ""
	}
	c, _ := a.rt.Secrets.Resolve(ctx, q.Client.Credentials)
	return c
}

// Poll runs forever, refreshing this plugin's active runs (status, log diff, metrics).
func (a *SidecarBackend) Poll(ctx context.Context) {
	every := a.rt.Cfg.QueuePoll
	if every <= 0 {
		every = 5 * time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		runs, err := a.rt.Store.ListRuns(ctx, models.RunFilter{BackendKind: "queue", Active: true, Limit: 500})
		if err != nil {
			continue
		}
		var wg sync.WaitGroup
		sem := make(chan struct{}, 8)
		for i := range runs {
			run := runs[i]
			if !strings.HasPrefix(run.BackendID, a.name+"/") || handleOf(&run).JobID == "" {
				continue
			}
			wg.Add(1)
			sem <- struct{}{}
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				pctx, cancel := context.WithTimeout(ctx, every*4)
				defer cancel()
				a.refresh(pctx, &run)
			}()
		}
		wg.Wait()
	}
}

func (a *SidecarBackend) refresh(ctx context.Context, run *v1.Run) {
	h := handleOf(run)
	creds := a.creds(ctx, run)
	st, err := a.client.Status(ctx, h.JobID, creds)
	if err != nil {
		slog.Warn("queue plugin status", "plugin", a.name, "run", run.ID, "job", h.JobID, "err", err)
		return
	}
	// Always pull the log once more when the job reaches a terminal phase.
	a.pullLog(ctx, run, h, creds)
	a.pullMetrics(ctx, run, h, creds, v1.Terminal(st.Phase))
	phase := st.Phase
	if phase == "" {
		return
	}
	msg := st.Message
	if msg == "" && st.RawPhase != "" {
		msg = a.name + " phase " + st.RawPhase
	}
	// Artifacts before the terminal phase, not after: reaching succeeded is
	// what makes outputs.model register a version, and it can only register
	// checkpoints that have already been recorded. The agent path gets this
	// right by flushing in finish(); this one had it backwards.
	if v1.Terminal(phase) {
		a.recordArtifacts(ctx, run, h)
	}
	a.rt.SetStatus(ctx, run.ID, models.RunUpdate{Phase: phase, ExitCode: st.ExitCode, StartedAt: st.StartedAt, FinishedAt: st.FinishedAt, Message: msg})
	if v1.Terminal(phase) {
		a.mu.Lock()
		delete(a.state, run.ID)
		a.mu.Unlock()
	}
}

// pullLog fetches the full snapshot and appends only the new suffix
// (never push the whole snapshot to the browser).
func (a *SidecarBackend) pullLog(ctx context.Context, run *v1.Run, h sidecarHandle, creds string) {
	snap, err := a.client.Log(ctx, h.JobID, creds)
	if err != nil {
		return
	}
	a.mu.Lock()
	s := a.state[run.ID]
	if s == nil {
		s = &sidecarLogState{}
		a.state[run.ID] = s
	}
	a.mu.Unlock()
	data := []byte(snap)
	if !s.verified {
		// After a server restart: compare against what is already stored.
		stored, _, _ := a.rt.Logs.ReadAt(run.ID, v1.StreamStdout, 0, 0)
		s.size, s.prefixHash, s.verified = int64(len(stored)), sha256.Sum256(stored), true
	}
	if int64(len(data)) < s.size || sha256.Sum256(data[:s.size]) != s.prefixHash {
		// The snapshot no longer extends what we stored (log rotated/restarted).
		a.rt.SysLog(run.ID, "job log snapshot changed unexpectedly; appending the new snapshot in full")
		a.rt.Logs.Append(run.ID, v1.StreamStdout, []byte("\n----- [mldojo] new job log snapshot -----\n"))
		s.size = 0
	}
	if int64(len(data)) > s.size {
		if _, err := a.rt.Logs.Append(run.ID, v1.StreamStdout, data[s.size:]); err == nil {
			s.size = int64(len(data))
			s.prefixHash = sha256.Sum256(data)
		}
	}
}

func (a *SidecarBackend) pullMetrics(ctx context.Context, run *v1.Run, h sidecarHandle, creds string, final bool) {
	o := run.Metadata.Outputs
	if o == nil || len(o.Metrics) == 0 {
		return
	}
	a.mu.Lock()
	s := a.state[run.ID]
	a.mu.Unlock()
	if s == nil || s.noMetrics || (!final && time.Since(s.lastMetric) < 30*time.Second) {
		return
	}
	s.lastMetric = time.Now()
	// Paths are relative: to the job's /job_data dir by default, or to
	// defaults.metrics_bucket when the queue sets one.
	var paths []queue_sidecar.MetricPath
	for _, m := range o.Metrics {
		paths = append(paths, queue_sidecar.MetricPath{Type: m.Type, Path: path.Clean(strings.TrimPrefix(m.Path, "./"))})
	}
	q, _ := a.rt.Store.GetQueue(ctx, run.BackendID)
	bucket := ""
	if q != nil {
		bucket = defStr(q.Defaults, "metrics_bucket")
	}
	pts, err := a.client.Metrics(ctx, h.JobID, creds, bucket, paths, true)
	var he *queue_sidecar.HTTPError
	if errors.As(err, &he) && he.Status == 501 {
		s.noMetrics = true
		a.rt.SysLog(run.ID, "%s metrics scan unavailable: %s", a.name, he.Msg)
		return
	}
	if err != nil || len(pts) == 0 {
		return
	}
	if err := a.rt.Store.UpsertMetrics(ctx, run.ID, pts); err == nil {
		a.rt.Logs.Broker.Publish(logstore.MetricTopic(run.ID), pts)
	}
}

// artifactsBucket is where job outputs are listed: "" = the job's own data
// dir (default) or defaults.artifacts_bucket.
func (a *SidecarBackend) artifactsBucket(ctx context.Context, run *v1.Run) string {
	if q, _ := a.rt.Store.GetQueue(ctx, run.BackendID); q != nil {
		return defStr(q.Defaults, "artifacts_bucket")
	}
	return ""
}

// jobURI renders <plugin>://<job>/<path> (job data) or bucket://<bucket>/<path>.
func jobURI(plugin, jobID, bucket, p string) string {
	if bucket == "" {
		return plugin + "://" + jobID + "/" + strings.TrimPrefix(p, "/")
	}
	return "bucket://" + bucket + "/" + strings.TrimPrefix(p, "/")
}

// recordArtifacts lists outputs through the sidecar (artifacts stay in the
// bucket/job data; only URIs are recorded).
func (a *SidecarBackend) recordArtifacts(ctx context.Context, run *v1.Run, h sidecarHandle) {
	o := run.Metadata.Outputs
	if o == nil || h.JobID == "" {
		return
	}
	var globs []queue_sidecar.FileGlob
	for _, kg := range []struct{ k, g string }{{"log", o.Logs}, {"ckpt", o.Checkpoints}, {"video", o.Videos}, {"image", o.Images}} {
		if kg.g != "" {
			globs = append(globs, queue_sidecar.FileGlob{Kind: kg.k, Glob: kg.g})
		}
	}
	if len(globs) == 0 {
		return
	}
	bucket := a.artifactsBucket(ctx, run)
	files, err := a.client.Files(ctx, h.JobID, a.creds(ctx, run), bucket, globs)
	if err != nil {
		slog.Warn("queue plugin artifacts", "plugin", a.name, "run", run.ID, "err", err)
		return
	}
	arts := make([]v1.Artifact, 0, len(files))
	for _, f := range files {
		arts = append(arts, v1.Artifact{Kind: f.Kind, URI: jobURI(a.name, h.JobID, bucket, f.Path), SizeBytes: f.Size})
	}
	a.rt.Store.UpsertArtifacts(ctx, run.ID, arts)
}

func (a *SidecarBackend) Cancel(ctx context.Context, run *v1.Run) error {
	if v1.Terminal(run.Status) {
		return nil
	}
	h := handleOf(run)
	if h.JobID != "" {
		if err := a.client.Cancel(ctx, h.JobID, a.creds(ctx, run)); err != nil {
			return err
		}
	}
	_, err := a.rt.SetStatus(ctx, run.ID, models.RunUpdate{Phase: v1.PhaseCancelled, Message: "cancelled by user"})
	return err
}

func (a *SidecarBackend) ArtifactsList(ctx context.Context, run *v1.Run) ([]v1.Artifact, error) {
	a.recordArtifacts(ctx, run, handleOf(run))
	return a.rt.Store.ListArtifacts(ctx, run.ID)
}

// ArtifactsFetch downloads one output through the sidecar into the server
// cache ("temporary download" preview). Nothing is copied back into
// central storage permanently.
func (a *SidecarBackend) ArtifactsFetch(ctx context.Context, run *v1.Run, uri string) (string, error) {
	h := handleOf(run)
	arts, err := a.rt.Store.ListArtifacts(ctx, run.ID)
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
	bucket, p := "", ""
	if rest, ok := strings.CutPrefix(uri, a.name+"://"+h.JobID+"/"); ok {
		p = rest
	} else if rest, ok := strings.CutPrefix(uri, "bucket://"); ok {
		bucket, p, _ = strings.Cut(rest, "/")
	} else {
		return "", Userf("not a %s artifact of this run: %s", a.name, uri)
	}
	dest := a.rt.Logs.CachePath(run.ID + "|" + uri)
	if st, err := os.Stat(dest); err == nil && (art.SizeBytes == 0 || st.Size() == art.SizeBytes) {
		return dest, nil
	}
	tmp, err := a.rt.Logs.TempFile("queue-art-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	_, err = a.client.Download(ctx, h.JobID, a.creds(ctx, run), bucket, p, tmp)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", err
	}
	return dest, os.Rename(tmp.Name(), dest)
}

func (a *SidecarBackend) GPUStats(ctx context.Context, run *v1.Run) ([]v1.GPUStat, error) {
	return nil, nil
}
