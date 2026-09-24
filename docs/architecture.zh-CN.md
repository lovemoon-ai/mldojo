[English](architecture.md) | 简体中文

# 架构（v1 实现）

本文说明实现中的关键机制。

## 进程

| 进程 | 位置 | 说明 |
|---|---|---|
| `mldojo-api` | API 主机 | Go HTTP 服务，负责控制面（PostgreSQL）和数据面（`data_dir` 下的日志文件与 blob），并托管 agent 连接和 Web 静态文件 |
| `mldojo-agent` | 每个节点 | 反向 WebSocket 连接到 API，负责执行 run、采集 GPU、回传日志/指标/产物 |
| 队列插件（可选） | 通常在 API 主机 | 实现队列插件协议的 HTTP sidecar，API 按 `api.queue_plugins` 里配置的地址调用，见 [queue-plugins.zh-CN.md](queue-plugins.zh-CN.md) |
| PostgreSQL 16 | API 主机 | docker 或 `deploy/native/install-postgres.sh` |

## Agent 连接

- 传输：`GET /api/v1/agent/connect` 升级为 WebSocket，携带 `Authorization: Bearer <agent token>` 和 `X-Mldojo-Node`。
  消息格式见 `proto/mldojo/v1/agent.go`。
- 注册：`node add` 时 server 生成随机 token，DB 只存它的 sha256。agent 首次 hello 后才写入 node 记录，
  失败则什么都不留。
- 断线重连：指数退避加抖动（1s→30s）。重连后 server 在 `welcome` 里告诉 agent 每个 run、每个 stream
  已保存到的字节数，agent 从这里继续发送，所以日志不重复、不丢。指标重连后会整体重扫，server 按
  `(run, key, step)` upsert，保证幂等。
- 保活：server 每 20s 发一次 ping，60s 收不到任何消息就断开。
- 节点连不到 server 时（例如堡垒机后面），可以用 `--reverse-tunnel`：server 保持一条 SSH 连接，
  在节点的 `127.0.0.1:<23000+hash(id)>` 上做 `-R` 转发，agent 通过这个端口拨回来。端口固定，
  API 重启后隧道会自动重建。

## SSH

- 用 `golang.org/x/crypto/ssh`，不依赖系统 ssh。`via` 按顺序尝试，某一条通就用它；`local` 表示直连。
  via 可以递归嵌套（最深 4 层）。
- 用户名原样传递，所以 `a@b@10.x@host` 这种 @-嵌套写法可用（CLI 按最后一个 `@` 切出主机）。
- 认证：`identity`（secret:// 或路径）、`password`（secret://，同时处理 keyboard-interactive）；
  两者都没配时，依次用 ssh-agent 和 `~/.ssh/id_{ed25519,rsa,ecdsa}`。
- 主机指纹：TOFU（首次信任），记录在 `~/.mldojo/known_hosts`。
- 部署：上传二进制（sha256 相同则跳过）并写配置。有 linger 时用 `systemd --user`（`KillMode=process`，
  这样 agent 重启时训练进程不受影响），否则用 `nohup setsid`。
- **`~/.mldojo` 放在哪**：run 工作目录（含 checkpoint）、数据集缓存、venv/conda 缓存都在它下面，留在
  home 会把 home 盘撑爆——常见的情况是系统盘快满了，旁边却挂着几块空着的大数据盘。所以节点上**还没有**
  `~/.mldojo` 时，部署会先挑空闲空间最多的数据盘，在这个用户自己的目录下（`<盘>/<user>`、
  `<盘>/users/<user>`、`<盘>/home/<user>` 里已有且属于他的那个；都没有且盘可写时新建 `<盘>/<user>`）
  建 `mldojo/`，再把 `~/.mldojo` 软链接过去。home 本身就在空闲最多的盘上时（例如 `/home` 就是一块大本地盘）
  什么都不做。不参与挑选的：FUSE（s3fs 报 16 EB 空闲，JuiceFS 是全集群共享的桶）、overlay/tmpfs 等伪文件系统、
  挂载点是单个文件的 bind mount（容器里的 `/etc/hosts`）、`/boot` `/run` 等。
  **已经有 `~/.mldojo` 的节点不动**——部署也在 `node upgrade` 时跑，而正在跑的作业正往里面写。想留在 home，
  部署前先 `mkdir ~/.mldojo`。决策逻辑对着真实节点的探测输出测试（`testdata/real/datahome/`）。
  `node add --dry-run` 会显示打算放在哪，`node show` 的 `mldojo dir` 显示实际在哪。
- `node add --dry-run`：只探测连通性和节点信息，什么都不部署。

## Run 生命周期

```
queued ──dispatch──▶ starting ──agent fetches code / prepares env──▶ running ──▶ succeeded | failed | cancelled
  ▲ stays queued while the agent is offline; dispatched once the agent says hello
```

agent 离线时 run 保持 queued，agent 上线（hello）后自动派发。

- 代码：CLI 打包 tar.gz，git 仓库会同时记录 commit 和脏工作区 patch（包括 untracked 文件），
  上传为 `blob://sha256`。agent 通过 `/agent/blobs/{sha}` 下载。另外两种方式：`git`（agent 自己
  clone repo@ref）和 `inline-patch`（clone 后 apply patch）。
- 执行：`<workdir_root>/<run>/code` 存放代码，`_mldojo/` 下是 `run.sh`、`stdout.log`、`stderr.log`、
  `exit_code` 和 `state.json`。进程用 `setsid` 起在独立 session 里，退出码由 wrapper 写入文件，
  所以 agent 重启后能重新接管。
- env：`none` / `venv`（按 requirements 文件哈希缓存在 `~/.mldojo/envs`）/ `conda`（environment.yaml
  哈希或已有 env 名）/ `docker`（`--gpus device=…`，数据集以只读方式挂载）。
- GPU：没有调度器，但有准入。提交时按节点的静态规格检查 `gpu_type`、`min_mem_gb` 和卡数，
  永远不可能满足的直接报错（exit 2）；**派发时**在节点锁里按心跳报上来的**实际空闲显存**再查一次，
  不够就继续排队并在 `message` 里说明原因——`min_mem_gb` 原本只跟卡的总显存比，
  97 GB 的卡上别人占了 90 GB 也照样放行，作业一上去就 OOM。已经准入但进程还没起来的 run
  也计入占用，否则遥测里那张卡还是空的，前后脚派发的两个 run 会都以为自己放得下。
  节点报不出 GPU 统计（没有 nvidia-smi）时不做这个检查。真正挑哪几张卡由 agent 决定：
  没被 mldojo run 占用、显存占用最少的优先，设置 `CUDA_VISIBLE_DEVICES`。
- 日志：stdout/stderr 由 agent 每 300ms tail 一次，推给 server；`system` stream 记录平台事件。
  WS 帧格式为 `{seq, ts, kind, payload}`。
- 指标：文件扫描（jsonl、tensorboard event，支持增量和部分记录），加上 SDK 写的
  `_mldojo/sdk_metrics.jsonl`，以及 HTTP ingest（`MLDOJO_RUN_TOKEN`）。NaN/Inf 在 JSON 里用字符串表示。
- 产物：按 outputs glob 每 30s 扫一次，结束时再扫一次，URI 为 `node://<node>/<abs path>`。
  下载时 agent 先 PUT 到 server 缓存，再以 Range 方式返回给浏览器（视频预览可用）。

## 队列插件

队列后端以插件形式接入：一个实现[队列插件协议](queue-plugins.zh-CN.md)的 HTTP sidecar，负责把
recipe 翻译成对应集群的作业。target 写作 `queue:<plugin>/<queue>`。server 每 `api.queue_poll_sec`
秒（默认 5）轮询一次 status 和完整日志快照，用 `长度 + 前缀 sha256` 找出新增部分并追加，只把增量推给
UI（badge 显示 "near-realtime"）。指标通过插件读取作业输出里的 jsonl/tensorboard。
产物：结束时插件按 outputs glob 列出文件，这时只记录 URI（`<plugin>://<job>/<path>`），不做搬运。
预览或下载时才通过插件临时下载到 server 缓存。
注意：很多集群在作业结束后代码目录就不可读了，训练代码要把产物和指标写到插件能读到的输出位置，
recipe 里的 outputs 路径也应相对于这些位置。

## 数据集

提交时要求数据集已登记。派发到节点前按以下顺序解析：
① 该节点上登记的 node_path；
② 节点缓存 `datasets_cache_root/<name>/<version>`（存在 `.mldojo_complete` 标记才算）；
③ 从其他在线节点的 node_path 同步（优先 authoritative）。同步走 server relay：源 agent 以 tar 流
PUT，目标 agent 以 GET 解包，两边都只需要能连到 server，所以堡垒机后的节点也能用。完成后登记为新的
node_path。

挂载：非 docker 环境优先做软链 `mount → path`，没有权限时退回环境变量 `MLDOJO_DATASET_<NAME>`；
docker 环境用 `-v path:mount:ro`；队列插件通常用 bucket mount。

## Secrets

值用 age（X25519）加密后存入 `secrets` 表。主密钥的来源依次为：OS keychain（macOS Keychain /
Secret Service，3s 超时）→ `MLDOJO_MASTER_KEY` → `master_key_file`，也可以用 `mldojo secret unlock`
推送。首次启动会自动生成主密钥（优先存入 keychain），并记录 recipient，用来防止换错 key。
处于锁定状态时每分钟自动重试一次。任何 API 都不会返回明文。

## 其他实现细节

- API 路径统一加 `/api/v1` 前缀（Web 与 API 同源）。
- runs 表有 `name` 列，nodes 表有 `agent_token_hash` 列，另有 `settings` 表（token、主密钥 recipient）。
- Web 采用 Next.js 静态导出，页面用 query 参数路由（`/run?id=`）。
- 数据集在节点上的"挂载"用软链实现，挂不上时退回环境变量。
