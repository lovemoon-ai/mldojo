English | [简体中文](api.zh-CN.md)

# MLDojo API (v1)

All endpoints live under `/api/v1`. JSON in, JSON out. The Go types in
`proto/mldojo/v1/api.go` are the source of truth for every object shape.

## Auth

- Single token (`MLDOJO_TOKEN`): `Authorization: Bearer <token>`.
- WebSockets and raw artifact/blob URLs (browsers cannot set headers): `?token=<token>` query parameter
  (accepted on every endpoint).
- `POST /runs/{id}/metrics` also accepts the per-run token (`MLDOJO_RUN_TOKEN`).
- `GET /api/v1/health` is unauthenticated.
- `GET /api/v1/metrics` requires **admin** unless `api.metrics_public` is on (see [deploy.md](deploy.md)).


## Permissions

After authentication there is an authorization layer (migration `0003_authz.sql`):

| Principal | Role | Can do |
|---|---|---|
| API token | `admin` | Everything. The CLI, agents and scripts use it, so it stays admin |
| SSO user listed in `sso.admins` | `admin` | Everything |
| Other SSO users | `member` | Read everything; submit runs; **delete only the** projects / experiments / runs **they own** |

- **Admin only**: `POST/DELETE /nodes`, `/nodes/{id}/upgrade`, `POST/DELETE /queues`,
  `DELETE /datasets/{ref}`, writes under `/secrets/*`, `GET /audit`, `GET /users`.
  These either make the server open outbound SSH connections, or change credentials the server later uses.
- **Ownership**: projects use `owner`, runs use `metadata.submitter`; both are written at creation time from
  the authenticated identity (only an admin can set `owner` on someone else's behalf). **Legacy rows with no
  owner can be deleted by anyone**, otherwise old data would be stuck forever.
- Roles follow the config: `sso.admins` is synced into `users.role` on every login, so promoting someone means
  editing the config and logging in again, not a manual `UPDATE` in the database.

### Browser-side protections

- The session cookie is `SameSite=Lax`, and Lax is **site**-scoped: sibling sites under the same registrable
  domain (e.g. `*.example.com`) count as same-site. So **state-changing requests check `Origin`**:
  a cross-origin `POST/PUT/DELETE` carrying the cookie gets 403; requests with a Bearer token are unaffected
  (a cross-site page cannot read the token, so it cannot forge one). The WebSocket handshake is checked the same way.
- CORS only echoes configured origins (`public_url` / `web_url`); it no longer reflects arbitrary `Origin`s.
- `/auth/logout` only accepts `POST` (a GET could be triggered by an `<img>` on any page).
- Every `/api/` response carries `X-Request-Id`, matching the `request_id` in the server access log and audit records.

## Errors & exit codes

Non-2xx responses return `{"error": "...", "code": "..."}`.

| HTTP | code | CLI exit |
|---|---|---|
| 400/422 | `user_error` | 2 |
| 404 | `not_found` | 2 |
| 502/503/504 | `unreachable` | 3 |
| 502 | `backend_error` | 4 |
| 403 | `forbidden` |
| 409 | `conflict` | 5 |
| 401 | `unauthorized` | 2 |

## Resources

| Method | Path | Body | Returns |
|---|---|---|---|
| GET | `/health` | | `Health` |
| GET | `/metrics` | | Prometheus text format (`text/plain; version=0.0.4`) — **admin only** unless `api.metrics_public` |
| GET | `/projects` | | `[Project]` |
| POST | `/projects` | `{name, description}` | `Project` |
| GET | `/projects/{name}` | | `Project` |
| DELETE | `/projects/{name}?force=1` | | `{ok}` (409 if it has experiments and no force) |
| GET | `/projects/{p}/experiments` | | `[Experiment]` |
| POST | `/projects/{p}/experiments` | `{name, description, recipe_yaml, tags}` | `Experiment` |
| GET | `/projects/{p}/experiments/{e}` | | `Experiment` |
| DELETE | `/projects/{p}/experiments/{e}?force=1` | | `{ok}` |
| GET | `/runs?project=&experiment=&status=&target=&limit=` | | `[Run]` (newest first) |
| POST | `/runs` | `SubmitRequest` | `SubmitResponse` |
| GET | `/runs/{id}` | | `Run` (id may be a unique prefix) |
| POST | `/runs/{id}/cancel` | | `Run` |
| GET | `/audit?actor=&action=&target=&limit=200` | | `[AuditEntry]` — **admin only**. Who changed what, with the request id |
| GET | `/users` | | `[User]` — **admin only** |
| DELETE | `/runs/{id}?force=1` | | `{deleted}` — also removes the run's logs, metrics, artifact index and events. Refuses an active run unless `force=1`, which cancels it first |
| GET | `/runs/{id}/events` | | `[RunEvent]` |
| GET | `/runs/{id}/logs?stream=stdout&offset=0&limit=1048576&tail=0` | | `text/plain`; header `X-Log-Size` = total bytes. `tail=N` returns the last N bytes |
| WS | `/runs/{id}/logs/ws?stream=stdout\|stderr\|system\|all&offset=0&follow=1` | | `Frame`s (see below) |
| GET | `/runs/{id}/metrics?key=&since_step=0&max_points=2000` | | `{keys: [string], points: [MetricPoint], sampled: bool}`. `max_points` caps points **per key** (even-stride sampling, endpoints kept); `sampled` says whether any key was thinned |
| WS | `/runs/{id}/metrics/ws?since=step:0` | | `Frame`s kind `metric` |
| POST | `/runs/{id}/metrics` | `[MetricPoint]` or `{points:[...]}` | `{ok, count}` (SDK ingest; accepts the user token or the run token `MLDOJO_RUN_TOKEN`) |
| GET | `/runs/{id}/artifacts` | | `[Artifact]` |
| POST | `/runs/{id}/artifacts/refresh` | | `[Artifact]` (asks the agent to rescan) |
| GET | `/runs/{id}/artifacts/raw?uri=<uri>` | | file bytes (supports `Range`; used for video/image preview) |
| GET | `/runs/{id}/gpu` | | `[GPUStat]` of the GPUs assigned to the run |
| GET | `/runs/{id}/code` | | `{source, repo, ref, commit, dirty, patch, bundle_uri}` (`patch` is the dirty-worktree diff text) |
| GET | `/runs/{id}/models` | | `[ModelVersion]` this run produced |
| GET | `/models?project=` | | `[Model]` |
| GET | `/models/{p}/{name}` | | `{model, versions: [ModelVersion], used_by: [{version, run_id, project, experiment, name, status}]}` — `used_by` lists the runs that read this version |
| POST | `/models/{p}/{name}/versions` | `{run_id, uri, stage, notes}` | `ModelVersion` (201). `outputs.model` in a recipe does this automatically when the run succeeds |
| POST | `/models/{p}/{name}/versions/{v}/stage` | `{stage}` | `ModelVersion` (production/staging are unique; the previous holder becomes archived) |
| GET | `/compare?a=<run>&b=<run>` | | `{a: Run, b: Run, code: {...}, metrics: [{key, a, b, delta}], env: [{path, a, b}]}` |
| GET | `/nodes` | | `[Node]` |
| POST | `/nodes` | `NodeAddRequest` | `Node` (blocks until the agent dials back, ≤60s; failure = error, nothing stored). With `"dry_run": true`: `{ok, route, latency_ms, probe}` and nothing is deployed |
| GET | `/nodes/{id}` | | `Node` |
| DELETE | `/nodes/{id}?stop_agent=1` | | `{ok}` |
| POST | `/nodes/{id}/test` | | `{ok, reachable, agent_online, latency_ms, via, message}` |
| GET | `/nodes/{id}/gpu` | | `[GPUStat]` |
| WS | `/nodes/{id}/gpu/ws` | | `Frame`s kind `gpu`, payload `[GPUStat]`, every heartbeat (~5s) |
| GET | `/queues` | | `[Queue]` |
| POST | `/queues` | `Queue` (id, backend, client, defaults, ...) | `Queue` |
| GET | `/queues/{id...}` | | `Queue` (ids contain `/`, e.g. `mock/default`; see [queue-plugins.md](queue-plugins.md)) |
| DELETE | `/queues/{id...}` | | `{ok}` |
| GET | `/datasets` | | `[Dataset]` |
| POST | `/datasets` | `Dataset` (name, version, mount, locations) | `Dataset` |
| GET | `/datasets/{name@version}` | | `Dataset` (`name` alone = latest version) |
| DELETE | `/datasets/{name@version}` | | `{ok}` |
| POST | `/datasets/{name@version}/push` | `{node}` | `{node, path}` (prewarm the node cache) |
| GET | `/secrets` | | `[SecretMeta]` (never values) |
| GET | `/secrets/status` | | `SecretStatus` |
| POST | `/secrets/unlock` | `{key?}` (age identity; empty = server keychain/env/file) | `SecretStatus` |
| POST | `/secrets/{ns}/{name}` | `{value_b64, description}` | `SecretMeta` |
| DELETE | `/secrets/{ns}/{name}` | | `{ok}` |
| POST | `/blobs` | raw bytes | `BlobRef` (`blob://<sha256>`) |
| GET | `/blobs/{sha}` | | raw bytes |

`NodeAddRequest`:

```json
{
  "id": "bastion-gpu-1", "display_name": "Bastion GPU 1 (8x GPU)", "labels": ["8gpu"],
  "connection": {"type": "ssh", "host": "bastion.example.com", "port": 2222,
                 "user": "alice@alice@192.0.2.20",
                 "identity": "secret://ssh_keys/id_ed25519",
                 "via": [{"node": "gpu-a"}, {"node": "local"}]},
  "proxy": {"http": "...", "https": "...", "no_proxy": ["..."]},
  "workdir_root": "~/.mldojo/runs", "datasets_cache_root": "~/.mldojo/datasets"
}
```

## AI endpoints

See also [ai-endpoints.md](ai-endpoints.md).

| Method | Path | Returns |
|---|---|---|
| GET | `/ai/runs/{id}/brief` | `{id, project, exp, status, progress, latest_metrics, anomalies, best_ckpt, target, elapsed, eta, summary?}` |
| GET | `/ai/nodes/free` | `[{node_id, gpus_free, mem_free, labels}]` |
| POST | `/ai/runs` | loose `{project, exp?, target, cmd, image?, gpus?, seeds?}` → `SubmitResponse` |
| GET | `/ai/experiments/{project}/{exp}/summary` | `{project, exp, runs, by_status, best_run, metric, next_steps, summary?}` |
| POST | `/ai/anomaly-check/{run}` | `{run_id, anomalies, checked_at}` |

Natural-language `summary` fields are included only with header `X-Include-Summary: true`.

## Observability: `GET /api/v1/metrics`

Prometheus text format, served by a tiny built-in registry (`api/internal/promexp`, no client library).
Admin is required by default; scrapers usually don't carry a token, so `api.metrics_public: true` /
`MLDOJO_METRICS_PUBLIC=1` allows anonymous scraping. **Only do this when the listener is internal or the
reverse proxy restricts access**: the metrics expose project/experiment counts, node counts and the route list.

| Metric | Type | Meaning |
|---|---|---|
| `mldojo_build_info{version}` | gauge | Always 1; the version is in the label |
| `mldojo_uptime_seconds` / `mldojo_goroutines` / `mldojo_memory_alloc_bytes` / `mldojo_memory_sys_bytes` | gauge | Basic process stats |
| `mldojo_runs{status}` | gauge | Runs per status in the database, **cached for 15s** so not every scrape hits the database |
| `mldojo_runs_finished_total{status}` | counter | Runs that reached a terminal state since process start (deleting runs doesn't affect it; use this for failure-rate alerts) |
| `mldojo_run_dispatch_seconds{backend}` | histogram | Wait time from `queued` to `starting` |
| `mldojo_agents_online` | gauge | Agents currently connected back |
| `mldojo_nodes{agent_status}` | gauge | Registered nodes (online/offline) |
| `mldojo_gpus_total` / `mldojo_gpus_busy` | gauge | GPUs reported by online agents / those occupied by MLDojo runs |
| `mldojo_http_requests_total{method,route,status}` | counter | `route` is the **route pattern** (`/api/v1/runs/{id}`), not a concrete id |
| `mldojo_http_request_duration_seconds{method,route}` | histogram | Excludes WebSocket and the streaming `/agent/*` endpoints |
| `mldojo_http_requests_in_flight` | gauge | `/api/` requests being handled |
| `mldojo_db_pool_conns{state}` / `mldojo_db_pool_max_conns` | gauge | pgxpool usage (state: acquired/idle/total) |
| `mldojo_db_pool_acquires_total` / `_empty_acquires_total` / `_acquire_wait_seconds_total` | counter | Connection acquires / acquires that had to wait / total wait time |
| `mldojo_data_dir_bytes` | gauge | Size of `api.data_dir`, sampled in the background every 5 minutes (no disk walk on the scrape path) |
| `mldojo_secrets_unlocked` | gauge | 1 = secrets are unlocked |
| `mldojo_alerts_total{event,outcome}` | counter | Alert delivery: outcome is `sent` / `suppressed` / `failed` |

Label cardinality is bounded: `route` comes from the patterns registered on the `ServeMux`, and every other
label is an enum. The registry also has a hard cap of 2000 series per metric; anything beyond lands in `other`.

## WebSocket frames

Every data-plane message is a `Frame`: `{seq, ts, kind, payload}`.

| kind | payload |
|---|---|
| `log` | `{stream, offset, data}` |
| `metric` | `[MetricPoint]` |
| `gpu` | `[GPUStat]` |
| `status` | `Run` |
| `eof` | `{}`: the run is terminal and everything was delivered |
| `error` | `{error}` |

## Agent endpoints (internal)

| Method | Path | Notes |
|---|---|---|
| WS | `/agent/connect` | reverse connection; `proto/mldojo/v1/agent.go` |
| GET | `/agent/blobs/{sha}` | code bundles / patches (agent token) |
| PUT | `/agent/upload/{id}` | agent → server file upload (artifact fetch) |
| GET/PUT | `/agent/relay/{id}` | server pipes a tar stream from one agent to another (dataset sync) |
