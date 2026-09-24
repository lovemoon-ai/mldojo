English | [简体中文](queue-plugins.zh-CN.md)

# Queue plugins

Besides SSH nodes, MLDojo can submit runs to external job queues (a cluster scheduler, a cloud batch service,
an in-house training platform). Each such backend is a **queue plugin**: a small HTTP "sidecar" process that
implements the protocol below. The API server calls it to submit jobs, poll their status and logs, cancel them,
and read their metrics and output files. MLDojo itself ships no cluster-specific plugin; it ships a reference
**mock** plugin that runs jobs as local subprocesses.

## Concepts

| Thing | Form | Example |
|---|---|---|
| Plugin name | `<plugin>` | `mock` |
| Queue id | `<plugin>/<queue>` | `mock/default` |
| Run target | `queue:<plugin>/<queue>` | `queue:mock/default` |
| Artifact URI (job output) | `<plugin>://<job>/<path>` | `mock://j-1a2b/outputs/model.ckpt` |
| Artifact URI (bucket) | `bucket://<bucket>/<path>` | `bucket://my-bucket/runs/x/model.ckpt` |

## Configure

Tell the API where each plugin listens, in `config.yaml`:

```yaml
api:
  queue_plugins:
    mock: http://127.0.0.1:8766
  queue_poll_sec: 5          # how often queue runs are polled (default 5; or MLDOJO_QUEUE_POLL_SEC)
```

or with an environment variable (comma-separated `name=url` pairs):

```bash
MLDOJO_QUEUE_PLUGINS="mock=http://127.0.0.1:8766,other=http://127.0.0.1:8767"
```

`GET /api/v1/health` (and `mldojo health`) reports each plugin's reachability as
`"queue_plugins": {"<name>": true|false}`. With no plugins configured it is `{}`, which is normal.

Then register queues and submit to them:

```bash
mldojo queue add --id mock/default --backend mock
mldojo queue add --id <plugin>/<queue> --backend <plugin> \
    --credentials secret://<plugin>/default --defaults-file q.yaml
mldojo run submit -f recipes/examples/hello/recipe.yaml --target queue:mock/default --wait
```

Queue runs' logs are **polled** (near-realtime), not streamed: every `queue_poll_sec` the server fetches the
status and a full log snapshot and appends only the new part.

A recipe can override its env for queue targets, e.g. to use a docker image there:

```yaml
env:
  default: {type: venv, spec: requirements.txt}
  overrides:
    - when: {backend_kind: queue, backend: mock}
      use: {type: docker, image: registry.example.com/my-team/train:latest}
```

### Deploying a plugin

- **Native**: `deploy/native/install.sh` runs `<dir>/install.sh` for each directory listed in
  `MLDOJO_PLUGIN_DIRS` (set it in `deploy/site.env`). A plugin kept in its own (possibly private) repo ships an
  `install.sh` that installs the sidecar, e.g. as a `systemd --user` unit, and adds itself to
  `MLDOJO_QUEUE_PLUGINS`.
- **docker compose**: compose does not include any plugin. Run the plugin separately and set
  `MLDOJO_QUEUE_PLUGINS` on the api container.

## Try it locally with the mock plugin

```bash
python3 adapters/queue_sidecar/mock/server.py --host 127.0.0.1 --port 8766
# in another shell, start the API with:
MLDOJO_QUEUE_PLUGINS=mock=http://127.0.0.1:8766
mldojo queue add --id mock/default --backend mock
mldojo run submit -f recipes/examples/hello/recipe.yaml --target queue:mock/default --wait
```

The mock plugin needs only the Python standard library. It keeps job state under `--state-dir` (default
`$TMPDIR/mldojo-queue-sidecar`): `<state-dir>/jobs/<job_id>/{code,output.log}`. A job is `queued` for about 1 s,
then `running`, then `succeeded` or `failed` by exit code. `cancel` sends SIGTERM to the process group (SIGKILL
after 5 s). It is also the best starting point for writing a real plugin.

## Protocol

All bodies are JSON. Errors are returned as `{"error": "..."}` with a 4xx/5xx status.

**Credentials.** If the queue was registered with `--credentials secret://...`, the API resolves the secret and
sends it on every request as `X-Queue-Credentials: base64(credentials)` (on `POST /jobs` also in the
`credentials` field). What the credentials contain (a token, a config file, ...) is up to the plugin. Plugins
should redact tokens and passwords from logs and error messages.

| Method / path | Body | Response |
|---|---|---|
| `GET /health` | | `{"ok": true, "mock": bool, "sdk": "<version>"\|null, "error"?}` (`error` describes a configuration problem) |
| `POST /jobs` | see below | `{"job_id", "workspace_folder", "url", "dag_id": null, "raw": {...}}` |
| `GET /jobs/{id}/status` | | `{"job_id", "phase", "raw_phase", "message", "started_at", "finished_at", "exit_code"}` |
| `GET /jobs/{id}/log` | | `{"job_id", "log": "<full snapshot>"}` |
| `POST /jobs/{id}/cancel` | `{}` | `{"ok": true}` (must be idempotent) |
| `POST /jobs/{id}/metrics` | `{"bucket", "paths": [{"type": "jsonl"\|"tensorboard", "path"}], "tracking"}` | `{"points": [{"step", "key", "value", "ts"}], "warnings"?}` |
| `POST /jobs/{id}/files` | `{"bucket", "globs": [{"kind", "glob"}], "max_files": 2000}` | `{"files": [{"kind", "path", "size"}], "warnings"?}` |
| `GET /jobs/{id}/download?bucket=&path=` | | raw bytes, `application/octet-stream`, `Content-Length` when known |

General rules:
- Timestamps are RFC3339 or `null`.
- Unknown job ids return 404.
- Failures talking to the underlying scheduler should return 502; a plugin that is not configured properly
  should return 503.
- `POST /jobs` must return a non-empty `job_id`.

### `POST /jobs`

Body fields: `run_id, job_name, queue_name, project_id, docker_image, num_workers, gpu_per_worker,
cpu_per_worker, cpu_mem_ratio, wall_time_min, cmd, workdir, bundle_url, bundle_token, env, input_bucket,
output_bucket, mounts[{bucket,path,mount}], job_password, credentials, extra`.

- `queue_name` is the `<queue>` part of the queue id. `project_id`, `job_password` (resolved from
  `--job-password secret://...`) and `extra` come from the queue definition; `extra` is an opaque object
  (queue `defaults.extra`) the plugin may merge into its native job spec.
- Resources come from the recipe, falling back to queue `defaults` (1 worker, 0 GPU, 4 CPU, ratio 4, 60 min).
- The code bundle is a tar.gz at `bundle_url`; fetch it with `Authorization: Bearer <bundle_token>` and extract
  it with path-safety checks. `cmd` should run from `workdir` inside the extracted code.
- `env` must be exported to the job. It includes `MLDOJO_RUN_ID`, `MLDOJO_RUN_TOKEN`, `MLDOJO_API_URL`,
  `MLDOJO_PROJECT`, `MLDOJO_EXPERIMENT` and `MLDOJO_PARAM_*`.
- `input_bucket` / `output_bucket` accept `"a,b"` or a list. `mounts` are datasets with a bucket location; a
  plugin that can only mount whole buckets can symlink `path` to `mount` in its job script.
- `url` (optional) links to the job's page in the scheduler UI; it is shown on the run page.

### Status phases

`phase` must be one of the MLDojo phases: `queued`, `starting`, `running`, `succeeded`, `failed`, `cancelled`.
Put the scheduler's own state in `raw_phase` and any reason in `message`. Set `exit_code` when known
(0 for success; 128+N for death by signal N is a good convention).

### Logs

`GET /jobs/{id}/log` returns the **whole** log so far (return `""` before the job starts). The API diffs
successive snapshots, so the log must only grow.

### Metrics

`paths` are relative to the job's output location (`bucket` `""`), or to a named bucket (queue
`defaults.metrics_bucket`). Return 501 if the plugin can't read metrics at all; the API then stops asking for
that run.

jsonl parsing (one JSON object per line):
- The step comes from `step`, else `_step`, else the line index.
- Every other finite numeric top-level field becomes a key.
- The timestamp comes from `ts`, `timestamp` or `_timestamp` (epoch s/ms or ISO), else the file mtime.
- Unparseable lines, such as a half-written last line, are skipped.

tensorboard: a single event file, or a directory scanned for `*tfevents*` (`simple_value` and scalar
float/double tensors).

A missing file is a `warnings` entry, not an error: metric files often appear late in a run.

### Artifacts: `/files` and `/download`

Paths are relative to the job's output location (`bucket` `""`) or to a named bucket (queue
`defaults.artifacts_bucket`). Artifacts are only listed when the run ends; files are fetched on demand when a
user previews or downloads them.

Globs:
- `*` and `?` match within one path segment; `**` matches any number of directories
  (`outputs/**/*.ckpt` matches `outputs/a.ckpt` and `outputs/x/y/a.ckpt`); `{a,b}` alternates and may be nested.
- Globs are matched against the whole relative path.
- A file is assigned to the first glob that matches it, so a catch-all like `outputs/**` belongs last.
- Listing should be bounded (the mock stops at 16 levels, 20000 visited entries, or `max_files`, capped at
  10000). Hitting a limit, or a prefix that doesn't exist, adds a `warnings` entry; it is not an error.

Download returns 404 if the file is missing and 400 for a bad or escaping path or a directory. Stream large
files rather than buffering them.

Note: on many schedulers the job's code directory is not readable after the job ends. Training code should write
checkpoints and metrics to the output location the plugin reads, and recipe outputs globs should be relative to it.
