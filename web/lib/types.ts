// Mirrors proto/mldojo/v1/api.go (JSON field names are authoritative).
// Conventions:
//   time.Time        -> string (RFC 3339)
//   *T               -> T | null
//   `omitempty`      -> optional field
//   slices/maps without omitempty may be encoded as null by Go -> `| null`
//   json.RawMessage  -> JsonValue

export type JsonValue =
  | string
  | number
  | boolean
  | null
  | JsonValue[]
  | { [key: string]: JsonValue };

export const PHASES = ["queued", "starting", "running", "succeeded", "failed", "cancelled"] as const;
export type Phase = (typeof PHASES)[number];

export const TERMINAL_PHASES: readonly string[] = ["succeeded", "failed", "cancelled"];
export function isTerminal(phase: string | undefined | null): boolean {
  return !!phase && TERMINAL_PHASES.includes(phase);
}

export type LogStream = "stdout" | "stderr" | "system";

export interface ApiErrorBody {
  error: string;
  code: string; // user_error | unreachable | backend_error | conflict | not_found | unauthorized | internal
}

export interface Project {
  id: string;
  name: string;
  description: string;
  owner: string;
  created_at: string;
  updated_at: string;
  experiment_count: number;
  run_count: number;
}

export interface Experiment {
  id: string;
  project_id: string;
  project: string;
  name: string;
  description: string;
  recipe_yaml: string;
  tags: string[] | null;
  created_at: string;
  updated_at: string;
  run_counts: Record<string, number> | null; // by status
}

/** One evaluation trial. For a policy this is the unit of result: a success
 *  rate is a summary of these, and the seed is what makes two evaluations
 *  comparable trial by trial rather than only rate to rate. */
export interface Episode {
  index: number;
  seed?: number;
  success: boolean;
  steps?: number;
  duration_ms?: number;
  video_uri?: string;
  extra?: Record<string, JsonValue>;
}

export interface EpisodeSummary {
  total: number;
  successes: number;
  success_rate: number;
  with_video: number;
}

export interface EpisodesResponse {
  run_id: string;
  summary: EpisodeSummary;
  episodes: Episode[];
}

export interface Run {
  id: string;
  experiment_id: string;
  project: string;
  experiment: string;
  name: string; // e.g. "seed=0"
  target: string;
  backend_kind: string;
  backend_id: string;
  backend_handle: JsonValue;
  resources: JsonValue; // usually Resources
  env: JsonValue; // usually EnvSpec
  code_commit: string;
  code_patch_uri: string;
  status: string;
  exit_code: number | null;
  started_at: string | null;
  finished_at: string | null;
  created_at: string;
  metadata: RunMetadata;
}

export interface RunMetadata {
  tags?: string[];
  params?: Record<string, JsonValue>;
  cmd?: string;
  workdir?: string;
  notes?: string;
  message?: string;
  code_source?: string; // git | local | inline-patch
  code_repo?: string;
  code_ref?: string;
  code_bundle_uri?: string;
  code_dirty?: boolean;
  env_lock?: string; // pip freeze / conda list, captured at run time
  image_digest?: string; // the digest behind a mutable docker tag
  datasets?: RunDataset[];
  models?: RunModel[]; // registered versions this run read
  outputs?: Outputs | null;
  extra_env?: Record<string, string>;
  log_mode?: string; // "realtime" | "near-realtime"
  external_url?: string;
  submitter?: string;
  summary?: string;
}

export interface RunDataset {
  name: string;
  version: string;
  mount: string;
}

export interface RunModel {
  name: string; // project/name
  version: number;
  uri: string;
  as?: string; // the parameter it filled
}

export interface Outputs {
  logs?: string;
  checkpoints?: string;
  videos?: string;
  images?: string;
  metrics?: MetricsSource[];
  episodes?: EpisodesSource[];
  model?: string; // registry entry a successful run registers into
}

export interface EpisodesSource {
  type?: string; // csv | jsonl | filenames
  path: string;
}

export interface MetricsSource {
  type: string; // tensorboard | jsonl
  path: string;
}

export interface Resources {
  gpus?: number;
  gpu_type?: string;
  min_mem_gb?: number;
  workers?: number;
  gpu_per_worker?: number;
  cpu_per_worker?: number;
  cpu_mem_ratio?: number;
  wall_time_min?: number;
}

export interface EnvSpec {
  type: string; // docker | conda | venv | none
  spec?: string;
  image?: string;
  vars?: Record<string, string>;
}

export interface Node {
  id: string;
  display_name: string;
  labels: string[] | null;
  connection: NodeConnection;
  proxy: Proxy | null;
  capacity: Capacity | null;
  agent_status: string; // online | offline | degraded
  agent_version: string;
  last_heartbeat: string | null;
  workdir_root: string;
  datasets_cache_root: string;
  created_at: string;
  updated_at: string;
  gpu_stats?: GPUStat[];
  active_runs: number;
}

export interface NodeConnection {
  type: string; // ssh | local
  host?: string;
  port?: number;
  user?: string;
  identity?: string; // secret://ssh_keys/x or a path
  password?: string; // secret://passwords/x
  via?: Via[];
  extra_ssh_options?: string[];
  agent_server_url?: string;
  reverse_tunnel?: boolean;
}

export interface Via {
  node: string; // node id, or "local"
}

export interface Proxy {
  http?: string;
  https?: string;
  no_proxy?: string[];
}

export interface Capacity {
  gpus: GPUInfo[] | null;
  cpu: number;
  mem_gb: number;
  disk_gb: number;
  hostname?: string;
  os?: string;
  arch?: string;
  mldojo_dir?: string; // ~/.mldojo resolved: where runs, datasets and envs live
}

export interface GPUInfo {
  index: number;
  model: string;
  mem_gb: number;
}

export interface GPUStat {
  index: number;
  model: string;
  util: number; // percent
  mem_used_mb: number; // MiB
  mem_total_mb: number;
  temp: number; // celsius
  run_ids?: string[];
}

export interface Queue {
  id: string;
  backend: string;
  display_name: string;
  labels: string[] | null;
  client: QueueClient;
  defaults: Record<string, JsonValue> | null;
  capacity_hint: Record<string, JsonValue> | null;
  proxy: Proxy | null;
  created_at: string;
  updated_at: string;
  active_runs: number;
}

/** Live scheduler capacity from GET /queues/resources; total/used/free count `unit`. */
export interface QueueResource {
  plugin: string;
  queue_id?: string;
  name: string;
  cluster: string;
  accelerator: string;
  unit: "gpu" | "cpu";
  total: number;
  used: number;
  free: number;
  running_jobs: number;
  queued_jobs: number;
  queued_wait_sec: number;
  usable: boolean;
  utilization: number | null;
}

export interface QueueResources {
  queues: QueueResource[];
  errors?: Record<string, string>;
  warnings?: string[];
}

export interface QueueClient {
  sdk: string;
  credentials?: string;
  project_id?: string;
  job_password?: string;
}

export interface Dataset {
  id: string;
  name: string;
  version: string;
  mount: string;
  locations: DatasetLocation[] | null;
  created_at: string;
}

export interface DatasetLocation {
  id?: string;
  kind: string; // node_path | bucket
  node?: string;
  path?: string;
  provider?: string;
  bucket?: string;
  credentials?: string;
  authoritative: boolean;
}

export interface SecretMeta {
  namespace: string;
  name: string;
  ref: string; // secret://ns/name
  description?: string;
  size: number;
  created_at: string;
  updated_at: string;
}

export interface SecretStatus {
  unlocked: boolean;
  source?: string; // keychain | env | file | api
  recipient?: string;
}

export interface Artifact {
  id: string;
  run_id: string;
  kind: string; // log | ckpt | video | image | other
  uri: string;
  size_bytes: number;
  sha256?: string;
  created_at: string;
}

export interface MetricPoint {
  step: number;
  key: string;
  value: number;
  ts: string;
}

export interface RunEvent {
  id: number;
  run_id: string;
  ts: string;
  kind: string;
  payload: JsonValue;
}

export type FrameKind = "log" | "metric" | "gpu" | "status" | "eof" | "error";

export interface Frame<P = JsonValue> {
  seq: number;
  ts: string;
  kind: FrameKind | string;
  payload: P;
}

export interface LogPayload {
  stream: string;
  offset: number;
  data: string;
}

export interface SubmitRequest {
  recipe_yaml: string;
  target: string;
  gpus?: number;
  matrix?: string[];
  params?: Record<string, JsonValue>;
  project?: string;
  experiment?: string;
  code?: CodeInfo | null;
  notes?: string;
  dry_run?: boolean;
}

export interface CodeInfo {
  source: string; // git | local | inline-patch
  repo?: string;
  ref?: string;
  commit?: string;
  dirty?: boolean;
  patch_uri?: string;
  bundle_uri?: string;
}

export interface SubmitResponse {
  experiment: Experiment;
  runs: Run[] | null;
}

export interface BlobRef {
  uri: string;
  sha256: string;
  size: number;
}

export interface Health {
  ok: boolean;
  version: string;
  db: string;
  secrets: string;
  agents_online: number;
  /** Queue plugin name -> ready. */
  queue_plugins?: Record<string, boolean>;
}

export interface User {
  id: string;
  provider: string;
  email?: string;
  phone?: string;
  name?: string;
  created_at: string;
  last_login?: string;
}

/** GET /auth/config (unauthenticated): what the login page can offer. */
export interface AuthConfig {
  sso_enabled: boolean;
  provider?: string; // "conductor"
  login_url?: string; // /api/v1/auth/login
  token_login: boolean;
}

/** GET /auth/me — `user` is only present in sso mode. */
export interface AuthStatus {
  authenticated: boolean;
  mode?: string; // sso | token
  user?: User;
}

// ---- Response shapes defined only in docs/api.md -------------------------

export interface OkResponse {
  ok: boolean;
}

export interface MetricsResponse {
  keys: string[] | null;
  points: MetricPoint[] | null;
}

/** GET /runs/{id}/code */
export interface RunCode {
  source: string;
  repo: string;
  ref: string;
  commit: string;
  dirty: boolean;
  patch: string; // dirty-worktree diff text
  bundle_uri: string;
}

export interface CompareMetric {
  key: string;
  a: number | null;
  b: number | null;
  delta: number | null;
}

export interface CompareEnv {
  path: string;
  a: JsonValue;
  b: JsonValue;
}

/** GET /compare?a=&b= — `code` is unspecified in api.md, rendered generically. */
export interface CompareResponse {
  a: Run;
  b: Run;
  code: Record<string, JsonValue> | null;
  metrics: CompareMetric[] | null;
  env: CompareEnv[] | null;
}

/** POST /nodes/{id}/test */
export interface NodeTestResult {
  ok: boolean;
  reachable: boolean;
  agent_online: boolean;
  latency_ms: number;
  via: string;
  message: string;
}

/** POST /secrets/{ns}/{name} */
export interface SecretSetRequest {
  value_b64: string;
  description?: string;
}

/** GET /compare/many — several runs with their params, final metrics and curves. */
export type CompareRun = {
  id: string;
  name: string;
  project: string;
  experiment: string;
  status: string;
  target: string;
  params?: Record<string, JsonValue>;
  latest?: Record<string, number>;
  series?: Record<string, { step: number; value: number }[]>;
};

export type CompareManyResponse = {
  runs: CompareRun[];
  metric_keys: string[];
  param_keys: string[];
  sampled: boolean;
};

/** GET /ai/experiments/{p}/{e}/summary — every run's params against one metric. */
export interface ExpSummary {
  project: string;
  exp: string;
  runs: number;
  by_status: Record<string, number>;
  metric: string;
  lower_is_better: boolean;
  ranking: {
    id: string;
    name: string;
    status: string;
    value: number | null;
    step: number;
    params?: Record<string, JsonValue>;
  }[];
  next_steps: string[];
}
