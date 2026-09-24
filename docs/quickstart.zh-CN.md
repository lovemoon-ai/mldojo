[English](quickstart.md) | 简体中文

# Quickstart：从零到第一个 run

目标：在一台干净的机器上起好 MLDojo，接入一个计算节点，跑通 `recipes/examples/hello/`，
在 CLI 和 Web 上都看到日志、指标和产物。**全程约 10 分钟**（不含镜像构建）。

每一步都给了**期望看到的输出**。对不上就跳到 [docs/troubleshooting.zh-CN.md](troubleshooting.zh-CN.md)，
那里按症状索引。

---

## 0. 先选一条路

MLDojo 分两层，可以分开放：

- **控制面**：`mldojo-api`（Go）+ PostgreSQL + Web。只需要能跑容器或能跑一个 Go 二进制。
- **计算节点**：装 `mldojo-agent` 的机器，训练真正跑在这里。**它需要 `python3`**（hello recipe 用 `python3 train.py`）。

| 你的情况 | 走哪条 |
|---|---|
| 想最快看到东西，机器上有 docker | **A：docker-compose**（本文主线） |
| 训练就在这台机器上跑，没有 root / 没有 docker | **B：原生部署** → [docs/deploy.zh-CN.md](deploy.zh-CN.md#原生部署无-root无-docker) |
| 在仓库里改代码、要热重载 | `mldojo dev up`（前台起 postgres + API + Next.js dev server，需要 checkout、go、npm、docker） |

> **A 和 B 的关键差别在「加节点」这一步**：原生部署时 API 就跑在你要训练的机器上，
> 一句 `mldojo node add --local` 就完事；compose 模式下 API 在容器里，
> `--local` 指的是**容器自己**——详见 [第 4 步](#4-接入第一个计算节点)。

---

## 1. 起控制面（docker-compose）

`MLDOJO_PUBLIC_URL` 是**节点上的 agent 回连 API 的地址**，所以不能写 `localhost`——
要写节点能访问到的地址。单机自测填本机内网 IP 即可。

```bash
cd <repo-dir>
export MLDOJO_PUBLIC_URL=http://<LAN-IP>:8765
export POSTGRES_PASSWORD=$(openssl rand -hex 16)   # 必填；也可以都写进 .env（见 .env.example）
docker compose up -d
```

首次会构建两个镜像（Go + Next.js），慢的话几分钟。期望：

```
[+] Running 4/4
 ✔ Network mldojo_default       Created
 ✔ Container mldojo-postgres-1  Healthy
 ✔ Container mldojo-api-1       Started
 ✔ Container mldojo-web-1       Started
```

确认 API 活着（这个端点**不需要 token**）：

```bash
curl -s http://localhost:8765/api/v1/health
```

```json
{"ok":true,"version":"94ddcb5","db":"ok","secrets":"unlocked","agents_online":0,"queue_plugins":{}}
```

要点：
- `"ok":true` 且 `"db":"ok"` → 控制面通了。`db` 不是 `ok` 就是数据库没连上。
- `"agents_online":0` 正常，还没加节点。
- `"queue_plugins":{}` 正常，表示没有配置任何队列插件（跑 hello 用不到）。

## 2. 拿 API token

token 存在数据库里，由 API 自己签发。`token issue` 是幂等的——已经有就打印现有的那个：

```bash
docker compose exec api /app/bin/mldojo-api token issue
```

```
mld_7Qk2xR9vNbF3hJ8pLwYz0aCdEsTuViMn
```

> 轮换用 `mldojo-api token rotate`（旧 token 立即失效，所有节点的 agent 需要 `mldojo node upgrade` 换新 token）。

## 3. 装 CLI 并登录

CLI 是单个静态二进制。从容器里拷一份出来最省事（`dist/mldojo-linux-amd64` 是 Linux CLI）：

```bash
docker compose cp api:/app/dist/mldojo-linux-amd64 ./mldojo   # Linux
chmod +x ./mldojo && sudo mv ./mldojo /usr/local/bin/mldojo
```

macOS 或想自己编译：`make build` 后二进制在 `bin/mldojo`。

```bash
mldojo login --server http://localhost:8765 --token mld_7Qk2xR9vNbF3hJ8pLwYz0aCdEsTuViMn
```

```
logged in to http://localhost:8765 (0 projects); saved to /home/you/.mldojo/config.yaml
```

`(0 projects)` 说明 token 真的被服务端接受了——`login` 会先拿 token 调一次 `/projects` 验证，
失败就不会把它写进配置。

这会写 `~/.mldojo/config.yaml`（`0600`）。之后所有命令不用再带 `--server/--token`。
验证：

```bash
mldojo health
```

```
ok:            true
version:       94ddcb5
db:            ok
secrets:       unlocked
agents_online: 0
```

> 没配 server 时报 `no server configured: run `mldojo login --server URL --token TOKEN` or set MLDOJO_SERVER`，退出码 2。

## 4. 接入第一个计算节点

**节点必须有 `python3`**，否则第 5 步的 hello recipe 会以 exit 97 失败。

### 4a. 原生部署：`--local`

API 直接跑在这台机器上时，一句就够（`--local` = API 所在主机自己）：

```bash
mldojo node add --id local --local --labels dev
```

```
deploying agent to local (up to ~60s)...
node local online: -, 32 CPU, 124 GiB RAM (agent 94ddcb5)
```

`-` 是 GPU 摘要，表示没检测到 GPU；有卡时形如 `1x RTX 5090`。

### 4b. docker-compose：把宿主机作为 SSH 节点接进来

compose 模式下 `--local` 会把 agent 装进 **api 容器**里，而那个镜像是 `debian:bookworm-slim`，
**只装了 ca-certificates 和 git，没有 python3，也没有 systemd**。所以 hello recipe 在那里跑不起来。
正确做法是把**宿主机**（或任何一台有 python3 的机器）当成 SSH 节点接进来。

API 在容器里，读不到你 `~/.ssh` 下的私钥，所以先把私钥存成 secret，再用 `secret://` 引用：

```bash
mldojo secret set ssh_keys/mykey --from-file ~/.ssh/id_ed25519
```

```
stored secret://ssh_keys/mykey (464 bytes)
```

先用 `--dry-run` 只测连通性，**不部署任何东西**：

```bash
mldojo node add --id host --ssh $USER@<LAN-IP> --identity secret://ssh_keys/mykey --dry-run
```

```
reachable:     yes
route:         local -> host
latency:       12ms
host:          mybox (linux/amd64)
gpus:          -
python:        3.12.3
docker/conda:  true/false
(dry run: nothing was deployed)
```

`python` 那行有版本号就说明第 5 步能跑。确认后去掉 `--dry-run` 真正部署：

```bash
mldojo node add --id host --ssh $USER@<LAN-IP> --identity secret://ssh_keys/mykey --labels dev
```

```
deploying agent to host (up to ~60s)...
node host online: -, 32 CPU, 124 GiB RAM (agent 94ddcb5)
```

> `node add` 是**全有或全无**的：任何一步失败都不会在数据库里留下记录，
> 所以失败后直接改命令重试即可，不需要先 `node rm`。

### 验证节点在线

```bash
mldojo node ls
```

```
ID    AGENT   GPUS  UTIL  ACTIVE  LABELS  HEARTBEAT
host  online  -     -     0       dev     3s ago
```

`AGENT` 必须是 `online`，`HEARTBEAT` 应该在几秒内（agent 每 5s 心跳一次）。
不是 online 见 [troubleshooting：agent 一直 offline](troubleshooting.zh-CN.md#症状agent-一直-offline--node-add-失败)。

## 5. 提交第一个 run

仓库自带的 hello recipe 不需要 GPU，只用 `python3` 跑 20 步假训练，写 jsonl 指标和一个假
checkpoint。先看一眼它长什么样：

```bash
cat recipes/examples/hello/recipe.yaml
```

它的 `params.seed` 是个列表 `[0, 1]`。**不加 `--matrix` 时列表参数只取第一个值**，
加了 `--matrix seed` 才会展开成 2 个 run。先跑单个：

```bash
mldojo run submit -f recipes/examples/hello/recipe.yaml --target node:host --wait
```

```
submitted 1 run(s): a3f1c2d8
ID        EXPERIMENT          NAME         TARGET      STATUS     EXIT  CREATED  DURATION
a3f1c2d8  smoke/hello-world   hello-world  node:host   succeeded  0     8s ago   6s
```

`STATUS` 是 `succeeded`、`EXIT` 是 `0` 就成了。

- `--wait` 会等所有 run 结束；**任何一个没 succeeded，CLI 退出码是 4**（适合写进 CI）。
- 想边跑边看日志用 `--follow`（单 run 时会直接流式打印 stdout+stderr+system）。
- 目标格式是 `node:<id>` 或 `queue:<plugin>/<queue>`。

状态一直是 `queued` 或 `starting` 不动，见
[troubleshooting：run 卡在 queued](troubleshooting.zh-CN.md#症状run-一直卡在-queued)。

## 6. 看日志、指标和产物

把上一步的 run id（可以用唯一前缀，8 位就够）记作 `$RUN`：

```bash
RUN=a3f1c2d8
```

**日志**（默认只有 stdout；`--stream all` 把 stdout/stderr/平台事件合在一起，system 行带 `[mldojo]` 前缀）：

```bash
mldojo run logs $RUN --stream all
```

```
[mldojo] status -> starting
[mldojo] status -> running
hello from mldojo run a3f1c2d8-... seed=0
CUDA_VISIBLE_DEVICES=
step 1/20 loss=0.9149
...
step 20/20 loss=0.1443
done
[mldojo] status -> succeeded
```

跑的过程中可以 `-f` 跟随到结束（注意 `-f` 在 `run logs` 里是 `--follow`，
而在 `run submit` 里是 `--file`）。

**指标**。hello 走两条路：jsonl 文件扫描（`loss`、`lr`）和 Python SDK（`sdk/acc`），两条都该有 20 个点：

```bash
mldojo run metrics $RUN
```

```
KEY      POINTS  LAST STEP  LAST      MIN       MAX
loss     20      20         0.144296  0.135335  0.914935
lr       20      20         0.001     0.001     0.001
sdk/acc  20      20         0.855704  0.085065  0.864665
```

单个 key 的完整曲线用 `--key`：

```bash
mldojo run metrics $RUN --key loss
```

表是空的见 [troubleshooting：指标看不到](troubleshooting.zh-CN.md#症状日志不刷新--指标看不到)。

**产物**：

```bash
mldojo run artifacts ls $RUN
```

```
KIND  SIZE  URI
ckpt  16 B  file:///home/you/.mldojo/runs/host/a3f1c2d8-.../outputs/model_step20.ckpt
```

下载到本地（走 agent 中转，不需要你能直连节点）：

```bash
mldojo run artifacts get $RUN <URI-from-above> --dest /tmp
```

```
saved /tmp/model_step20.ckpt (16 B)
```

**一次看完**（状态、进度、最新指标、异常、最好的 checkpoint）：

```bash
mldojo ai brief $RUN
```

## 7. 跑一个参数矩阵

这才是 recipe 的意义——一次提交，多个 run：

```bash
mldojo run submit -f recipes/examples/hello/recipe.yaml --target node:host --matrix seed --wait
```

```
submitted 2 run(s): 7b3e91a4 c05d2f68
```

两个 run 只有 `seed` 不同。对比一下：

```bash
mldojo compare 7b3e91a4 c05d2f68
```

```
A: 7b3e91a4 hello-world (succeeded)
B: c05d2f68 hello-world (succeeded)

code: same_commit=true same_patch=true  A=... dirty=false  B=... dirty=false

METRIC   A         B         DELTA
loss     0.144296  0.141872  -0.002424
sdk/acc  0.855704  0.858128  +0.002424

SETTING       A  B
params.seed   0  1
```

`--matrix all` 展开所有列表参数；`--param k=v` 临时覆盖（值按 YAML 解析，
所以 `--param seed=[0,1,2]` 是列表、`--param steps=100` 是整数）。

## 8. 打开 Web

```
http://localhost:3000
```

首次打开会要求粘贴 token（就是第 2 步那个），之后存在浏览器里。应该能看到：

- 首页：项目 / 实验 / run 列表，刚才那几个 run 在最上面
- run 页：日志实时刷新（WebSocket）、指标曲线、产物、GPU 占用
- `/nodes`：节点和实时 GPU 状态

Web 是纯静态导出 + PWA，可以装到手机桌面。
打不开或 401 见 [troubleshooting：Web 打不开](troubleshooting.zh-CN.md#症状web-打不开--一直-401)。

---

## 常见岔路

**没有 docker / 不能装 docker** → 走原生部署，全程无 root：
[docs/deploy.zh-CN.md 原生部署](deploy.zh-CN.md#原生部署无-root无-docker)。
需要 Go ≥ 1.26 和 Node ≥ 20.9，PostgreSQL 由 `deploy/native/install-postgres.sh` 装成
systemd --user 服务（端口 55432），不需要系统级的 postgres。

**国内网络拉不动依赖**：

```bash
make build dist GOPROXY=https://goproxy.cn,direct
npm_config_registry=https://registry.npmmirror.com make web
```

**没有 GPU**：hello recipe 的 `resources.default.gpus` 是 `0`，CPU 机器上能跑。
真实 recipe 要 GPU 时，`run submit` 会在提交阶段就检查节点容量并拒绝
（例如 1 卡节点上要 4 卡，退出码 2）。

**手上只有一台机器，还想试 SSH 节点**：`--ssh $USER@127.0.0.1` 是合法的，
e2e 测试就是这么干的——前提是你能免密 `ssh $USER@127.0.0.1`。

**想试试队列（可选）**：仓库自带一个 mock 队列插件，把 job 作为本地子进程跑：

```bash
python3 adapters/queue_sidecar/mock/server.py --host 127.0.0.1 --port 8766
```

让 API 带上 `MLDOJO_QUEUE_PLUGINS=mock=http://127.0.0.1:8766` 重启，然后
`mldojo queue add --id mock/default --backend mock`，提交时用 `--target queue:mock/default`。
协议和写自己的插件见 [docs/queue-plugins.zh-CN.md](queue-plugins.zh-CN.md)。

**想先确认整套东西在这台机器上能跑通**：直接跑 `scripts/e2e.sh`。
它起一个完全隔离的实例（独立端口、数据库、agent），覆盖 CLI/API/agent/SSH/ProxyJump/
数据集/指标/产物/取消/重启续传/Web 路由，跑完自动清理。需要 Postgres + 能 SSH 到自己 + systemd。

---

## 下一步

**写自己的 recipe** → [docs/recipes.zh-CN.md](recipes.zh-CN.md)。从 hello 改起最快：
把 `code.source` 换成 `git`（CLI 会自动带上 HEAD commit 和未提交的改动，保证可回溯）、
`env.default.type` 换成 `venv` 或 `conda`、`run.cmd` 换成你的训练命令。
指标默认靠文件扫描（jsonl / tensorboard），一行代码都不用改；
想主动上报就 `import mldojo; mldojo.log({"loss": x}, step=i)`，
已经在用 wandb 的话设 `run.wandb: shim` 就能原样跑。

**加更多节点** → [docs/deploy.zh-CN.md 节点](deploy.zh-CN.md#节点)。
支持 ProxyJump（`--via a --via b`，按顺序回退）、多层堡垒机的 `@` 嵌套用户名、
节点连不到 API 时用 `--reverse-tunnel` 反向隧道。加之前一律先 `--dry-run`。

**接 API / 写自动化** → [docs/api.zh-CN.md](api.zh-CN.md)（~50 个端点，含 WebSocket 帧格式和
错误码↔HTTP↔退出码对照表）、[docs/ai-endpoints.zh-CN.md](ai-endpoints.zh-CN.md)（给 agent 用的紧凑端点）。
CLI 每条命令都支持 `--json`，退出码稳定（0 成功 / 2 用户错误 / 3 不可达 / 4 后端或 run 失败 / 5 冲突），
不想解析 JSON 时直接判退出码就行。

**公网访问和 SSO 登录** → [docs/deploy.zh-CN.md](deploy.zh-CN.md#公网访问ssh-反向隧道--公网机-nginx)。

**出问题** → [docs/troubleshooting.zh-CN.md](troubleshooting.zh-CN.md)。
