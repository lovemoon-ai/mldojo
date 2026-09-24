English | [简体中文](architecture.zh-CN.md)

# Architecture (v1 implementation)

This document explains the key mechanisms in the implementation.

## Processes

| Process | Where | Notes |
|---|---|---|
| `mldojo-api` | API host | Go HTTP server. Owns the control plane (PostgreSQL) and the data plane (log files and blobs under `data_dir`), hosts agent connections and serves the Web static files |
| `mldojo-agent` | Every node | Connects back to the API over a reverse WebSocket; runs jobs, collects GPU stats, ships logs/metrics/artifacts |
| Queue plugins (optional) | Usually the API host | HTTP sidecars implementing the queue-plugin protocol; the API calls them at the URLs in `api.queue_plugins`. See [queue-plugins.md](queue-plugins.md) |
| PostgreSQL 16 | API host | docker or `deploy/native/install-postgres.sh` |

## Agent connection

- Transport: `GET /api/v1/agent/connect` upgraded to a WebSocket, with `Authorization: Bearer <agent token>` and `X-Mldojo-Node`.
  The message format is in `proto/mldojo/v1/agent.go`.
- Registration: on `node add` the server generates a random token and stores only its sha256. The node record is
  written only after the agent's first hello; on failure nothing is kept.
- Reconnect: exponential backoff with jitter (1s→30s). After reconnecting, the server tells the agent in `welcome`
  how many bytes of each stream of each run it has already stored, and the agent resumes from there, so logs are
  neither duplicated nor lost. Metrics are fully rescanned after a reconnect and the server upserts by
  `(run, key, step)`, which keeps it idempotent.
- Keepalive: the server pings every 20s and disconnects after 60s without any message.
- When a node cannot reach the server (e.g. behind a bastion), use `--reverse-tunnel`: the server keeps an SSH
  connection open and sets up a `-R` forward on the node's `127.0.0.1:<23000+hash(id)>`; the agent dials back
  through that port. The port is fixed, and the tunnel is rebuilt automatically after an API restart.

## SSH

- Uses `golang.org/x/crypto/ssh`, not the system ssh. `via` entries are tried in order and the first that works is
  used; `local` means a direct connection. `via` can nest recursively (up to 4 levels).
- The username is passed through as is, so @-nested forms like `a@b@10.x@host` work (the CLI splits off the host
  at the last `@`).
- Auth: `identity` (secret:// or a path), `password` (secret://, also handles keyboard-interactive). When neither is
  set, ssh-agent and then `~/.ssh/id_{ed25519,rsa,ecdsa}` are tried.
- Host keys: TOFU (trust on first use), recorded in `~/.mldojo/known_hosts`.
- Deploy: uploads the binary (skipped if the sha256 matches) and writes the config. With linger it uses
  `systemd --user` (`KillMode=process`, so restarting the agent doesn't kill training processes); otherwise
  `nohup setsid`.
- **Where `~/.mldojo` lives**: run workdirs (including checkpoints), dataset caches and venv/conda caches all live
  under it, and leaving it in home fills up the home disk. A common case is a nearly full system disk with large,
  empty data disks mounted next to it. So when a node does **not yet** have `~/.mldojo`, deploy picks the data
  disk with the most free space, creates `mldojo/` under the user's own directory there (whichever of
  `<disk>/<user>`, `<disk>/users/<user>`, `<disk>/home/<user>` exists and belongs to the user; if none does and
  the disk is writable, it creates `<disk>/<user>`), and symlinks `~/.mldojo` to it. If home is already on the disk
  with the most free space (e.g. `/home` is itself a large local disk), nothing is done. Excluded from the choice:
  FUSE (s3fs reports 16 EB free, JuiceFS is a cluster-wide shared bucket), pseudo filesystems such as
  overlay/tmpfs, bind mounts of single files (`/etc/hosts` in containers), `/boot`, `/run` and similar.
  **Nodes that already have `~/.mldojo` are left alone**: deploy also runs on `node upgrade`, while running jobs
  are writing into it. To keep it in home, `mkdir ~/.mldojo` before deploying. The decision logic is tested
  against probe output from real nodes (`testdata/real/datahome/`). `node add --dry-run` shows where it would go,
  and `mldojo dir` in `node show` shows where it actually is.
- `node add --dry-run`: probes connectivity and node info only; deploys nothing.

## Run lifecycle

```
queued ──dispatch──▶ starting ──agent fetches code / prepares env──▶ running ──▶ succeeded | failed | cancelled
  ▲ stays queued while the agent is offline; dispatched once the agent says hello
```

- Code: the CLI packs a tar.gz; for git repos it also records the commit and a patch of the dirty worktree
  (including untracked files), uploaded as `blob://sha256`. The agent downloads it through `/agent/blobs/{sha}`.
  Two other modes: `git` (the agent clones repo@ref itself) and `inline-patch` (clone, then apply a patch).
- Execution: code goes into `<workdir_root>/<run>/code`; `_mldojo/` holds `run.sh`, `stdout.log`, `stderr.log`,
  `exit_code` and `state.json`. The process is started in its own session with `setsid`, and a wrapper writes the
  exit code to a file, so a restarted agent can take the run over again.
- env: `none` / `venv` (cached in `~/.mldojo/envs` by the hash of the requirements file) / `conda`
  (environment.yaml hash or an existing env name) / `docker` (`--gpus device=…`, datasets mounted read-only).
- GPUs: there is no scheduler, but there is admission control. At submit time the node's static specs are checked
  against `gpu_type`, `min_mem_gb` and the GPU count, and anything that can never fit is rejected (exit 2).
  **At dispatch time** it is checked again, under the node lock, against the **actual free memory** reported by
  heartbeats; if it doesn't fit, the run stays queued and `message` explains why. (Previously `min_mem_gb` was only
  compared to the card's total memory, so a 97 GB card with 90 GB taken by someone else still passed, and the job
  OOMed right away.) Runs that have been admitted but whose process hasn't started yet also count as occupying
  their GPUs; otherwise telemetry would still show the card as free and two runs dispatched back to back would
  both think they fit. Nodes that can't report GPU stats (no nvidia-smi) skip this check. The agent picks the
  actual cards: those not used by an mldojo run and with the least memory in use come first, and it sets
  `CUDA_VISIBLE_DEVICES`.
- Logs: the agent tails stdout/stderr every 300ms and pushes them to the server; the `system` stream records
  platform events. WS frames have the form `{seq, ts, kind, payload}`.
- Metrics: file scanning (jsonl and tensorboard event files, incremental, tolerant of partial records), plus
  `_mldojo/sdk_metrics.jsonl` written by the SDK, plus HTTP ingest (`MLDOJO_RUN_TOKEN`). NaN/Inf are encoded as
  strings in JSON.
- Artifacts: the outputs globs are scanned every 30s and once more at the end, with URIs
  `node://<node>/<abs path>`. On download the agent first PUTs the file into the server cache, which then serves
  it to the browser with Range support (so video preview works).

## Queue plugins

Queue backends plug in as HTTP sidecars implementing the [queue-plugin protocol](queue-plugins.md); a plugin
translates a recipe into a job on its cluster. Targets are written `queue:<plugin>/<queue>`. The server polls the
status and a full log snapshot every `api.queue_poll_sec` seconds (default 5), finds the new part using
`length + prefix sha256`, appends it, and pushes only the delta to the UI (the badge shows "near-realtime").
Metrics are read through the plugin from jsonl/tensorboard files in the job output.
Artifacts: at the end, the plugin lists files matching the outputs globs and only the URIs
(`<plugin>://<job>/<path>`) are recorded; nothing is copied. On preview or download the file is fetched through
the plugin into the server cache.
Note: on many clusters the code directory is no longer readable after the job ends, so training code should write
artifacts and metrics to an output location the plugin can read, and the outputs paths in the recipe should be
relative to it.

## Datasets

Datasets must be registered at submit time. Before dispatching to a node they are resolved in this order:
① a node_path registered on that node;
② the node cache `datasets_cache_root/<name>/<version>` (only counts if the `.mldojo_complete` marker exists);
③ sync from a node_path on another online node (authoritative first). Syncing goes through the server relay: the
source agent PUTs a tar stream and the target agent GETs and unpacks it. Both only need to reach the server, so
nodes behind a bastion work too. When done, the copy is registered as a new node_path.

Mounting: outside docker, a symlink `mount → path` is preferred, falling back to the environment variable
`MLDOJO_DATASET_<NAME>` without permission; docker uses `-v path:mount:ro`; queue plugins usually use bucket mounts.

## Secrets

Values are encrypted with age (X25519) and stored in the `secrets` table. The master key comes from, in order: the
OS keychain (macOS Keychain / Secret Service, 3s timeout) → `MLDOJO_MASTER_KEY` → `master_key_file`; it can also
be pushed with `mldojo secret unlock`. On first start a master key is generated automatically (stored in the
keychain when possible) and its recipient is recorded to guard against using the wrong key. While locked, it
retries automatically once a minute. No API ever returns plaintext.

## Other implementation details

- All API paths have the `/api/v1` prefix (Web and API share an origin).
- The runs table has a `name` column, the nodes table has an `agent_token_hash` column, and there is a `settings`
  table (token, master key recipient).
- The Web UI is a Next.js static export; pages are routed with query parameters (`/run?id=`).
- Dataset "mounts" on nodes are symlinks, falling back to environment variables when a link can't be created.
