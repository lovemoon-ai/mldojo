// Package v1 holds the wire schema shared by mldojo-api, mldojo-agent, the
// mldojo CLI and (by documentation) the web app. Everything is JSON encoded.
package v1

import (
	"encoding/json"
	"time"
)

// Run phases.
const (
	PhaseQueued    = "queued"
	PhaseStarting  = "starting"
	PhaseRunning   = "running"
	PhaseSucceeded = "succeeded"
	PhaseFailed    = "failed"
	PhaseCancelled = "cancelled"
)

// Terminal reports whether a run phase is final.
func Terminal(phase string) bool {
	return phase == PhaseSucceeded || phase == PhaseFailed || phase == PhaseCancelled
}

// Log streams.
const (
	StreamStdout = "stdout"
	StreamStderr = "stderr"
	StreamSystem = "system" // platform messages (dataset sync, submit, poll errors)
)

// Error is the JSON error body returned by the API for every non-2xx response.
type Error struct {
	Error string `json:"error"`
	Code  string `json:"code"` // user_error | unreachable | backend_error | conflict | not_found | unauthorized | internal
}

type Project struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Owner       string    `json:"owner"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	// MaxConcurrentRuns caps how many of this project's runs may be active
	// at once, so one sweep cannot take the whole cluster. 0 = unlimited.
	MaxConcurrentRuns int `json:"max_concurrent_runs,omitempty"`
	// Aggregates (list/show only).
	ExperimentCount int `json:"experiment_count"`
	RunCount        int `json:"run_count"`
	ActiveRuns      int `json:"active_runs"`
}

type Experiment struct {
	ID          string         `json:"id"`
	ProjectID   string         `json:"project_id"`
	Project     string         `json:"project"`
	Name        string         `json:"name"`
	Description string         `json:"description"`
	RecipeYAML  string         `json:"recipe_yaml"`
	Tags        []string       `json:"tags"`
	CreatedAt   time.Time      `json:"created_at"`
	UpdatedAt   time.Time      `json:"updated_at"`
	RunCounts   map[string]int `json:"run_counts"` // by status
}

type Run struct {
	ID            string          `json:"id"`
	ExperimentID  string          `json:"experiment_id"`
	Project       string          `json:"project"`
	Experiment    string          `json:"experiment"`
	Name          string          `json:"name"` // e.g. "seed=0"
	Target        string          `json:"target"`
	BackendKind   string          `json:"backend_kind"`
	BackendID     string          `json:"backend_id"`
	BackendHandle json.RawMessage `json:"backend_handle"`
	Resources     json.RawMessage `json:"resources"`
	Env           json.RawMessage `json:"env"`
	CodeCommit    string          `json:"code_commit"`
	CodePatchURI  string          `json:"code_patch_uri"`
	Status        string          `json:"status"`
	ExitCode      *int            `json:"exit_code"`
	StartedAt     *time.Time      `json:"started_at"`
	FinishedAt    *time.Time      `json:"finished_at"`
	CreatedAt     time.Time       `json:"created_at"`
	Metadata      RunMetadata     `json:"metadata"`
}

// RunMetadata is stored in runs.metadata.
type RunMetadata struct {
	Tags          []string          `json:"tags,omitempty"`
	Params        map[string]any    `json:"params,omitempty"`
	Cmd           string            `json:"cmd,omitempty"`
	Workdir       string            `json:"workdir,omitempty"`
	Setup         string            `json:"setup,omitempty"`
	Wandb         string            `json:"wandb,omitempty"`
	Notes         string            `json:"notes,omitempty"`
	Message       string            `json:"message,omitempty"`
	CodeSource    string            `json:"code_source,omitempty"` // git | local | inline-patch
	CodeRepo      string            `json:"code_repo,omitempty"`
	CodeRef       string            `json:"code_ref,omitempty"`
	CodeBundleURI string            `json:"code_bundle_uri,omitempty"`
	CodeDirty     bool              `json:"code_dirty,omitempty"`
	EnvLock       string            `json:"env_lock,omitempty"`     // pip freeze / conda list at run time
	ImageDigest   string            `json:"image_digest,omitempty"` // the digest behind a mutable docker tag
	Datasets      []RunDataset      `json:"datasets,omitempty"`
	Models        []RunModel        `json:"models,omitempty"` // registered versions this run read
	Outputs       *Outputs          `json:"outputs,omitempty"`
	ExtraEnv      map[string]string `json:"extra_env,omitempty"`
	LogMode       string            `json:"log_mode,omitempty"` // "realtime" | "near-realtime"
	Retry         *RetryPolicy      `json:"retry,omitempty"`
	Attempt       int               `json:"attempt,omitempty"`  // 1 for the first try
	RetryOf       string            `json:"retry_of,omitempty"` // the first attempt's run id
	ResumedFrom   string            `json:"resumed_from,omitempty"`
	ExternalURL   string            `json:"external_url,omitempty"`
	Submitter     string            `json:"submitter,omitempty"`
	Summary       string            `json:"summary,omitempty"`
}

// RetryPolicy re-runs a job the platform lost rather than one that was
// wrong. A thirty-minute network blip used to cost a multi-day training run,
// and resuming it from the last checkpoint was done by hand.
type RetryPolicy struct {
	Max int `json:"max,omitempty" yaml:"max"`
	// On is "infra" (default) or "any". Retrying a job that failed on its
	// own exit code just runs the same bug again.
	On string `json:"on,omitempty" yaml:"on"`
	// ResumeFrom is a glob over the previous attempt's checkpoints. The
	// newest match is passed to the next attempt as MLDOJO_RESUME_FROM.
	ResumeFrom string `json:"resume_from,omitempty" yaml:"resume_from"`
}

// Retries reports the configured attempt cap.
func (p *RetryPolicy) Retries() int {
	if p == nil {
		return 0
	}
	return p.Max
}

// RetryOnAny reports whether a job's own failure should be retried too.
func (p *RetryPolicy) RetryOnAny() bool { return p != nil && p.On == "any" }

type RunDataset struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Mount   string `json:"mount"`
}

type Outputs struct {
	Logs        string          `json:"logs,omitempty"`
	Checkpoints string          `json:"checkpoints,omitempty"`
	Videos      string          `json:"videos,omitempty"`
	Images      string          `json:"images,omitempty"`
	Metrics     []MetricsSource `json:"metrics,omitempty"`
	// Episodes points at what an evaluation harness already writes -- a
	// manifest CSV, a JSONL file, or a directory of episodeN_success.mp4 --
	// so a result can be collected without changing the harness.
	Episodes []EpisodesSource `json:"episodes,omitempty"`
	// Model is the registry entry a successful run registers its newest
	// checkpoint under, closing the loop back to the recipe that reads it.
	Model string `json:"model,omitempty"`
}

// RunModel records a registry version a run consumed: which model, which
// version, and the path it resolved to.
type RunModel struct {
	Name    string `json:"name"` // project/name
	Version int    `json:"version"`
	URI     string `json:"uri"`
	As      string `json:"as,omitempty"` // the parameter it filled
}

// EpisodesSource is where a run's per-episode results can be read.
type EpisodesSource struct {
	// Type is csv | jsonl | filenames. Empty means: work it out from the
	// path, which is what people will expect.
	Type string `json:"type,omitempty" yaml:"type"`
	Path string `json:"path" yaml:"path"`
}

type MetricsSource struct {
	Type string `json:"type"` // tensorboard | jsonl
	Path string `json:"path"`
}

// Resources as resolved at submit time (defaults + overrides for the target).
type Resources struct {
	GPUs         int    `json:"gpus,omitempty" yaml:"gpus"`
	GPUType      string `json:"gpu_type,omitempty" yaml:"gpu_type"`
	MinMemGB     int    `json:"min_mem_gb,omitempty" yaml:"min_mem_gb"`
	Workers      int    `json:"workers,omitempty" yaml:"workers"`
	GPUPerWorker int    `json:"gpu_per_worker,omitempty" yaml:"gpu_per_worker"`
	CPUPerWorker int    `json:"cpu_per_worker,omitempty" yaml:"cpu_per_worker"`
	CPUMemRatio  int    `json:"cpu_mem_ratio,omitempty" yaml:"cpu_mem_ratio"`
	WallTimeMin  int    `json:"wall_time_min,omitempty" yaml:"wall_time_min"`
}

// EnvSpec as resolved at submit time.
type EnvSpec struct {
	Type  string            `json:"type" yaml:"type"` // docker | conda | venv | none
	Spec  string            `json:"spec,omitempty" yaml:"spec"`
	Image string            `json:"image,omitempty" yaml:"image"`
	Vars  map[string]string `json:"vars,omitempty" yaml:"vars"`
}

type Node struct {
	ID                string          `json:"id"`
	DisplayName       string          `json:"display_name"`
	Labels            []string        `json:"labels"`
	Connection        NodeConnection  `json:"connection"`
	Proxy             *Proxy          `json:"proxy"`
	Capacity          *Capacity       `json:"capacity"`
	AgentStatus       string          `json:"agent_status"` // online | offline | degraded
	AgentVersion      string          `json:"agent_version"`
	LastHeartbeat     *time.Time      `json:"last_heartbeat"`
	WorkdirRoot       string          `json:"workdir_root"`
	DatasetsCacheRoot string          `json:"datasets_cache_root"`
	CreatedAt         time.Time       `json:"created_at"`
	UpdatedAt         time.Time       `json:"updated_at"`
	GPUStats          []GPUStat       `json:"gpu_stats,omitempty"`
	Disks             []DiskStat      `json:"disks,omitempty"`
	ActiveRuns        int             `json:"active_runs"`
	MaxRuns           int             `json:"max_runs,omitempty"` // 0 = unlimited
	Extra             json.RawMessage `json:"-"`
}

type NodeConnection struct {
	Type            string   `json:"type" yaml:"type"` // ssh | local
	Host            string   `json:"host,omitempty" yaml:"host"`
	Port            int      `json:"port,omitempty" yaml:"port"`
	User            string   `json:"user,omitempty" yaml:"user"`
	Identity        string   `json:"identity,omitempty" yaml:"identity"` // secret://ssh_keys/x or a path
	Password        string   `json:"password,omitempty" yaml:"password"` // secret://passwords/x
	Via             []Via    `json:"via,omitempty" yaml:"via"`
	ExtraSSHOptions []string `json:"extra_ssh_options,omitempty" yaml:"extra_ssh_options"`
	// AgentServerURL overrides the server URL the agent dials back to.
	AgentServerURL string `json:"agent_server_url,omitempty" yaml:"agent_server_url"`
	// ReverseTunnel makes the server keep an SSH session open and forward a
	// port on the node back to the API (for nodes that cannot reach the server).
	ReverseTunnel bool `json:"reverse_tunnel,omitempty" yaml:"reverse_tunnel"`
	// ReverseTunnelMode is how: "forward" (default, ssh -R) or "stdio", a
	// relay over a plain exec session for sshds that forbid forwarding.
	ReverseTunnelMode string `json:"reverse_tunnel_mode,omitempty" yaml:"reverse_tunnel_mode"`
}

type Via struct {
	Node string `json:"node" yaml:"node"` // node id, or "local" for a direct dial
}

type Proxy struct {
	HTTP    string   `json:"http,omitempty" yaml:"http"`
	HTTPS   string   `json:"https,omitempty" yaml:"https"`
	NoProxy []string `json:"no_proxy,omitempty" yaml:"no_proxy"`
}

// Env renders the proxy as environment variables.
func (p *Proxy) Env() map[string]string {
	env := map[string]string{}
	if p == nil {
		return env
	}
	if p.HTTP != "" {
		env["http_proxy"], env["HTTP_PROXY"] = p.HTTP, p.HTTP
	}
	if p.HTTPS != "" {
		env["https_proxy"], env["HTTPS_PROXY"] = p.HTTPS, p.HTTPS
	}
	if len(p.NoProxy) > 0 {
		v := ""
		for i, s := range p.NoProxy {
			if i > 0 {
				v += ","
			}
			v += s
		}
		env["no_proxy"], env["NO_PROXY"] = v, v
	}
	return env
}

type Capacity struct {
	GPUs     []GPUInfo `json:"gpus"`
	CPU      int       `json:"cpu"`
	MemGB    int       `json:"mem_gb"`
	DiskGB   int       `json:"disk_gb"`
	Hostname string    `json:"hostname,omitempty"`
	OS       string    `json:"os,omitempty"`
	Arch     string    `json:"arch,omitempty"`
	// MldojoDir is ~/.mldojo resolved: the disk runs, datasets and envs fill.
	MldojoDir string `json:"mldojo_dir,omitempty"`
}

type GPUInfo struct {
	Index int    `json:"index"`
	Model string `json:"model"`
	MemGB int    `json:"mem_gb"`
}

// DiskStat is one filesystem a node writes to. Paths matter more than
// mount points here: a job can have its workdir on a small container overlay
// and its checkpoints on a shared cluster filesystem, and "free disk" means
// nothing until you say which one.
type DiskStat struct {
	Path    string  `json:"path"`  // a directory that lives on this filesystem
	Mount   string  `json:"mount"` // where it is mounted, when that is knowable
	TotalGB float64 `json:"total_gb"`
	FreeGB  float64 `json:"free_gb"`
}

// GPUProc is a process holding a GPU. Without this, a card a stranger's job
// is using is indistinguishable from an idle one, and nobody can tell whether
// the memory sitting on a card belongs to a live job or a leaked process.
type GPUProc struct {
	PID       int     `json:"pid"`
	Name      string  `json:"name,omitempty"` // as nvidia-smi reports it
	User      string  `json:"user,omitempty"` // empty when the pid is not in our namespace
	Cmd       string  `json:"cmd,omitempty"`  // truncated command line
	MemUsedMB float64 `json:"mem_used_mb"`
	RunID     string  `json:"run_id,omitempty"` // set when it belongs to an MLDojo run
}

// Unresolved reports that the node could not look this process up at all --
// neither an owner nor a command line. That happens when nvidia-smi reports
// pids from a namespace the agent cannot see, and it means the process
// cannot be attributed either way.
func (p GPUProc) Unresolved() bool { return p.User == "" && p.Cmd == "" }

type GPUStat struct {
	Index     int       `json:"index"`
	UUID      string    `json:"uuid,omitempty"`
	Model     string    `json:"model"`
	Util      float64   `json:"util"`        // percent
	MemUsedMB float64   `json:"mem_used_mb"` // MiB
	MemTotMB  float64   `json:"mem_total_mb"`
	Temp      float64   `json:"temp"` // celsius
	RunIDs    []string  `json:"run_ids,omitempty"`
	Procs     []GPUProc `json:"procs,omitempty"`
}

// FreeMB is the memory nothing is holding. This is the number that decides
// whether a job fits, and it does not care who the memory belongs to.
func (g GPUStat) FreeMB() float64 {
	if f := g.MemTotMB - g.MemUsedMB; f > 0 {
		return f
	}
	return 0
}

// Foreign reports memory held by processes MLDojo did not start.
//
// A process the node could not resolve at all is only counted when nothing
// of ours is on the card. Some hosts -- a container sharing a GPU but not a
// pid namespace -- have nvidia-smi report pids the agent cannot look up, so
// a run's own training process arrives anonymous. On a card we did place a
// run on, the honest reading of anonymous memory is "probably that run",
// not "a stranger": calling it foreign double-counts our own job and warns
// us that we are about to run out of memory because of ourselves.
func (g GPUStat) Foreign() (procs int, memMB float64) {
	ours := len(g.RunIDs) > 0
	for _, p := range g.Procs {
		if p.RunID == "" && (!ours || !p.Unresolved()) {
			procs++
			memMB += p.MemUsedMB
		}
	}
	return procs, memMB
}

// Episode is one evaluation rollout. For a policy the unit of result is not
// a scalar per step but a trial that either worked or did not, identified by
// the seed that produced it so two evaluations can be compared trial by
// trial rather than rate to rate.
type Episode struct {
	Index      int            `json:"index"`
	Seed       *int64         `json:"seed,omitempty"`
	Success    bool           `json:"success"`
	Steps      int            `json:"steps,omitempty"`
	DurationMS int64          `json:"duration_ms,omitempty"`
	VideoURI   string         `json:"video_uri,omitempty"`
	Extra      map[string]any `json:"extra,omitempty"`
}

// EpisodeSummary is a run's evaluation result in one line.
type EpisodeSummary struct {
	Total       int     `json:"total"`
	Successes   int     `json:"successes"`
	SuccessRate float64 `json:"success_rate"`
	WithVideo   int     `json:"with_video"`
}

// Summarize counts an episode list.
func Summarize(eps []Episode) EpisodeSummary {
	s := EpisodeSummary{Total: len(eps)}
	for _, e := range eps {
		if e.Success {
			s.Successes++
		}
		if e.VideoURI != "" {
			s.WithVideo++
		}
	}
	if s.Total > 0 {
		s.SuccessRate = float64(s.Successes) / float64(s.Total)
	}
	return s
}

// MetricSuccessRate is where an evaluation's result is published as a
// metric, so ranking, comparison and the AI summaries work on it without
// knowing anything about episodes.
const (
	MetricSuccessRate = "eval/success_rate"
	MetricEpisodes    = "eval/episodes"
)

// GPUSamplePoint is one bucket of a card's history.
type GPUSamplePoint struct {
	TS           time.Time `json:"ts"`
	Util         float64   `json:"util"`
	MemUsedMB    float64   `json:"mem_used_mb"`
	MemTotMB     float64   `json:"mem_total_mb"`
	Temp         float64   `json:"temp"`
	ForeignMemMB float64   `json:"foreign_mem_mb,omitempty"`
	Mine         bool      `json:"mine,omitempty"` // an MLDojo run held it
}

// IdleGPU is a card a run held without using: allocated, and at 0%.
type IdleGPU struct {
	NodeID    string    `json:"node_id"`
	GPUIndex  int       `json:"gpu_index"`
	RunID     string    `json:"run_id"`
	Project   string    `json:"project,omitempty"`
	HeldHours float64   `json:"held_hours"`
	BusyHours float64   `json:"busy_hours"`
	AvgUtil   float64   `json:"avg_util"`
	LastSeen  time.Time `json:"last_seen"`
}

type Queue struct {
	ID           string          `json:"id"`
	Backend      string          `json:"backend"`
	DisplayName  string          `json:"display_name"`
	Labels       []string        `json:"labels"`
	Client       QueueClient     `json:"client"`
	Defaults     map[string]any  `json:"defaults"`
	CapacityHint map[string]any  `json:"capacity_hint"`
	Proxy        *Proxy          `json:"proxy"`
	CreatedAt    time.Time       `json:"created_at"`
	UpdatedAt    time.Time       `json:"updated_at"`
	ActiveRuns   int             `json:"active_runs"`
	Extra        json.RawMessage `json:"-"`
}

type QueueClient struct {
	SDK         string `json:"sdk" yaml:"sdk"`
	Credentials string `json:"credentials,omitempty" yaml:"credentials"`
	ProjectID   string `json:"project_id,omitempty" yaml:"project_id"`
	JobPassword string `json:"job_password,omitempty" yaml:"job_password"`
}

type Dataset struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	Version   string            `json:"version"`
	Mount     string            `json:"mount"`
	Locations []DatasetLocation `json:"locations"`
	CreatedAt time.Time         `json:"created_at"`
}

type DatasetLocation struct {
	ID            string `json:"id,omitempty"`
	Kind          string `json:"kind"` // node_path | bucket
	Node          string `json:"node,omitempty"`
	Path          string `json:"path,omitempty"`
	Provider      string `json:"provider,omitempty"`
	Bucket        string `json:"bucket,omitempty"`
	Credentials   string `json:"credentials,omitempty"`
	Authoritative bool   `json:"authoritative"`
}

type SecretMeta struct {
	Namespace   string    `json:"namespace"`
	Name        string    `json:"name"`
	Ref         string    `json:"ref"` // secret://ns/name
	Description string    `json:"description,omitempty"`
	Size        int       `json:"size"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type SecretStatus struct {
	Unlocked  bool   `json:"unlocked"`
	Source    string `json:"source,omitempty"` // keychain | env | file | api
	Recipient string `json:"recipient,omitempty"`
}

type Artifact struct {
	ID        string    `json:"id"`
	RunID     string    `json:"run_id"`
	Kind      string    `json:"kind"` // log | ckpt | video | image | other
	URI       string    `json:"uri"`
	SizeBytes int64     `json:"size_bytes"`
	SHA256    string    `json:"sha256,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// SweepResult is one run's standing in a sweep.
type SweepResult struct {
	ID     string         `json:"id"`
	Name   string         `json:"name"`
	Status string         `json:"status"`
	Params map[string]any `json:"params,omitempty"`
	Value  *Float         `json:"value"`
	Step   int64          `json:"step"`
}

// Step is a compact point for series payloads: repeating the key and
// timestamp on every sample triples the size of a comparison response.
type Step struct {
	Step  int64 `json:"step"`
	Value Float `json:"value"`
}

type MetricPoint struct {
	Step  int64     `json:"step"`
	Key   string    `json:"key"`
	Value float64   `json:"value"`
	TS    time.Time `json:"ts"`
}

type RunEvent struct {
	ID      int64           `json:"id"`
	RunID   string          `json:"run_id"`
	TS      time.Time       `json:"ts"`
	Kind    string          `json:"kind"`
	Payload json.RawMessage `json:"payload"`
}

// Frame is the envelope for every WebSocket data-plane message.
type Frame struct {
	Seq     int64           `json:"seq"`
	TS      time.Time       `json:"ts"`
	Kind    string          `json:"kind"` // log | metric | gpu | status | eof | error
	Payload json.RawMessage `json:"payload"`
}

type LogPayload struct {
	Stream string `json:"stream"`
	Offset int64  `json:"offset"`
	Data   string `json:"data"`
}

// SubmitRequest is the body of POST /runs.
type SubmitRequest struct {
	RecipeYAML string         `json:"recipe_yaml"`
	Target     string         `json:"target"`
	GPUs       int            `json:"gpus,omitempty"`       // overrides resources.gpus
	Matrix     []string       `json:"matrix,omitempty"`     // param names to expand
	Params     map[string]any `json:"params,omitempty"`     // overrides
	Project    string         `json:"project,omitempty"`    // overrides metadata.project
	Experiment string         `json:"experiment,omitempty"` // overrides metadata.name
	Code       *CodeInfo      `json:"code,omitempty"`
	Notes      string         `json:"notes,omitempty"`
	DryRun     bool           `json:"dry_run,omitempty"`
}

// CodeInfo is filled by the CLI after bundling the local working tree.
type CodeInfo struct {
	Source    string `json:"source"` // git | local | inline-patch
	Repo      string `json:"repo,omitempty"`
	Ref       string `json:"ref,omitempty"`
	Commit    string `json:"commit,omitempty"`
	Dirty     bool   `json:"dirty,omitempty"`
	PatchURI  string `json:"patch_uri,omitempty"`  // blob://sha
	BundleURI string `json:"bundle_uri,omitempty"` // blob://sha (tar.gz of the tree)
}

type SubmitResponse struct {
	Experiment Experiment `json:"experiment"`
	Runs       []Run      `json:"runs"`
}

type BlobRef struct {
	URI    string `json:"uri"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

type Health struct {
	OK       bool   `json:"ok"`
	Version  string `json:"version"`
	DB       string `json:"db"`
	Secrets  string `json:"secrets"`
	AgentsOn int    `json:"agents_online"`
	// QueuePlugins reports each configured queue plugin's sidecar health.
	QueuePlugins map[string]bool `json:"queue_plugins,omitempty"`
}

// User is an identity from the SSO provider.
type User struct {
	ID        string     `json:"id"`
	Provider  string     `json:"provider"`
	Email     string     `json:"email,omitempty"`
	Phone     string     `json:"phone,omitempty"`
	Name      string     `json:"name,omitempty"`
	Role      string     `json:"role,omitempty"` // admin | member
	CreatedAt time.Time  `json:"created_at"`
	LastLogin *time.Time `json:"last_login,omitempty"`
}

// Display is the best human-readable label for a user.
func (u *User) Display() string {
	if u == nil {
		return ""
	}
	for _, s := range []string{u.Name, u.Email, u.Phone, u.ID} {
		if s != "" {
			return s
		}
	}
	return ""
}

// AuthConfig is GET /auth/config (unauthenticated): what the login page needs.
type AuthConfig struct {
	SSOEnabled bool   `json:"sso_enabled"`
	Provider   string `json:"provider,omitempty"`  // "conductor"
	LoginURL   string `json:"login_url,omitempty"` // /api/v1/auth/login
	TokenLogin bool   `json:"token_login"`         // the API token still works
}

// AuthStatus is GET /auth/me.
type AuthStatus struct {
	Authenticated bool   `json:"authenticated"`
	Mode          string `json:"mode,omitempty"` // sso | token
	User          *User  `json:"user,omitempty"`
}
