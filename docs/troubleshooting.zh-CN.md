[English](troubleshooting.md) | 简体中文

# 故障排查

按**症状**索引。每条的结构都是：症状 → 怎么确认 → 怎么修。
所有报错文案都是代码里的原文，可以直接拿去 `grep`。

新装的实例请先走 [docs/quickstart.md](quickstart.zh-CN.md)；部署细节见 [docs/deploy.md](deploy.zh-CN.md)。

症状索引：

- [agent 一直 offline / `node add` 失败](#症状agent-一直-offline--node-add-失败)
- [run 一直卡在 queued](#症状run-一直卡在-queued)
- [run 一直卡在 starting](#症状run-一直卡在-starting)
- [run 立刻失败，exit code 96 或 97](#症状run-立刻失败exit-code-96-或-97)
- [secrets are locked](#症状secrets-are-locked)
- [日志不刷新 / 指标看不到](#症状日志不刷新--指标看不到)
- [Web 打不开 / 一直 401](#症状web-打不开--一直-401)
- [SSO 登录后跳回 localhost](#症状sso-登录后跳回-localhost)
- [SSO 配好了，但登录页上根本没有 SSO 按钮](#症状sso-配好了但登录页上根本没有-sso-按钮)
- [队列插件不可用](#症状队列插件不可用)
- [数据库连不上 / 迁移失败](#症状数据库连不上--迁移失败)

---

## 先跑这三条

八成的问题在这里就定位了：

```bash
mldojo health                 # 控制面：db / secrets / 在线 agent 数 / 队列插件
mldojo node ls                # 每个节点的 agent 是不是 online、心跳多久以前
mldojo run logs <run> --stream system   # 平台侧对这个 run 做了什么（不是你的训练输出）
```

`--stream system` 是最被低估的一条：调度、分发、数据集同步、agent 掉线、reap
都写在这条流里，带 `[mldojo]` 前缀。

### CLI 退出码

`mldojo <任何命令> --help` 顶部也写着这一行。写脚本时判退出码比解析输出可靠：

| 码 | 含义 | 典型情况 |
|---|---|---|
| 0 | 成功 | |
| 2 | 用户错误 | 参数错、资源不存在（404）、token 无效（401）、recipe 不合法 |
| 3 | 不可达 | 连不上 API、节点 SSH 不通、agent 离线 |
| 4 | 后端错误 / run 失败 | 服务端 5xx；**`run submit --wait` 时有 run 没 succeeded 也是 4** |
| 5 | 冲突 | 同名 project / node 已存在 |
| 130 | Ctrl-C | |

**没有 1**——CLI 任何路径都不会返回 1。`mldojo-api` 相反，启动失败一律 exit 1。
`--json` 时错误也是 JSON，打到 **stdout**：`{"error": ..., "code": ..., "exit_code": N}`。

> 注意区分：上面是 **CLI 进程**的退出码。`mldojo run ls` 里 `EXIT` 列是**训练进程**的退出码，
> 两套完全无关（见 [exit code 96 / 97](#症状run-立刻失败exit-code-96-或-97)）。

---

## 症状：agent 一直 offline / `node add` 失败

### 怎么确认

```bash
mldojo node test <id>        # 分两步报：SSH 能不能通、agent 有没有连回来
mldojo node ls               # AGENT 列 + HEARTBEAT 列
```

`node test` 的 message 只有三种：`ok`、`agent is not connected`、`agent ping failed: ...`。
拿不准路由问题时用不落库的探测：

```bash
mldojo node add --id <id> --ssh <user@host> --identity secret://ssh_keys/<k> --dry-run
```

> `node add` 是**全有或全无**的：任何一步失败都不写数据库（`AddNode` 注释原文
> "Any failure returns an error and stores nothing"）。所以失败后**不需要**先 `node rm`，直接改命令重试。

### 按报错对症

| 报错（原文） | 原因 | 怎么修 |
|---|---|---|
| `no ssh auth method: set identity or password` | 既没 `--identity` 也没 `--password` | 给一个。私钥推荐走 secret：`mldojo secret set ssh_keys/k --from-file ~/.ssh/id_ed25519` 后 `--identity secret://ssh_keys/k` |
| `identity secret://ssh_keys/k: ...` | 私钥读不出或解析不了 | 确认 secret 存在（`mldojo secret ls`）且 secrets 没锁；**带密码短语的私钥不支持**，换一把无密码的 |
| `host key for <host> changed (edit ~/.mldojo/known_hosts if expected): ...` | 首次信任后主机指纹变了（重装/换机/中间人） | 确认是预期变更后，编辑 `~/.mldojo/known_hosts`（**API 主机上**的，不是你 `~/.ssh/known_hosts`）删掉旧行 |
| `all routes to <id> failed: via local: ...; via gpu-a: ...` | 所有 `--via` 路由都没通，分号后面是每条的具体原因 | 逐条看。`via local` 指 API 主机直连 |
| `via chain too deep (cycle?)` | `--via` 形成环 | 检查 via 链，节点不能间接指回自己 |
| `node cannot be its own via` | `--via` 写了自己 | 去掉 |
| `node "x" already exists` | id 占用（409，退出码 5） | 换 id，或 `mldojo node rm x` |
| `connection.host is required for ssh nodes` | `--ssh` 没给 host | `--ssh user@host` |
| `reverse_tunnel requires an ssh node` | `--local` 配了 `--reverse-tunnel` | 本机节点不需要隧道，去掉 |

### `agent ... did not connect back within 60s`

完整文案：

```
agent on <route> did not connect back to <url> within 60s (is the server reachable from the node?
set connection.agent_server_url or reverse_tunnel). agent log:
<节点上的 agent 日志尾部>
```

这条说明 **SSH 通了、agent 装上了、但它连不回 API**。错误里已经附了节点上的 agent 日志尾部，先读它。

确认节点能不能到 API（`<url>` 就是报错里那个）：

```bash
ssh <user@host> "curl -sS -m 5 <url>/api/v1/health"
```

修法按情况：

- **`public_url` 配错**（最常见）：写成了 `localhost` 或 API 主机自己才能解析的地址。
  agent 用的是 `api.public_url` / `MLDOJO_PUBLIC_URL`。没配时直接报
  `server public_url is not configured; set api.public_url (MLDOJO_PUBLIC_URL) to a URL agents can reach`。
- **单个节点走另一条地址**：`mldojo node add ... --agent-server-url http://<别的地址>:8765`。
- **节点根本到不了 API**（公网机、防火墙单向）：加 `--reverse-tunnel`，让 agent 走 SSH 反向隧道回连。
  加之前先 `--dry-run` 验证，输出里会多一行 `reverse tunnel:`：`ok` 表示服务端允许 `-R`，
  `not allowed: ...` 表示被禁（堡垒机常见）。

节点本地的 agent 日志（`node add` 失败后 agent 会被停掉，但日志还在）：

```bash
ssh <user@host> "tail -50 ~/.mldojo/agent/<node-id>.log"
```

> 没有 `mldojo node logs` 这个命令——agent 日志只在节点本地。

### 堡垒机：命令被静默丢弃

报错：

```
upload agent to <host>: checksum mismatch after upload (a bastion command filter may have dropped the command)
```

有的 JumpServer 堡垒机会**静默丢弃**包含 `rm -f` 的 exec 命令（返回空输出、exit 0）。
部署脚本为此不用 `rm`，并且每一步都校验是否真的生效（二进制 sha256、启停脚本的完成标记）。
这条报错就是那个校验抓到的。同类的还有：

- `start script did not complete on <host> (output: "...")`
- `stop script did not complete on <host> (output: "...")`
- `unexpected probe output from <host>: "..."`

修法：看 `output:` 里堡垒机到底回了什么。通常要么是命令被过滤，要么是登录时有横幅/交互提示混进了输出。

### 多层堡垒机的用户名

`--ssh` 按**最后一个 `@`** 切分 user/host，所以嵌套用户名可以原样写：

```bash
mldojo node add --id bastion-gpu-1 --ssh "alice@alice@192.0.2.20@bastion.example.com" --port 2222 \
    --identity secret://ssh_keys/id_ed25519 --via gpu-a --via local --dry-run
```

`--via` 可重复，**按顺序尝试、失败自动回退**；`local` 表示从 API 主机直连。
`--dry-run` 输出的 `route:` 会告诉你最终走通的是哪条，例如 `local -> gpu-a -> bastion-gpu-1`。

### agent 装上了但过一会又 offline

判定口径（都在服务端）：

- agent 每 **5 秒**心跳一次；
- WebSocket **60 秒**没有任何消息就断开，节点立刻标记 offline（ping 每 20s 发一次）；
- 连接一断，`nodes.agent_status` 马上写 `offline`。

所以 `HEARTBEAT` 超过 1 分钟基本就是真断了。查节点上的 agent 是否还活着：

```bash
ssh <user@host> "systemctl --user is-active mldojo-agent-<id>; tail -30 ~/.mldojo/agent/<id>.log"
```

agent 日志里会有 `disconnected from server` 带 `retry_in`（它会自己退避重连）。

**最常见的原因是没开 linger**：`systemd --user` 的服务在你 SSH 登出后被杀掉。

```bash
ssh <user@host> "loginctl show-user \$(id -un) -p Linger --value"   # 期望 yes
ssh <user@host> "loginctl enable-linger \$(id -un)"
```

没有可用 `systemctl --user` 的节点会退化成 `nohup setsid`，那种情况下机器重启 agent 就没了，
重新 `mldojo node add` 即可。

---

## 症状：run 一直卡在 queued

`queued` 表示平台收下了，但还没派给任何 agent。**queued 的 run 永远不会被自动判死**，
所以它可以一直挂着。

### 怎么确认

```bash
mldojo run logs <run> --stream system
mldojo run show <run>            # 看 message 字段
mldojo node ls                   # 目标节点在线吗
```

典型 system 日志：

```
[mldojo] node <id> agent is offline; the run stays queued until it reconnects
```

对应 run 的 message 是 `waiting for agent on <node>`。

### 怎么修

1. **目标节点离线** —— 这是绝大多数情况。按上面
   [agent 一直 offline](#症状agent-一直-offline--node-add-失败) 把节点弄回来，
   run 会自己继续（不用重新提交）。
2. **目标写错了**。提交时就会拒（退出码 2）：``unknown node "x" (see `mldojo node ls`)``。
   target 格式只有两种：`node:<id>` 和 `queue:<backend>/<queue>`。
3. **不想等了**：`mldojo run cancel <run>`。

> 资源不够**不会**让 run 卡在 queued——`run submit` 在提交阶段就检查并拒绝：
> `node <id> cannot satisfy gpus=4 gpu_type="" min_mem_gb=0 (node has: ...)`，退出码 2。

---

## 症状：run 一直卡在 starting

`starting` 覆盖的是**代码上传 → 镜像拉取 → 环境准备**这一段，第一次跑可能真的很慢
（conda env create、docker pull 都在这里）。

### 怎么确认

```bash
mldojo run logs <run> --stream system   # 分发进度
mldojo run logs <run> --stream stderr   # pip / conda / docker 的输出在这里
```

进入 starting 时 system 流里是 `[mldojo] dispatching to <node>`。

### 怎么修

- **等**。starting 的上限是 **2 小时**，超时后 reaper 把 run 标记 failed，原因写
  `stuck in starting for more than 2h0m0s`。
- **确实卡住了**：`mldojo run cancel <run>`，然后去节点上看 agent 日志。
  常见是 docker pull 拉不动内网镜像、或 conda 在解依赖。
- **想避开重复开销**：venv/conda 环境按 spec 文件的 hash 缓存在节点的
  `~/.mldojo/envs/`，同一个 spec 第二次跑就不用重建了。

---

## 症状：run 立刻失败，exit code 96 或 97

这两个是 **agent 包装脚本**约定的退出码，不是你的训练脚本返回的，也和 CLI 退出码无关。
它们出现在 `mldojo run ls` 的 `EXIT` 列和 `mldojo run show` 的 `message` 里。

| EXIT | message（原文） | 含义 |
|---|---|---|
| 96 | `workdir not found` | 包装脚本的 `cd <workdir>` 失败 |
| 97 | `environment setup failed (see stderr)` | venv / conda 环境准备失败 |

### 96：workdir not found

`run.workdir` 是**相对代码根目录**的（`code.source: git` 时是仓库根，不是 recipe 文件所在目录）。

```bash
mldojo run show <run>                   # 确认 workdir 和 code source
mldojo run logs <run> --stream system
```

修法：把 recipe 里的 `run.workdir` 改成代码包里真实存在的相对路径。
`code.source: local` 时还要确认那个目录没有被 `.gitignore` / `.mldojoignore` 过滤掉——
`local` 模式在 git 仓库内会**按 `.gitignore` 过滤**打包。

### 97：environment setup failed

先看 stderr，agent 会把原因打出来，都带 `[mldojo]` 前缀：

```bash
mldojo run logs <run> --stream stderr
```

| stderr 里的行 | 修法 |
|---|---|
| `[mldojo] conda not found on this node` | 节点上没 conda。agent 找的是 `conda`（PATH）、`~/miniconda3`、`~/miniforge3`、`~/anaconda3`、`/opt/conda`。装一个，或改用 `env.type: venv` |
| `[mldojo] conda env <name> not found` | `env.spec` 写的是环境名但节点上没有。改成 spec 文件（`environment.yaml`）让 agent 自己建 |
| `[mldojo] creating venv ...` 后面跟着 pip 报错 | 依赖装不上。多半是网络；给节点配代理：`mldojo node add ... --proxy http://...`（agent 会把 `http(s)_proxy` 传给 run） |
| `[mldojo] venv setup failed` / `[mldojo] conda env setup failed` | 同上，看它前面那几行真实报错 |

环境建失败时那个半成品目录会被删掉，所以修好问题直接重新提交即可，不用手动清理。

> `env.type: none`（hello recipe 用的）不会有 97——它只 `cd` 然后直接跑命令。
> 那种情况下 `python3: command not found` 会让 run 以 **127** 失败。

---

## 症状：secrets are locked

确切文案（注意是复数 `secrets are locked`，不是 `is`）：

```
secrets are locked: run `mldojo secret unlock` or set MLDOJO_MASTER_KEY
```

secrets 是用 age 主密钥加密的。锁住时**所有**读写 secret 的路径都会失败——
不只是 `mldojo secret set`，还包括任何 `secret://` 引用：SSH identity/password、
队列插件凭据、bucket 凭据。所以症状常常表现为 "`node add` 突然认证失败了"。

### 怎么确认

```bash
mldojo health                  # secrets: locked
mldojo secret ls               # 第一行 master key: locked
```

正常是 `master key: unlocked (<source>)`，source 告诉你钥匙是从哪来的。

服务端日志里对应两条：

```
secrets: master key rejected            # 找到钥匙了，但不是加密时用的那把
secrets: locked; unlock with `mldojo secret unlock`
```

### 怎么修

服务端自动解锁的顺序是 **keychain → `MLDOJO_MASTER_KEY` → key 文件**
（默认 `~/.mldojo/master.key`，可用 `MLDOJO_MASTER_KEY_FILE` 覆盖），
**任一把不匹配会自动往下试**。启动时试一次，之后只要还锁着就**每分钟重试**
（keyring 可能要等用户登录后才可用）。

```bash
mldojo secret unlock                              # 用 CLI 本机 keychain / MLDOJO_MASTER_KEY
mldojo secret unlock --key-file ~/.mldojo/master.key
```

成功输出 `secrets unlocked (key from <source>)`。

按场景：

- **服务器重启后锁着**：多半是 keychain 在无头会话里拿不到。
  容器/无头机上设 `MLDOJO_NO_KEYCHAIN=1`，改用 key 文件或 `MLDOJO_MASTER_KEY`。
  compose 默认就是这么干的（`MLDOJO_MASTER_KEY_FILE=/data/master.key`）。
- **换机 / 从备份恢复后锁着**：把备份里的 `master.key` 放回 `~/.mldojo/master.key` 就行，
  旧机器残留的 keychain 条目不会挡路（不匹配会自动往下试）。
  注意备份**默认不含主密钥**，要 `MLDOJO_BACKUP_INCLUDE_KEY=1` 才会打进去——
  见 [deploy.md 备份与恢复](deploy.zh-CN.md#备份与恢复)。
- **钥匙彻底丢了**：`master key does not match the key secrets were encrypted with`。
  已加密的 secrets **救不回来**，只能删掉重新 `mldojo secret set`。run / 指标 / 日志不受影响。

> 首次启动且库里还没有任何 secret 时，API 会**自动生成**一把主密钥（优先存 keychain，
> 失败则写 key 文件），日志是 `secrets: generated master key`。这是正常的。

---

## 症状：日志不刷新 / 指标看不到

### 日志不刷新

日志实时推送走 WebSocket（`/api/v1/runs/{id}/logs/ws`）。先分清是"没产生"还是"没送达"：

```bash
mldojo run logs <run> --stream all        # 一次性拉取，不走 WS
mldojo run logs <run> --stream all -f     # 跟随，走 WS
```

一次性拉能看到、`-f` 看不到 → **WebSocket 被中间层掐了**。几乎总是反向代理少了 Upgrade 头：

```nginx
proxy_http_version 1.1;
proxy_set_header Upgrade $http_upgrade;
proxy_set_header Connection $connection_upgrade;
proxy_read_timeout 1h;
proxy_buffering off;
```

两种都看不到 → 日志确实没产生：

- **Python 缓冲**。这是最常见的原因，和 MLDojo 无关。在 recipe 里加
  `run.env: {PYTHONUNBUFFERED: "1"}`，或 `print(..., flush=True)`。
- 输出全在 stderr：`--stream stderr`（默认只看 stdout）。
- 队列插件的 run 是**轮询**的，不是实时的：日志会有几秒延迟，`run show` 里 `log_mode` 是 `near-realtime`。

> `--stream all` 只在 WebSocket 上支持。直接打 REST 的 `GET /runs/{id}/logs?stream=all`
> 会报 `stream must be stdout|stderr|system`——那个端点只认单条流。

### 指标看不到

```bash
mldojo run metrics <run>            # 空表 = 一个点都没采到
```

指标有三条来源，确认你的 recipe 用的是哪条：

1. **文件扫描（默认，不用改代码）**：必须在 `outputs.metrics` 里声明路径，否则 agent 根本不去扫：
   ```yaml
   outputs:
     metrics:
       - {type: jsonl, path: outputs/metrics.jsonl}
       - {type: tensorboard, path: outputs/tb}
   ```
   路径相对 workdir。jsonl 要求**每行一个 JSON 对象**；step 依次取
   `step`/`_step`/`global_step`/`iteration`/`iter`/`epoch` 第一个存在的，都没有就用行号。
   其余数值字段都算指标。**训练脚本要及时 flush**，否则文件里一直是空的。
2. **SDK**：`import mldojo; mldojo.log({"loss": x}, step=i)`。agent 会自动把 SDK 放进 `PYTHONPATH`；
   别的环境（比如 docker 镜像里）用 `pip install sdk/python`。
3. **wandb shim**：recipe 里设 `run.wandb: shim`，`wandb.init/log/finish` 会写进 MLDojo。
   验证方式：日志里 `wandb.__version__` 应该是 `0.0.0-mldojo-shim`，不是真 wandb 的版本号。

曲线点数比预期少：`GET /metrics` 默认 `max_points=2000`，**按 key 均匀抽样**（保留首尾），
响应里 `sampled: true` 就表示抽过。CLI 拿全量用 `--key <key>` 单独取。

---

## 症状：Web 打不开 / 一直 401

### 页面是一句 "The web app is not built"

看到这个说明 API 活着，但没找到静态页面：

```
MLDojo API <version>
The web app is not built (set api.web_dir / MLDOJO_WEB_DIR to web/out). API: /api/v1/health
```

修法：`make web` 构建出 `web/out`，然后把 `api.web_dir` 指过去（原生部署是
`~/.mldojo/app/web`，`deploy/native/install.sh` 会原子替换它）。
国内网络：`npm_config_registry=https://registry.npmmirror.com make web`。
注意 Next 要求 Node ≥ 20.9。

### 页面能开但接口全 401

```bash
curl -s -o /dev/null -w '%{http_code}\n' http://<server>/api/v1/projects        # 期望 401
curl -s -o /dev/null -w '%{http_code}\n' -H "Authorization: Bearer $MLDOJO_TOKEN" \
     http://<server>/api/v1/projects                                            # 期望 200
```

401 是**预期行为**（除 `/api/v1/health` 外所有端点都要 token）。Web 首次打开会让你粘贴 token。
带 token 还 401 → token 不对，重新 `mldojo-api token issue`（幂等，打印现有的）。
刚 `token rotate` 过的话，所有节点都要 `mldojo node upgrade <id>` 换新 token。

浏览器和 WebSocket 没法设 header，所以它们用 `?token=<token>` 查询参数——**所有端点都接受**。

### 单页路由 404

`/run`、`/nodes` 这些路径 404 但首页正常 → 反代的 try_files 规则不对。
参考 `deploy/compose/nginx.conf`：

```nginx
location / { try_files $uri.html $uri $uri/index.html =404; }
```

（Next 静态导出的 `/run` 对应 `run.html`，而同名目录也存在，顺序不能反。）

---

## 症状：SSO 登录后跳回 localhost

登录走完 Conductor 却跳到了 `http://localhost:8765/...`，或者回调地址不对。

### 怎么确认

```bash
curl -s https://<你的域名>/api/v1/auth/config
```

### 怎么修

反向代理会隐藏真实 Host，所以 MLDojo **不从请求里推断**回调地址，全部基于配置拼。
设 `api.web_url`（或 `MLDOJO_WEB_URL`）为用户实际访问的公网地址：

```yaml
api:
  web_url: https://mldojo.example.com
```

没设 `web_url` 时会退回用 `public_url`——而 `public_url` 是给 agent 回连用的内网地址，
于是就跳回了内网/localhost。这两个地址用途不同，公网部署时都要配。

回调地址是 `<web_url>/api/v1/auth/callback`，**三处必须逐字节一致**（authorize 链接、
Conductor 上注册的 `redirect_uris`、token 请求），差一个斜杠就是 `invalid_grant`。

其他 SSO 报错（都会原样带到登录页 URL 的 `?error=`）：

| 报错 | 原因 |
|---|---|
| `token exchange failed (HTTP 401): ... (client_id/client_secret do not match the ones registered with the SSO provider)` | `invalid_client`：没注册、密钥不对，或 Conductor 没重启加载 |
| `token exchange failed (HTTP 400): ... (the code expired or was already used, or redirect_uri does not match the registered value byte for byte)` | `invalid_grant` |
| `login state expired, please try again` | state cookie 过期（10 分钟）或被丢弃 |
| `<用户> is not on this instance's allowlist` | 登录成功但不在 `sso.allow` 里 |

不带浏览器的验证方法：用真 secret 加一个假 code 打 token 端点，
返回 `invalid_grant` 说明 client_id/secret/redirect_uri 都对了，
返回 `invalid_client` 说明还没注册或没重启。

---

## 症状：SSO 配好了，但登录页上根本没有 SSO 按钮

**这是最近改过的行为，配置没动过的实例升级后会遇到。**

登录页只剩一个 token 输入框，提示语也从"用 Conductor 账号登录，或使用 API token。"
变成了英文的 `Paste your MLDojo API token (MLDOJO_TOKEN).`；
直接访问 `/api/v1/auth/login` 返回 400 `SSO is not configured on this server (api.sso in config.yaml)`。

### 怎么确认

```bash
curl -s http://<server>/api/v1/auth/config      # {"sso_enabled":false,...}
```

然后看 API 启动日志，有这一条就是它：

```
Conductor SSO is configured but disabled: sso.allow is empty. An empty allowlist would let
every account of the provider in with full access. Set sso.allow (or MLDOJO_SSO_ALLOW) to
the ids/emails that may sign in.
```

```bash
journalctl --user -u mldojo-api | grep "SSO is configured but disabled"    # 原生部署
docker compose logs api | grep "SSO is configured but disabled"           # compose
```

### 原因

**`sso.allow` 为空现在等于禁用 SSO。** 以前空白名单等于"放行所有人"——对公网部署这不是个
安全的默认值（Conductor 上任何一个账号都能拿到完整权限）。现在的判定是：

```go
func (c Config) Enabled() bool { return c.Configured() && len(c.Allow) > 0 }
```

即 OAuth 四项（`base_url` / `client_id` / `client_secret` / `web_url`）齐全**且**
白名单非空，SSO 才会启用。只配了前者、`allow` 留空或写成 `[]`，SSO 就静默关掉了，
只剩 token 登录——这正是"登录入口消失"的样子。

### 怎么修

显式列出允许登录的人。`id`、`email`、`phone`、`name` 都可以，匹配时不分大小写：

```yaml
# ~/.mldojo/config.yaml 的 api: 段
api:
  web_url: https://mldojo.example.com
  sso:
    base_url: https://conductor.example.com
    client_id: mldojo
    client_secret: <32 字节随机>
    allow:
      - you@example.com
      - teammate@example.com
```

或者用环境变量（逗号分隔，会**整个覆盖**配置文件里的 `allow`）：

```bash
MLDOJO_SSO_ALLOW=you@example.com,teammate@example.com
```

改完重启 API，启动日志应该变成 `Conductor SSO enabled`，并带上 `allowlist=<人数>`。再确认一次：

```bash
curl -s http://<server>/api/v1/auth/config      # 期望 "sso_enabled":true
```

> 白名单只管**能不能登录**。登录后不做细粒度鉴权，登录者会记录到
> `runs.metadata.submitter` 和 `projects.owner`（只记录、不鉴权）。

---

## 症状：队列插件不可用

`/api/v1/health`（以及 `mldojo health`）里 `"queue_plugins"` 有某个插件是 `false`，例如
`"queue_plugins": {"<name>": false}`。队列后端都是队列插件——一个实现了队列插件协议的 HTTP
sidecar 进程，协议见 [queue-plugins.zh-CN.md](queue-plugins.zh-CN.md)。

**没配任何插件时是 `{}`，完全正常**，可以无视。某个插件是 `false` 只影响 `queue:<plugin>/*`
这种 target，`node:*` 的 run 一点不受影响。

### 怎么确认

```bash
mldojo health                                        # queue_plugins: {"<name>": false}
curl -s <plugin-url>/health                          # 直接问插件进程
```

`<plugin-url>` 就是配置里给这个插件写的地址：config.yaml 的 `api.queue_plugins: {<name>: <url>}`，
或环境变量 `MLDOJO_QUEUE_PLUGINS="name=url,name2=url2"`（两处都没有这个名字，它就不会出现在 health 里）。

### 怎么修

- **插件进程没在跑**：把它起来。私有插件一般自带安装脚本和 systemd unit（原生部署时由
  `deploy/native/install.sh` 按 `MLDOJO_PLUGIN_DIRS` 安装），用 `systemctl --user status <unit>`
  和它自己的日志排查。
- **URL 配错**：确认 `api.queue_plugins` / `MLDOJO_QUEUE_PLUGINS` 里的地址从 **API 主机**
  （compose 下是 api 容器）能访问到，改完重启 API。
- **队列没注册**：提交时报 ``unknown queue "x" (see `mldojo queue ls`)``，先
  `mldojo queue add --id <plugin>/<queue> --backend <plugin> ...`。

**compose 用户注意**：compose 里不带任何插件。单独把插件跑起来，然后给 api 容器设
`MLDOJO_QUEUE_PLUGINS`。

### 用 mock 插件验证整条链路

分不清是插件的问题还是 MLDojo 的问题时，用自带的 mock 插件（作业在本机以子进程运行）：

```bash
python3 adapters/queue_sidecar/mock/server.py --host 127.0.0.1 --port 8766
MLDOJO_QUEUE_PLUGINS=mock=http://127.0.0.1:8766     # 设给 API 后重启
mldojo queue add --id mock/default --backend mock
mldojo run submit -f recipe.yaml --target queue:mock/default
```

mock 能跑通说明 API 侧没问题，问题在你的插件或它背后的调度器。

---

## 症状：数据库连不上 / 迁移失败

API 启动失败一律 exit 1，错误打到 stderr，前缀是 `mldojo-api:`。
迁移在 `serve` 启动时和 `migrate` 子命令里都会跑，**失败即启动失败**。

### 怎么确认

```bash
journalctl --user -u mldojo-api -n 50      # 原生部署
docker compose logs api --tail 50          # compose
curl -s http://<server>/api/v1/health       # "db" 字段不是 "ok" 时直接是驱动的原始错误
```

### 按报错对症

| 报错 | 原因 / 修法 |
|---|---|
| `mldojo-api: database_url is not set (api.database_url in ~/.mldojo/config.yaml or MLDOJO_DATABASE_URL)` | 没配连接串。原生部署由 `install-postgres.sh` 写到 `~/.mldojo/postgres.url`，`install.sh` 再写进 config |
| `connect postgres: ...` | 连不上。Ping 超时是 10 秒 |
| `database url: ...` | 连接串本身语法不对（`pgxpool.ParseConfig` 失败） |
| `migration <NNN_xxx.sql>: ...` | 某个迁移执行失败。每个迁移**各自一个事务**，失败会回滚，所以库不会停在半截状态 |

排查连接本身（原生安装的 PostgreSQL 在 `~/opt/pgsql-16/bin`，**不在 PATH 上**）：

```bash
systemctl --user status mldojo-postgres
cat ~/.mldojo/postgres.url
~/opt/pgsql-16/bin/psql "$(cat ~/.mldojo/postgres.url)" -c 'select 1'
```

compose：

```bash
docker compose ps postgres                 # 期望 healthy
docker compose exec postgres pg_isready -U mldojo
```

已应用的迁移记在 `schema_migrations` 表里，按文件名字典序执行：

```bash
psql "$MLDOJO_DATABASE_URL" -c 'select * from schema_migrations order by version'
```

**迁移失败后不要手动改表**。先看清是哪个文件挂了；恢复到已知正常状态用
[deploy.md 的恢复流程](deploy.zh-CN.md#恢复)。

> `mldojo-api db create` / `db drop` 存在但主要给测试用（`scripts/e2e.sh` 靠它建临时库）。
> **`db drop` 会删掉整个数据库**，别在生产上手滑。

---

## 还是没解决

1. **拿 e2e 当基准**：`scripts/e2e.sh` 会在当前主机起一个完全隔离的实例
   （独立端口、数据库、agent、mock 队列插件），跑完自动清理。
   它过了说明这台机器的基础设施没问题，问题在你的配置或 recipe；它挂了，看它第一条 `✗` 挂在哪个 section。
   `KEEP=1 scripts/e2e.sh` 会把实例留着供你手工检查。
2. **打开服务端 debug 日志**：`MLDOJO_LOG_LEVEL=debug`，重启 API。
3. **`--json`**：每条命令都支持，错误也是结构化的（`{"error": ..., "code": ..., "exit_code": N}`），
   比读人类输出更容易定位。
4. **确认版本**：`mldojo health` 的 `version` 和 `mldojo node ls` 里 agent 的版本对不上时，
   `mldojo node upgrade <id>` 升级 agent（正在跑的 run 会被新 agent 接着接管，不会中断）。
