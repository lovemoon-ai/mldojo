[English](api.md) | 简体中文

# MLDojo API（v1）

所有接口都在 `/api/v1` 下，请求和响应都是 JSON。各对象的结构以 `proto/mldojo/v1/api.go` 里的
Go 类型为准。

## 认证

- 单一 token（`MLDOJO_TOKEN`）：`Authorization: Bearer <token>`。
- WebSocket 和产物/blob 的原始 URL（浏览器没法设请求头）：用 `?token=<token>` 查询参数
  （所有接口都接受）。
- `POST /runs/{id}/metrics` 还接受 run 自己的 token（`MLDOJO_RUN_TOKEN`）。
- `GET /api/v1/health` 不需要认证。
- `GET /api/v1/metrics` 需要 **admin**，除非打开 `api.metrics_public`（见 [deploy.zh-CN.md](deploy.zh-CN.md)）。


## 权限

认证通过后还有一层授权（migration `0003_authz.sql`）：

| 主体 | 角色 | 能做什么 |
|---|---|---|
| API token | `admin` | 全部。CLI、agent 和脚本拿的就是它，所以保持管理员 |
| SSO 用户，列在 `sso.admins` | `admin` | 全部 |
| 其它 SSO 用户 | `member` | 读全部；提交 run；**只能删自己拥有的** project / experiment / run |

- **管理员专属**：`POST/DELETE /nodes`、`/nodes/{id}/upgrade`、`POST/DELETE /queues`、
  `DELETE /datasets/{ref}`、`/secrets/*` 的写操作、`GET /audit`、`GET /users`。
  这些要么能让服务端向外发起 SSH，要么能改写服务端随后会使用的凭据。
- **归属**：project 用 `owner`，run 用 `metadata.submitter`，都在创建时按认证身份写入
  （只有管理员能代他人指定 `owner`）。**没有 owner 的旧数据任何人都可以删**，
  否则历史数据会永远卡住。
- 角色跟着配置走：`sso.admins` 在每次登录时同步到 `users.role`，所以提权是改配置 + 重新登录，
  而不是手工 UPDATE 数据库。

### 浏览器侧防护

- 会话 cookie 是 `SameSite=Lax`，而 Lax 是 **site** 级的 —— 同一注册域名下的兄弟站点
  （例如 `*.example.com`）算同站。所以**状态变更请求会校验 `Origin`**：
  带 cookie 的跨源 `POST/PUT/DELETE` 返回 403；带 Bearer token 的不受影响
  （跨站页面读不到 token，无从伪造）。WebSocket 握手同样校验。
- CORS 只回配置内的 origin（`public_url` / `web_url`），不再反射任意 `Origin`。
- `/auth/logout` 只接受 `POST`（GET 会被任意页面的 `<img>` 触发）。
- 每个 `/api/` 响应带 `X-Request-Id`，与服务端访问日志和审计记录里的 `request_id` 对应。

## 错误与退出码

非 2xx 响应返回 `{"error": "...", "code": "..."}`。

| HTTP | code | CLI exit |
|---|---|---|
| 400/422 | `user_error` | 2 |
| 404 | `not_found` | 2 |
| 502/503/504 | `unreachable` | 3 |
| 502 | `backend_error` | 4 |
| 403 | `forbidden` |
| 409 | `conflict` | 5 |
| 401 | `unauthorized` | 2 |

## 资源

| 方法 | 路径 | 请求体 | 返回 |
|---|---|---|---|
| GET | `/health` | | `Health` |
| GET | `/metrics` | | Prometheus 文本格式（`text/plain; version=0.0.4`）——**admin only**，除非 `api.metrics_public` |
| GET | `/projects` | | `[Project]` |
| POST | `/projects` | `{name, description}` | `Project` |
| GET | `/projects/{name}` | | `Project` |
| DELETE | `/projects/{name}?force=1` | | `{ok}`（有 experiment 且没带 force 时返回 409） |
| GET | `/projects/{p}/experiments` | | `[Experiment]` |
| POST | `/projects/{p}/experiments` | `{name, description, recipe_yaml, tags}` | `Experiment` |
| GET | `/projects/{p}/experiments/{e}` | | `Experiment` |
| DELETE | `/projects/{p}/experiments/{e}?force=1` | | `{ok}` |
| GET | `/runs?project=&experiment=&status=&target=&limit=` | | `[Run]`（新的在前） |
| POST | `/runs` | `SubmitRequest` | `SubmitResponse` |
| GET | `/runs/{id}` | | `Run`（id 可以是唯一前缀） |
| POST | `/runs/{id}/cancel` | | `Run` |
| GET | `/audit?actor=&action=&target=&limit=200` | | `[AuditEntry]` —— **admin only**。谁改了什么，附 request id |
| GET | `/users` | | `[User]` —— **admin only** |
| DELETE | `/runs/{id}?force=1` | | `{deleted}` —— 同时删除该 run 的日志、指标、产物索引和事件。运行中的 run 会被拒绝，除非 `force=1`（先取消再删） |
| GET | `/runs/{id}/events` | | `[RunEvent]` |
| GET | `/runs/{id}/logs?stream=stdout&offset=0&limit=1048576&tail=0` | | `text/plain`；响应头 `X-Log-Size` = 总字节数。`tail=N` 返回最后 N 字节 |
| WS | `/runs/{id}/logs/ws?stream=stdout\|stderr\|system\|all&offset=0&follow=1` | | `Frame`（见下文） |
| GET | `/runs/{id}/metrics?key=&since_step=0&max_points=2000` | | `{keys: [string], points: [MetricPoint], sampled: bool}`。`max_points` 限制的是**每个 key** 的点数（等间隔采样，保留首尾）；`sampled` 表示是否有 key 被抽稀 |
| WS | `/runs/{id}/metrics/ws?since=step:0` | | kind 为 `metric` 的 `Frame` |
| POST | `/runs/{id}/metrics` | `[MetricPoint]` 或 `{points:[...]}` | `{ok, count}`（SDK 上报；接受用户 token 或 run token `MLDOJO_RUN_TOKEN`） |
| GET | `/runs/{id}/artifacts` | | `[Artifact]` |
| POST | `/runs/{id}/artifacts/refresh` | | `[Artifact]`（让 agent 重新扫描） |
| GET | `/runs/{id}/artifacts/raw?uri=<uri>` | | 文件字节（支持 `Range`，用于视频/图片预览） |
| GET | `/runs/{id}/gpu` | | 分配给该 run 的 GPU 的 `[GPUStat]` |
| GET | `/runs/{id}/code` | | `{source, repo, ref, commit, dirty, patch, bundle_uri}`（`patch` 是脏工作区的 diff 文本） |
| GET | `/runs/{id}/models` | | 该 run 产出的 `[ModelVersion]` |
| GET | `/models?project=` | | `[Model]` |
| GET | `/models/{p}/{name}` | | `{model, versions: [ModelVersion], used_by: [{version, run_id, project, experiment, name, status}]}` —— `used_by` 是读过这个版本的 run |
| POST | `/models/{p}/{name}/versions` | `{run_id, uri, stage, notes}` | `ModelVersion`（201）。recipe 里的 `outputs.model` 让 run 成功时自动做这件事 |
| POST | `/models/{p}/{name}/versions/{v}/stage` | `{stage}` | `ModelVersion`（production/staging 唯一，原持有者转 archived） |
| GET | `/compare?a=<run>&b=<run>` | | `{a: Run, b: Run, code: {...}, metrics: [{key, a, b, delta}], env: [{path, a, b}]}` |
| GET | `/nodes` | | `[Node]` |
| POST | `/nodes` | `NodeAddRequest` | `Node`（阻塞到 agent 拨回来，最多 60s；失败即报错，不留记录）。带 `"dry_run": true` 时返回 `{ok, route, latency_ms, probe}`，什么都不部署 |
| GET | `/nodes/{id}` | | `Node` |
| DELETE | `/nodes/{id}?stop_agent=1` | | `{ok}` |
| POST | `/nodes/{id}/test` | | `{ok, reachable, agent_online, latency_ms, via, message}` |
| GET | `/nodes/{id}/gpu` | | `[GPUStat]` |
| WS | `/nodes/{id}/gpu/ws` | | kind 为 `gpu` 的 `Frame`，payload 为 `[GPUStat]`，每次心跳（约 5s）一帧 |
| GET | `/queues` | | `[Queue]` |
| POST | `/queues` | `Queue`（id、backend、client、defaults 等） | `Queue` |
| GET | `/queues/resources` | | `{queues: [QueueResource + plugin, queue_id?], errors?, warnings?}`，来自插件的实时容量（[queue-plugins.zh-CN.md](queue-plugins.zh-CN.md#资源)） |
| GET | `/queues/{id...}` | | `Queue`（id 里带 `/`，例如 `mock/default`；见 [queue-plugins.zh-CN.md](queue-plugins.zh-CN.md)） |
| DELETE | `/queues/{id...}` | | `{ok}` |
| GET | `/datasets` | | `[Dataset]` |
| POST | `/datasets` | `Dataset`（name、version、mount、locations） | `Dataset` |
| GET | `/datasets/{name@version}` | | `Dataset`（只写 `name` = 最新版本） |
| DELETE | `/datasets/{name@version}` | | `{ok}` |
| POST | `/datasets/{name@version}/push` | `{node}` | `{node, path}`（预热节点缓存） |
| GET | `/secrets` | | `[SecretMeta]`（从不返回值） |
| GET | `/secrets/status` | | `SecretStatus` |
| POST | `/secrets/unlock` | `{key?}`（age identity；为空 = 用服务端 keychain/env/file） | `SecretStatus` |
| POST | `/secrets/{ns}/{name}` | `{value_b64, description}` | `SecretMeta` |
| DELETE | `/secrets/{ns}/{name}` | | `{ok}` |
| POST | `/blobs` | 原始字节 | `BlobRef`（`blob://<sha256>`） |
| GET | `/blobs/{sha}` | | 原始字节 |

`NodeAddRequest`：

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

## AI 接口

另见 [ai-endpoints.zh-CN.md](ai-endpoints.zh-CN.md)。

| 方法 | 路径 | 返回 |
|---|---|---|
| GET | `/ai/runs/{id}/brief` | `{id, project, exp, status, progress, latest_metrics, anomalies, best_ckpt, target, elapsed, eta, summary?}` |
| GET | `/ai/nodes/free` | `[{node_id, gpus_free, mem_free, labels}]` |
| POST | `/ai/runs` | 宽松的 `{project, exp?, target, cmd, image?, gpus?, seeds?}` → `SubmitResponse` |
| GET | `/ai/experiments/{project}/{exp}/summary` | `{project, exp, runs, by_status, best_run, metric, next_steps, summary?}` |
| POST | `/ai/anomaly-check/{run}` | `{run_id, anomalies, checked_at}` |

只有带请求头 `X-Include-Summary: true` 时才返回自然语言的 `summary` 字段。

## 可观测性：`GET /api/v1/metrics`

Prometheus 文本格式，自带一个极小的 registry（`api/internal/promexp`，不引入 client 库）。
默认要 admin；抓取端通常不带 token，所以可以用 `api.metrics_public: true` /
`MLDOJO_METRICS_PUBLIC=1` 放开匿名抓取——**只在内网监听或反代做了限制时才这么配**，
指标会暴露项目/实验规模、节点数和路由清单。

| 指标 | 类型 | 说明 |
|---|---|---|
| `mldojo_build_info{version}` | gauge | 恒为 1，版本在 label 里 |
| `mldojo_uptime_seconds` / `mldojo_goroutines` / `mldojo_memory_alloc_bytes` / `mldojo_memory_sys_bytes` | gauge | 进程基础量 |
| `mldojo_runs{status}` | gauge | 库里各状态的 run 数，**带 15s 缓存**，每次抓取不会都打库 |
| `mldojo_runs_finished_total{status}` | counter | 进程启动以来进入终态的 run 数（删 run 不影响它，失败率告警用这个） |
| `mldojo_run_dispatch_seconds{backend}` | histogram | 从 `queued` 到 `starting` 的等待时间 |
| `mldojo_agents_online` | gauge | 当前有反向连接的 agent 数 |
| `mldojo_nodes{agent_status}` | gauge | 注册节点数（online/offline） |
| `mldojo_gpus_total` / `mldojo_gpus_busy` | gauge | 在线 agent 上报的卡数 / 其中被 MLDojo run 占用的卡数 |
| `mldojo_http_requests_total{method,route,status}` | counter | route 是**路由模式**（`/api/v1/runs/{id}`），不是具体 id |
| `mldojo_http_request_duration_seconds{method,route}` | histogram | 不含 WebSocket 与 `/agent/*` 流式端点 |
| `mldojo_http_requests_in_flight` | gauge | 正在处理的 `/api/` 请求数 |
| `mldojo_db_pool_conns{state}` / `mldojo_db_pool_max_conns` | gauge | pgxpool 连接池占用（state：acquired/idle/total） |
| `mldojo_db_pool_acquires_total` / `_empty_acquires_total` / `_acquire_wait_seconds_total` | counter | 取连接次数 / 需要等待的次数 / 累计等待时间 |
| `mldojo_data_dir_bytes` | gauge | `api.data_dir` 占用，后台每 5 分钟采样一次（不在抓取路径上走盘） |
| `mldojo_secrets_unlocked` | gauge | 1 = secrets 已解锁 |
| `mldojo_alerts_total{event,outcome}` | counter | 告警出口：outcome 为 `sent` / `suppressed` / `failed` |

标签基数是有界的：route 取自 `ServeMux` 的注册模式，其余 label 都是枚举值；
registry 另有每个指标 2000 series 的硬上限，超出后落进 `other`。

## WebSocket 帧

数据面的每条消息都是一个 `Frame`：`{seq, ts, kind, payload}`。

| kind | payload |
|---|---|
| `log` | `{stream, offset, data}` |
| `metric` | `[MetricPoint]` |
| `gpu` | `[GPUStat]` |
| `status` | `Run` |
| `eof` | `{}`：run 已结束，所有数据都已送达 |
| `error` | `{error}` |

## Agent 接口（内部）

| 方法 | 路径 | 说明 |
|---|---|---|
| WS | `/agent/connect` | 反向连接；见 `proto/mldojo/v1/agent.go` |
| GET | `/agent/blobs/{sha}` | 代码包 / patch（agent token） |
| PUT | `/agent/upload/{id}` | agent → server 上传文件（取产物用） |
| GET/PUT | `/agent/relay/{id}` | server 把一个 agent 的 tar 流转给另一个 agent（数据集同步） |
