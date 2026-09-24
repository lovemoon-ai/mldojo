[English](queue-plugins.md) | 简体中文

# 队列插件

除了 SSH 节点，MLDojo 还能把 run 提交到外部作业队列（集群调度器、云上批处理服务、内部训练平台等）。
每种这样的后端都是一个**队列插件**：一个实现下述协议的小型 HTTP "sidecar" 进程。API server 调用它来
提交作业、轮询状态和日志、取消作业、读取指标和输出文件。MLDojo 本身不带任何特定集群的插件，只带一个
参考实现 **mock** 插件，它把作业当作本地子进程来跑。

## 概念

| 东西 | 形式 | 例子 |
|---|---|---|
| 插件名 | `<plugin>` | `mock` |
| 队列 id | `<plugin>/<queue>` | `mock/default` |
| run target | `queue:<plugin>/<queue>` | `queue:mock/default` |
| 产物 URI（作业输出） | `<plugin>://<job>/<path>` | `mock://j-1a2b/outputs/model.ckpt` |
| 产物 URI（bucket） | `bucket://<bucket>/<path>` | `bucket://my-bucket/runs/x/model.ckpt` |

## 配置

在 `config.yaml` 里告诉 API 每个插件监听在哪：

```yaml
api:
  queue_plugins:
    mock: http://127.0.0.1:8766
  queue_poll_sec: 5          # how often queue runs are polled (default 5)
```

或者用环境变量（逗号分隔的 `name=url`）：

```bash
MLDOJO_QUEUE_PLUGINS="mock=http://127.0.0.1:8766,other=http://127.0.0.1:8767"
```

`GET /api/v1/health`（以及 `mldojo health`）用 `"queue_plugins": {"<name>": true|false}` 报告每个插件
是否可达。没配插件时是 `{}`，这很正常。

然后注册队列并提交：

```bash
mldojo queue add --id mock/default --backend mock
mldojo queue add --id <plugin>/<queue> --backend <plugin> \
    --credentials secret://<plugin>/default --defaults-file q.yaml
mldojo run submit -f recipes/examples/hello/recipe.yaml --target queue:mock/default --wait
```

队列 run 的日志是**轮询**的（准实时），不是流式的：server 每 `queue_poll_sec` 秒取一次状态和完整日志
快照，只追加新增的部分。

recipe 可以针对队列 target 覆盖 env，比如在那里用 docker 镜像：

```yaml
env:
  default: {type: venv, spec: requirements.txt}
  overrides:
    - when: {backend_kind: queue, backend: mock}
      use: {type: docker, image: registry.example.com/my-team/train:latest}
```

### 部署插件

- **原生部署**：`deploy/native/install.sh` 会对 `MLDOJO_PLUGIN_DIRS`（在 `deploy/site.env` 里设置）列出的
  每个目录执行 `<dir>/install.sh`。放在独立（可以是私有）仓库里的插件自带一个 `install.sh`，负责安装
  sidecar（例如装成 `systemd --user` unit），并把自己加进 `MLDOJO_QUEUE_PLUGINS`。
- **docker compose**：compose 不带任何插件。单独运行插件，并给 api 容器设置 `MLDOJO_QUEUE_PLUGINS`。

## 用 mock 插件在本地试一下

```bash
python3 adapters/queue_sidecar/mock/server.py --host 127.0.0.1 --port 8766
# in another shell, start the API with:
MLDOJO_QUEUE_PLUGINS=mock=http://127.0.0.1:8766
mldojo queue add --id mock/default --backend mock
mldojo run submit -f recipes/examples/hello/recipe.yaml --target queue:mock/default --wait
```

mock 插件只依赖 Python 标准库。作业状态放在 `--state-dir` 下（默认 `$TMPDIR/mldojo-queue-sidecar`）：
`<state-dir>/jobs/<job_id>/{code,output.log}`。作业先 `queued` 约 1 秒，然后 `running`，再按退出码变成
`succeeded` 或 `failed`。`cancel` 向进程组发 SIGTERM（5 秒后 SIGKILL）。要写真正的插件，从它改起最方便。

## 协议

请求体和响应体都是 JSON。出错时返回 4xx/5xx 状态码和 `{"error": "..."}`。

**凭据。** 如果注册队列时带了 `--credentials secret://...`，API 会解析这个 secret，并在每个请求上以
`X-Queue-Credentials: base64(credentials)` 发送（`POST /jobs` 还会放在 `credentials` 字段里）。凭据的
内容（token、配置文件……）由插件自己决定。插件应当在日志和错误信息里隐去 token 和密码。

| 方法 / 路径 | 请求体 | 响应 |
|---|---|---|
| `GET /health` | | `{"ok": true, "mock": bool, "sdk": "<version>"\|null, "error"?}`（`error` 说明配置问题） |
| `POST /jobs` | 见下文 | `{"job_id", "workspace_folder", "url", "dag_id": null, "raw": {...}}` |
| `GET /jobs/{id}/status` | | `{"job_id", "phase", "raw_phase", "message", "started_at", "finished_at", "exit_code"}` |
| `GET /jobs/{id}/log` | | `{"job_id", "log": "<full snapshot>"}` |
| `POST /jobs/{id}/cancel` | `{}` | `{"ok": true}`（必须幂等） |
| `POST /jobs/{id}/metrics` | `{"bucket", "paths": [{"type": "jsonl"\|"tensorboard", "path"}], "tracking"}` | `{"points": [{"step", "key", "value", "ts"}], "warnings"?}` |
| `POST /jobs/{id}/files` | `{"bucket", "globs": [{"kind", "glob"}], "max_files": 2000}` | `{"files": [{"kind", "path", "size"}], "warnings"?}` |
| `GET /jobs/{id}/download?bucket=&path=` | | 原始字节，`application/octet-stream`，已知大小时带 `Content-Length` |

通用规则：
- 时间戳用 RFC3339 或 `null`。
- 未知的 job id 返回 404。
- 与底层调度器通信失败返回 502；插件本身没配好返回 503。
- `POST /jobs` 必须返回非空的 `job_id`。

### `POST /jobs`

请求体字段：`run_id, job_name, queue_name, project_id, docker_image, num_workers, gpu_per_worker,
cpu_per_worker, cpu_mem_ratio, wall_time_min, cmd, workdir, bundle_url, bundle_token, env, input_bucket,
output_bucket, mounts[{bucket,path,mount}], job_password, credentials, extra`。

- `queue_name` 是队列 id 里的 `<queue>` 部分。`project_id`、`job_password`（由 `--job-password secret://...`
  解析而来）和 `extra` 来自队列定义；`extra` 是一个不透明对象（队列的 `defaults.extra`），插件可以把它合并进
  自己原生的作业描述。
- 资源取自 recipe，缺省时退回队列的 `defaults`（1 个 worker、0 GPU、4 CPU、ratio 4、60 分钟）。
- 代码包是 `bundle_url` 上的一个 tar.gz；下载时带 `Authorization: Bearer <bundle_token>`，解包时要做路径安全
  检查。`cmd` 应当在解包后代码里的 `workdir` 下执行。
- `env` 必须导出到作业环境里，其中包括 `MLDOJO_RUN_ID`、`MLDOJO_RUN_TOKEN`、`MLDOJO_API_URL`、
  `MLDOJO_PROJECT`、`MLDOJO_EXPERIMENT` 和 `MLDOJO_PARAM_*`。
- `input_bucket` / `output_bucket` 可以是 `"a,b"` 或列表。`mounts` 是带 bucket location 的数据集；只能挂载
  整个 bucket 的插件可以在作业脚本里把 `path` 软链到 `mount`。
- `url`（可选）指向调度器 UI 里该作业的页面，会显示在 run 页面上。

### 状态 phase

`phase` 必须是 MLDojo 的 phase 之一：`queued`、`starting`、`running`、`succeeded`、`failed`、`cancelled`。
调度器自己的状态放 `raw_phase`，原因放 `message`。知道退出码时填 `exit_code`（成功为 0；被信号 N 杀死时
报 128+N 是个好约定）。

### 日志

`GET /jobs/{id}/log` 返回到目前为止的**完整**日志（作业开始前返回 `""`）。API 对前后两次快照做 diff，所以
日志只能增长。

### 指标

`paths` 相对于作业的输出位置（`bucket` 为 `""`），或相对于某个具名 bucket（队列的
`defaults.metrics_bucket`）。插件完全读不了指标时返回 501，API 之后就不再为这个 run 请求指标。

jsonl 解析（每行一个 JSON 对象）：
- step 取 `step`，没有则取 `_step`，再没有则用行号。
- 其余所有有限数值型的顶层字段都作为 key。
- 时间戳取 `ts`、`timestamp` 或 `_timestamp`（epoch 秒/毫秒或 ISO），都没有则用文件 mtime。
- 解析不了的行（比如写了一半的最后一行）跳过。

tensorboard：单个 event 文件，或者一个目录（扫描其中的 `*tfevents*`）；支持 `simple_value` 和标量
float/double tensor。

文件不存在是一条 `warnings`，不是错误：指标文件经常在 run 后期才出现。

### 产物：`/files` 和 `/download`

路径相对于作业的输出位置（`bucket` 为 `""`），或相对于某个具名 bucket（队列的 `defaults.artifacts_bucket`）。
产物只在 run 结束时列一次；用户预览或下载时才按需取文件。

Glob：
- `*` 和 `?` 只匹配一个路径段内；`**` 匹配任意层目录（`outputs/**/*.ckpt` 同时匹配 `outputs/a.ckpt` 和
  `outputs/x/y/a.ckpt`）；`{a,b}` 表示多选一，可以嵌套。
- glob 与整个相对路径匹配。
- 一个文件归属于第一个匹配它的 glob，所以 `outputs/**` 这种兜底的要放最后。
- 列目录要有上限（mock 在 16 层、访问 20000 个条目或 `max_files`（最多 10000）时停止）。触到上限或前缀不存在
  时加一条 `warnings`，不算错误。

下载时文件不存在返回 404，路径非法、越界或是目录时返回 400。大文件要流式返回，不要整个读进内存。

注意：很多调度器在作业结束后代码目录就不可读了。训练代码应该把 checkpoint 和指标写到插件读取的输出位置，
recipe 里的 outputs glob 也应相对于那里。
