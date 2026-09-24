package v1

import (
	"encoding/json"
	"time"
)

// Agent <-> server protocol. The agent dials the server (reverse connection)
// at GET /api/v1/agent/connect with
//
//	Authorization: Bearer <agent token>
//	X-Mldojo-Node: <node id>
//
// and upgrades to a WebSocket. Every message on the socket is an AgentMsg.
// Requests carry an ID; the peer answers with a MsgReply carrying the same ID.

const AgentProtocolVersion = 1

// Message types.
const (
	// agent -> server
	MsgHello     = "hello"
	MsgHeartbeat = "heartbeat"
	MsgRunStatus = "run_status"
	MsgLog       = "log"
	MsgMetrics   = "metrics"
	MsgArtifacts = "artifacts"
	MsgEpisodes  = "episodes"

	// server -> agent
	MsgWelcome   = "welcome"
	MsgSpawn     = "spawn"
	MsgCancel    = "cancel"
	MsgSendFile  = "send_file"  // agent PUTs a file/dir tarball to a server URL
	MsgFetchTar  = "fetch_tar"  // agent GETs a tarball from a server URL and extracts it
	MsgExec      = "exec"       // run a short shell command, reply with output
	MsgListFiles = "list_files" // glob files under a run workdir
	MsgDiskUsage = "disk_usage" // how much each run's workdir is holding
	MsgPurgeRun  = "purge_run"  // delete a run's workdir on the node
	MsgShutdown  = "shutdown"

	// both directions
	MsgReply = "reply"
	MsgPing  = "ping"
)

type AgentMsg struct {
	Type    string          `json:"type"`
	ID      string          `json:"id,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

type Reply struct {
	OK    bool            `json:"ok"`
	Error string          `json:"error,omitempty"`
	Data  json.RawMessage `json:"data,omitempty"`
}

type Hello struct {
	NodeID          string          `json:"node_id"`
	Version         string          `json:"version"`
	ProtocolVersion int             `json:"protocol_version"`
	Capacity        Capacity        `json:"capacity"`
	Runs            []AgentRunState `json:"runs"`
	WorkdirRoot     string          `json:"workdir_root"`
	DatasetsRoot    string          `json:"datasets_cache_root"`
}

type AgentRunState struct {
	RunID string `json:"run_id"`
	Phase string `json:"phase"`
}

// Welcome is the server's answer to Hello: where to resume log shipping.
type Welcome struct {
	ServerVersion string                      `json:"server_version"`
	LogOffsets    map[string]map[string]int64 `json:"log_offsets"` // run_id -> stream -> bytes stored
	CancelRuns    []string                    `json:"cancel_runs"` // runs cancelled while agent was away
	HeartbeatSec  int                         `json:"heartbeat_sec"`
}

type Heartbeat struct {
	TS   time.Time `json:"ts"`
	GPUs []GPUStat `json:"gpus"`
	// Disks is sampled every heartbeat. Capacity.DiskGB is only sent with
	// hello, so it is a snapshot from whenever the agent last reconnected --
	// useless for deciding whether a checkpoint will fit.
	Disks []DiskStat `json:"disks,omitempty"`
}

type RunStatusMsg struct {
	RunID      string     `json:"run_id"`
	Phase      string     `json:"phase"`
	ExitCode   *int       `json:"exit_code,omitempty"`
	Message    string     `json:"message,omitempty"`
	PID        int        `json:"pid,omitempty"`
	Workdir    string     `json:"workdir,omitempty"`
	Commit     string     `json:"commit,omitempty"`
	GPUs       []int      `json:"gpus,omitempty"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	// EnvLock is the resolved dependency list (pip freeze / conda list) and
	// ImageDigest the immutable form of a docker tag. Recording only the
	// spec's file name or tag is not enough to reproduce a run later.
	EnvLock     string `json:"env_lock,omitempty"`
	ImageDigest string `json:"image_digest,omitempty"`
}

type LogMsg struct {
	RunID  string `json:"run_id"`
	Stream string `json:"stream"`
	Offset int64  `json:"offset"` // byte offset of Data within the stream
	Data   []byte `json:"data"`
}

type MetricsMsg struct {
	RunID  string        `json:"run_id"`
	Points []MetricPoint `json:"points"`
}

type ArtifactsMsg struct {
	RunID string         `json:"run_id"`
	Items []ArtifactItem `json:"items"`
}

// EpisodesMsg carries a run's evaluation results. Unlike metrics it is the
// whole list every time: an evaluation has tens of episodes, not millions of
// points, and a harness that rewrites its manifest at the end must not be
// missed.
type EpisodesMsg struct {
	RunID    string    `json:"run_id"`
	Episodes []Episode `json:"episodes"`
}

// DiskUsage reports what MLDojo is holding on a node. A cluster fills up
// one abandoned run directory at a time, and until now nothing could see it
// from the outside.
type DiskUsage struct {
	Root    string      `json:"root"`
	Entries []DiskEntry `json:"entries"`
	// Truncated is set when the walk hit its file budget, so a total is
	// reported as a floor rather than quietly being wrong.
	Truncated bool `json:"truncated,omitempty"`
}

type DiskEntry struct {
	RunID    string    `json:"run_id"`
	Path     string    `json:"path"`
	Bytes    int64     `json:"bytes"`
	Files    int       `json:"files"`
	Modified time.Time `json:"modified"`
}

type PurgeRunRequest struct {
	RunID string `json:"run_id"`
}

type ArtifactItem struct {
	Kind      string `json:"kind"`
	Path      string `json:"path"` // absolute path on the node
	SizeBytes int64  `json:"size_bytes"`
	// SHA256 is empty for files above the agent's hashing limit. Without it
	// there is no integrity check and no way to tell two checkpoints apart.
	SHA256 string `json:"sha256,omitempty"`
}

// SpawnRequest asks the agent to start a run.
type SpawnRequest struct {
	RunID      string            `json:"run_id"`
	Project    string            `json:"project"`
	Experiment string            `json:"experiment"`
	Cmd        string            `json:"cmd"`
	Workdir    string            `json:"workdir"` // relative to the code root
	Env        EnvSpec           `json:"env"`
	Vars       map[string]string `json:"vars"` // extra environment variables
	GPUs       int               `json:"gpus"`
	Code       SpawnCode         `json:"code"`
	Datasets   []SpawnDataset    `json:"datasets"`
	Outputs    *Outputs          `json:"outputs,omitempty"`
	WandbShim  bool              `json:"wandb_shim"`
	// WallTimeMin stops the run after this many minutes. 0 means no limit,
	// which is what node runs used to get unconditionally.
	WallTimeMin int `json:"wall_time_min,omitempty"`
}

type SpawnCode struct {
	Source    string `json:"source"`               // bundle | git
	BundleURL string `json:"bundle_url,omitempty"` // server URL to GET (agent token)
	Repo      string `json:"repo,omitempty"`
	Ref       string `json:"ref,omitempty"`
	Commit    string `json:"commit,omitempty"`
	PatchURL  string `json:"patch_url,omitempty"`
}

type SpawnDataset struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Mount   string `json:"mount"`
	Path    string `json:"path"` // resolved path on this node
}

type CancelRequest struct {
	RunID string `json:"run_id"`
}

type SendFileRequest struct {
	Path string `json:"path"`
	URL  string `json:"url"` // PUT target (agent token)
	Tar  bool   `json:"tar"` // stream a tar of the directory instead of a file
}

type FetchTarRequest struct {
	URL  string `json:"url"`  // GET source (agent token)
	Dest string `json:"dest"` // directory to extract into
	// Marker is created inside Dest after a successful extraction.
	Marker string `json:"marker,omitempty"`
}

type ExecRequest struct {
	Cmd        string `json:"cmd"`
	TimeoutSec int    `json:"timeout_sec"`
}

type ExecResult struct {
	ExitCode int    `json:"exit_code"`
	Output   string `json:"output"`
}

type ListFilesRequest struct {
	RunID string   `json:"run_id"`
	Globs []string `json:"globs"`
}
