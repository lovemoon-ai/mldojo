[English](deploy.md) | 简体中文

# 部署

## 站点配置：deploy/site.env

与具体站点相关的设置放在 `deploy/site.env` 里。这个文件已被 gitignore，不会进仓库；从模板复制一份再改：

```bash
cp deploy/site.env.example deploy/site.env
```

| 键 | 含义 |
|---|---|
| `MLDOJO_PUBLIC_URL` | agent 和用户访问 API 用的地址，例如 `https://mldojo.example.com` |
| `MLDOJO_INTRANET_URL` | 可选：给连不到公网地址的节点用的第二个地址，例如 `http://mldojo.internal:8765` |
| `MLDOJO_DEFAULT_SERVER` | 在 `make build` 时编进 CLI，作为默认的 `--server`；留空则为 `http://localhost:8765` |
| `MLDOJO_PLUGIN_DIRS` | 要安装的队列插件目录（见下文「原生部署」和 [queue-plugins.zh-CN.md](queue-plugins.zh-CN.md)） |

- `MLDOJO_DEFAULT_SERVER` 是**构建时**生效的：改了之后要重新 `make build` 才会进到 CLI 里。
- SKILL.md 由 API 在 `/SKILL.md` 提供。`deploy/native/install.sh` 会渲染其中的占位符：
  `{{MLDOJO_SERVER}}` → `MLDOJO_PUBLIC_URL`，`{{MLDOJO_INTRANET_SERVER}}` → `MLDOJO_INTRANET_URL`（未设置时用公网地址）。

## docker-compose

```bash
export MLDOJO_PUBLIC_URL=http://<server-ip>:8765   # agent 反向连接用
export POSTGRES_PASSWORD=$(openssl rand -hex 16)   # 必填，没有默认值；请保存好（或写进 .env）
docker compose up -d                                   # postgres + api + web(nginx)
docker compose exec api /app/bin/mldojo-api token issue
```

说明：
- 容器内的主密钥存放在 `apidata` 卷的 `/data/master.key` 文件中（容器里没有 keychain），也可以用 `MLDOJO_MASTER_KEY` 注入。
- compose 不包含任何队列插件。需要队列后端时，把插件（sidecar）单独运行起来，然后在 api 容器上设置 `MLDOJO_QUEUE_PLUGINS`（格式 `name=url,name2=url2`）。协议见 [queue-plugins.zh-CN.md](queue-plugins.zh-CN.md)。

## 部署拓扑示例

```
手机/浏览器 ──https──▶ mldojo.example.com (公网机)
                              │ nginx，TLS 由 certbot 签发
                              ▼ 127.0.0.1:18765
                        SSH 反向隧道（API 主机主动拨出，systemd --user 守护）
                              ▲
                    API 主机 mldojo.internal:8765  ← API + PostgreSQL + Web + 本机 agent
                              ▲ agent 反向连接（走内网，不经隧道）
                    其它计算节点
```

## 原生部署（无 root、无 docker）

在 Ubuntu 24.04 上（无需 sudo）：

```bash
mkdir -p ~/ws && cd ~/ws && git clone https://github.com/lovemoon-ai/mldojo.git && cd mldojo
cp deploy/site.env.example deploy/site.env && $EDITOR deploy/site.env
# 需要 PATH 中有 Go 工具链（版本见 go.mod）；go.dev 访问不了时可用 GOPROXY=https://goproxy.cn,direct
# Next 要求 node >= 20.9；系统自带的太旧时，可以从 npmmirror 装一个：
#   curl -sSL -o /tmp/node.tar.xz https://registry.npmmirror.com/-/binary/node/v22.21.1/node-v22.21.1-linux-x64.tar.xz
#   mkdir -p ~/opt/node-v22 && tar -xJf /tmp/node.tar.xz -C ~/opt/node-v22 --strip-components=1
export PATH=/usr/local/go/bin:$HOME/opt/node-v22/bin:$PATH
make build dist GOPROXY=https://goproxy.cn,direct
npm_config_registry=https://registry.npmmirror.com make web
loginctl enable-linger $(id -un)                                # 服务在登出后继续运行
PG_VERSION=16.15.0 deploy/native/install-postgres.sh             # 首次运行
deploy/native/install.sh
```

`deploy/native/install.sh` 会对 `MLDOJO_PLUGIN_DIRS` 里列出的每个目录执行 `<dir>/install.sh`——
私有队列插件自带安装脚本和 systemd unit。没有配置插件时，`/api/v1/health` 里的
`"queue_plugins"` 是 `{}`，这完全正常。

| systemd --user unit | 说明 |
|---|---|
| `mldojo-postgres` | PostgreSQL 16（zonky 二进制，`~/opt/pgsql-16`，数据在 `~/.mldojo/pgdata`，127.0.0.1:55432） |
| `mldojo-api` | `~/.mldojo/app/bin/mldojo-api serve`，读取 `~/.mldojo/config.yaml` 的 `api:` 段 |
| 队列插件的 unit | 由 `MLDOJO_PLUGIN_DIRS` 中各插件目录的 `install.sh` 创建（见 [queue-plugins.zh-CN.md](queue-plugins.zh-CN.md)） |
| `mldojo-agent-<node>` | 由 `mldojo node add` 创建（本机，或开了 linger 的远程节点） |
| `mldojo-tunnel` | SSH 反向隧道，把公网机的 127.0.0.1:18765 接到本机 8765（见下） |
| `mldojo-backup` | 每日备份 timer + oneshot service，由 `deploy/native/install-backup.sh` 创建（见「备份与恢复」） |

升级时重新执行 `make build dist web && deploy/native/install.sh` 即可，已有的 `config.yaml` 不会被覆盖。
agent 二进制在 `mldojo node add`（重新部署）时更新。

`~/.mldojo/config.yaml`：

```yaml
server: http://127.0.0.1:8765      # CLI
token: mld_...                     # CLI
api:
  listen: 0.0.0.0:8765
  database_url: postgres://mldojo:***@127.0.0.1:55432/mldojo?sslmode=disable
  public_url: https://mldojo.example.com
  web_dir: ~/.mldojo/app/web
  agent_dist: ~/.mldojo/app/dist
  queue_plugins:                   # 可选：<插件名>: <sidecar url>
    mock: http://127.0.0.1:8766
  queue_poll_sec: 5                # 队列 run 的轮询间隔，默认 5
```

所有字段都可以用环境变量覆盖：`MLDOJO_LISTEN`、`MLDOJO_DATABASE_URL`、`MLDOJO_PUBLIC_URL`、
`MLDOJO_WEB_DIR`、`MLDOJO_AGENT_DIST`、`MLDOJO_QUEUE_PLUGINS`（`name=url,name2=url2`）、`MLDOJO_API_TOKEN`、
`MLDOJO_MASTER_KEY(_FILE)`、`MLDOJO_NO_KEYCHAIN=1`、`MLDOJO_HOME`。

队列插件的协议、注册方式（`mldojo queue add`）以及用于本地试用的 mock 插件，见 [queue-plugins.zh-CN.md](queue-plugins.zh-CN.md)。

## 用 ~/.mldojo/env 管白名单

`deploy/native/install.sh` **每次部署都会重写** systemd unit，所以直接写进 unit 的
环境变量会在下次部署时丢掉。unit 里带了一行 `EnvironmentFile=-~/.mldojo/env`，
那个文件不会被覆盖，是放这类配置的地方：

```bash
cat > ~/.mldojo/env <<ENV
MLDOJO_SSO_ALLOW=you@example.com
MLDOJO_SSO_ADMINS=you@example.com
ENV
chmod 600 ~/.mldojo/env
systemctl --user restart mldojo-api
```

环境变量**覆盖** config.yaml 里的同名项，所以两边都写只会让人困惑——
建议 config.yaml 里干脆不写，让 env 成为唯一来源。

确认实际生效的值（读运行中进程的环境，而不是读文件）：

```bash
tr '\0' '\n' < /proc/$(systemctl --user show -p MainPID --value mldojo-api)/environ | grep MLDOJO_SSO
```

### 两个行为要知道

- **白名单为空 = SSO 直接停用**（不是「放行所有人」）。env 文件丢了、变量拼错了，
  结果都是没人能用 SSO 登录，而不是谁都能进——失败方向是安全的那一侧。
  这时 API token 依然可用，不会把自己彻底锁在门外。
- **把人移出白名单会立刻断掉他已有的会话**。白名单在每个请求上都会校验，
  不匹配就删掉那条 session。不需要手工去清 `sessions` 表。

单人私有部署就是把两个变量都设成自己一个人：

```bash
MLDOJO_SSO_ALLOW=you@example.com
MLDOJO_SSO_ADMINS=you@example.com
```
## 备份与恢复

没有备份的话，一次磁盘故障 = 全部实验历史永久丢失。
`deploy/native/backup.sh` 把要保的两样东西打成一个带 UTC 时间戳的 bundle（普通 tar）：

| bundle 成员 | 来源 | 说明 |
|---|---|---|
| `db.sql.gz` | `pg_dump --clean --if-exists`，gzip -9 | runs / projects / metrics / secrets / sessions 全量 |
| `data.tar.gz` | data_dir（默认 `~/.mldojo/data`） | `runs/*.log` 日志与 `blobs/` 代码包；**排除 `cache/` 和 `tmp/`**——前者是内容寻址的产物缓存、后者是没写完的上传，两者都由 `api/internal/logstore` 的 `New` 在每次启动时重建，备份它们只会让归档更大更慢 |
| `master.key` | `~/.mldojo/master.key` | **默认不含**，要显式设 `MLDOJO_BACKUP_INCLUDE_KEY=1` 才会打进去。`secrets` 表是 age 加密的，缺这把钥匙恢复出来也解不开；但备份的用途就是被复制到别处，把主密钥放进去等于"拿到这一个文件 = 拿到全部 SSH 私钥和 bucket 凭据"，所以默认让它和备份分开保管 |
| `MANIFEST` + `SHA256SUMS` | 脚本生成 | 元信息与逐成员校验和；**不含连接串或密码** |

bundle 落在 `$MLDOJO_BACKUP_DIR`（默认 `~/.mldojo/backups`，目录 `0700`、文件 `0600`），旁边附一个
`.sha256` 文件。默认保留最近 7 份（`MLDOJO_BACKUP_KEEP`），更早的自动删除。脚本自带锁
（有 `flock` 用 `flock`，否则用 `mkdir`），定时任务叠加时后来的那个直接跳过并 exit 0，
不会两份 dump 互相踩。所有输出里的数据库密码都会被换成 `***`。

### 安装每日定时备份（原生部署）

```bash
deploy/native/install-backup.sh                        # 每天一次，保留 7 份
MLDOJO_BACKUP_KEEP=30 deploy/native/install-backup.sh  # 保留 30 份
```

装完会**立刻跑一次**备份做验证——配错了在这一步就报错退出，而不是等到凌晨三点静默失败。
脚本生成 `mldojo-backup.service`（`Type=oneshot`）+ `mldojo-backup.timer`（`OnCalendar=daily`、
`RandomizedDelaySec=1h`、`Persistent=true`，关机错过的会在开机后补跑）。
`ExecStart` 指向当前 checkout 里的 `deploy/native/backup.sh`，`git pull` 之后的修复自动生效；
**挪了 checkout 路径就要重跑一次 `install-backup.sh`**。

```bash
systemctl --user list-timers mldojo-backup.timer    # 下次什么时候跑
systemctl --user start mldojo-backup.service        # 手动立刻备份一次
journalctl --user -u mldojo-backup.service -n 50    # 上次跑得怎么样
```

和其它 unit 一样，没开 `loginctl enable-linger` 的话登出后 timer 不会跑。

### 手动备份

```bash
deploy/native/backup.sh
```

数据库连接串按 `MLDOJO_DATABASE_URL` → `~/.mldojo/postgres.url` → `config.yaml` 的 `database_url`
顺序解析；data_dir 按 `MLDOJO_DATA_DIR` → `config.yaml` 的 `data_dir` → `~/.mldojo/data` 解析；
`pg_dump` 按 `MLDOJO_PGDUMP` → `PATH` → `~/opt/pgsql-*/bin/pg_dump` 查找
（原生安装的 PostgreSQL 不在 PATH 上）。

### 恢复

**破坏性操作**：会覆盖当前数据库和 data_dir。脚本要求显式确认，非交互环境下不给
`MLDOJO_RESTORE_YES=1` 会直接拒绝执行。

```bash
deploy/native/restore.sh                                     # 不带参数：列出可用 bundle
deploy/native/restore.sh ~/.mldojo/backups/mldojo-backup-20260920T030000Z.tar   # 交互式，要求输入 yes
MLDOJO_RESTORE_YES=1 deploy/native/restore.sh ~/.mldojo/backups/mldojo-backup-*.tar  # 脚本里用
```

做的事情，按顺序：

1. 校验 `.sha256` 旁文件 → 解包 → 校验包内 `SHA256SUMS` → `gzip -t`；任何一步不过就退出，**不碰现有数据**；
2. 打印 MANIFEST 和将被覆盖的目标（数据库连接串已脱敏），然后等确认；
3. `systemctl --user stop mldojo-api`；
4. `psql -v ON_ERROR_STOP=1` 灌入 dump（dump 自带 `DROP ... IF EXISTS`，会先删同名对象）；
5. 现有 data_dir **改名**成 `<data_dir>.pre-restore-<ts>`（不是删除——恢复错了还能退回去），再解包，并补建 `cache/`、`tmp/`；
6. 主密钥：本地没有就写入 `~/.mldojo/master.key`；本地有但和 bundle 里的不一样，则保留本地的、把备份里那份放到 `~/.mldojo/master.key.restored` 并告警；
7. `systemctl --user start mldojo-api`。

没有 `mldojo-api.service` 的环境（compose、macOS）脚本会提示你自己先停 API
（`docker compose stop api`），恢复完再自己起来。

主密钥的解锁顺序是 keychain → `MLDOJO_MASTER_KEY` → key file，**任一把不匹配会自动往下试**
（`api/internal/secrets` 的 `AutoUnlock`），所以换机恢复时把 bundle 里的 `master.key` 放回
`~/.mldojo/master.key` 就够了，旧机器上残留的 keychain 条目不会挡路。全都不匹配时 API 照常启动，
只是 secrets 处于 locked 状态，日志里会有 `secrets: master key rejected` / `secrets: locked`。

### 恢复演练

备份没演练过就等于没有备份。建议每季度在一台**非生产**机器上完整走一遍：

> 必须在**没有跑生产 MLDojo 的机器**上做：`install-postgres.sh` 固定把连接串写到
> `~/.mldojo/postgres.url`（不受 `MLDOJO_HOME` 影响），在生产机上跑会覆盖掉生产的那份。

```bash
# 1. 演练机上装一套独立的 PostgreSQL（换个端口，避免和别的实例撞）
PGPORT=55433 deploy/native/install-postgres.sh

# 2. 把生产的 bundle 拷过来（连 .sha256 一起）
mkdir -p ~/drill/backups && scp 'prod-host:~/.mldojo/backups/mldojo-backup-*.tar*' ~/drill/backups/

# 3. 恢复到演练环境（连接串自动取 ~/.mldojo/postgres.url，即上面新装的那套）
MLDOJO_DATA_DIR=~/drill/data MLDOJO_RESTORE_YES=1 \
  deploy/native/restore.sh ~/drill/backups/mldojo-backup-<ts>.tar

# 4. 起一个临时 API 指向演练库，注意看启动日志
MLDOJO_DATA_DIR=~/drill/data MLDOJO_DATABASE_URL=$(cat ~/.mldojo/postgres.url) \
  MLDOJO_LISTEN=127.0.0.1:8799 mldojo-api serve
```

验收清单，**全部对上**才算演练通过：

| 检查 | 命令 | 期望 |
|---|---|---|
| API 能起来 | `curl -s 127.0.0.1:8799/api/v1/health` | 健康 |
| 主密钥跟着回来了 | 看上一步的启动日志 | **没有** `secrets: master key rejected` 或 `secrets: locked`（最容易在演练里翻车的一项） |
| 元数据完整 | `MLDOJO_SERVER=http://127.0.0.1:8799 mldojo run ls` | 条数与生产一致 |
| 指标没丢 | `mldojo run metrics <run-id>` | 曲线点数与生产一致 |
| data_dir 真的回来了 | `mldojo run logs <run-id>` | 日志非空（验证 `runs/*.log`） |
| secrets 可用 | `mldojo secret ls` | 能列出名字 |
| RTO | 给整个流程掐表 | 记下来，这就是真实的恢复耗时 |

演练完把 `~/drill`、`~/.mldojo/pgdata` 和那个临时 PG 服务一并删掉。

### docker compose 形态

compose 里数据库是 `postgres:16-alpine`（命名卷 `pgdata`），data_dir 在 `apidata` 卷的 `/data`。
宿主机上一般没有 pg_dump，借容器里自带的那个即可：

```bash
cd <repo-dir>          # docker compose exec 需要能读到 docker-compose.yml
export MLDOJO_PGDUMP='docker compose exec -T postgres pg_dump'
export MLDOJO_PSQL='docker compose exec -T postgres psql'
export MLDOJO_DATABASE_URL="postgres://mldojo:$POSTGRES_PASSWORD@127.0.0.1:5432/mldojo?sslmode=disable"
```

data_dir 在命名卷里、宿主机直接读要 root，所以先拷出来再备份：

```bash
docker compose cp api:/data /tmp/mldojo-data
MLDOJO_DATA_DIR=/tmp/mldojo-data deploy/native/backup.sh
rm -rf /tmp/mldojo-data
```

想省掉这次拷贝，就把 `docker-compose.yml` 里的 `apidata:/data` 换成 bind mount（例如 `./data:/data`），
之后 `MLDOJO_DATA_DIR=./data deploy/native/backup.sh` 直接备份。恢复同理：先
`docker compose stop api`，跑 `restore.sh`，再 `docker compose start api`。

### 这套方案还没覆盖的

- **只有全量，没有 PITR**：没开 WAL 归档，恢复点粒度就是上一次备份（默认最坏丢 24 小时）。
  要做到 RPO 分钟级得上 `archive_command` + basebackup。
- **备份默认和数据在同一块盘上**：`~/.mldojo/backups` 就在本机，这只能防误删，防不了磁盘故障。
  至少再加一条异地同步（`rsync` 到别的机器或对象存储）才算真的有备份。
- bundle 里有 API token 和会话，**敏感程度等同于数据库本身**，异地存放前要加密。
  开了 `MLDOJO_BACKUP_INCLUDE_KEY=1` 的话还多一把能解开所有 secrets 的主密钥，
  那就必须加密，且不要和主密钥的另一份副本放在同一个地方。

## 公网访问：SSH 反向隧道 + 公网机 nginx

API 主机在内网、没有公网入口（或者 Tailscale、Cloudflare quick tunnel 这类方案用不了）时，
可以借一台自己的公网机中转：

```bash
# 1. API 主机：生成专用密钥并装好隧道服务
REMOTE_HOST=root@<public-host-ip> REMOTE_PORT=18765 deploy/native/install-tunnel.sh
# 2. 公网机：把上一步打印的那行加进 ~/.ssh/authorized_keys（已限制成只能绑定这一个端口）
#    restrict,port-forwarding,permitlisten="127.0.0.1:18765",command="/usr/bin/false" ssh-ed25519 ...
# 3. API 主机：启动隧道
systemctl --user enable --now mldojo-tunnel
# 4. 公网机：nginx 站点反代到隧道端口（注意 WebSocket 的 Upgrade 头，日志和指标都靠它）
#    server_name mldojo.example.com; proxy_pass http://127.0.0.1:18765;
# 5. DNS 加 A 记录指向公网机，解析生效后签证书：
certbot --nginx -d mldojo.example.com --non-interactive --redirect --keep-until-expiring
```

特点：API 主机只有出站连接，本机不对公网开任何端口；agent 仍然走内网直连 API，不经过隧道。
认证仍然只有 MLDojo 自己的 token，公网暴露后建议定期 `mldojo-api token rotate`。

## 登录：Conductor SSO

浏览器走 Conductor 单点登录，CLI 和 agent 继续用 API token，两条路同时有效。

```yaml
# ~/.mldojo/config.yaml 的 api: 段
api:
  web_url: https://mldojo.example.com             # 公网入口；反代会隐藏真实 Host，所有跳转都基于它拼
  sso:
    base_url: https://conductor.example.com
    client_id: mldojo
    client_secret: <random-32-bytes>              # 或 MLDOJO_SSO_CLIENT_SECRET
    # allow / admins 建议放 ~/.mldojo/env（见上），不要写在这里：
    # install.sh 每次都会重写 systemd unit，但不会碰那个文件。
    allow: [you@example.com]                      # 必填白名单（id/email/phone/name）
    admins: [you@example.com]                     # allow 的子集，拿 admin 角色
                                                  # 不在 admins 里的登录用户是 member：能看全部、能提交 run，
                                                  # 但只能删自己的，且不能动 node / queue / secret（见 docs/api.zh-CN.md「权限」）
```

**`allow` 留空 = SSO 被禁用**（`Enabled() = Configured() && len(Allow) > 0`）。空白名单以前等于放行
所有 Conductor 用户，对公网部署不是个安全的默认值。只配 OAuth 四项而不设 `allow` 时，登录页上的
SSO 入口会直接消失、只剩 token 登录，启动日志里是
`Conductor SSO is configured but disabled: sso.allow is empty`。
排查见 [troubleshooting.zh-CN.md](troubleshooting.zh-CN.md#症状sso-配好了但登录页上根本没有-sso-按钮)。

流程（`api/internal/auth`）：

| 端点 | 说明 |
|---|---|
| `GET /api/v1/auth/config` | 免认证，登录页据此决定是否显示 SSO 按钮 |
| `GET /api/v1/auth/login?next=/x` | 302 到 `<base>/oauth/authorize`，state 存在 10 分钟的 HttpOnly cookie 里 |
| `GET /api/v1/auth/callback` | 校验 state → 用 code 换身份 → 建会话 → 跳回 `next` |
| `GET /api/v1/auth/me` | 当前身份，`mode` 为 `sso` 或 `token` |
| `POST /api/v1/auth/logout` | 注销 |

会话存在 `sessions` 表，cookie 是 HttpOnly + Secure + SameSite=Lax，有效期 30 天；表里存的是 cookie 的
sha256，库被读走也换不出会话。Conductor 的 access_token **不保存**——MLDojo 不代表用户调 Conductor 的接口，
没有需要落盘的东西。登录者会记录到 `runs.metadata.submitter` 和 `projects.owner`（只记录、不做鉴权）。

在 Conductor 侧注册：把一条
`{"client_id":"mldojo","display_name":"MLDojo","client_secret":"…","redirect_uris":["https://mldojo.example.com/api/v1/auth/callback"]}`
加进 `CONDUCTOR_SSO_CLIENTS_JSON`（**必须单行**），然后重启 Conductor web 进程使其重新加载。几个坑：

- `redirect_uri` 三处必须逐字节一致（authorize 链接、注册表、token 请求），差一个斜杠就是 `invalid_grant`。
- `state` 必填且非空，否则 Conductor 的授权页直接报错且不会跳回来。
- 不用 OIDC 库：没有 discovery、没有 JWKS、没有 PKCE，access_token 是不透明字符串、没有过期时间。
- 不带浏览器的验证方式：用真 secret 加一个假 code 打 token 端点，返回 `invalid_grant` 说明
  client_id/secret/redirect_uri 都对了，返回 `invalid_client` 说明还没注册或没重启。

## 可观测性：metrics、告警与日志格式

### Prometheus 抓取

`GET /api/v1/metrics` 输出标准文本格式（指标清单见 [api.zh-CN.md](api.zh-CN.md#可观测性get-apiv1metrics)）。
**默认要 admin token**，因为它会暴露项目规模、节点数和全部路由。抓取端一般不带 token，两种配法：

```yaml
# ~/.mldojo/config.yaml 的 api: 段
api:
  metrics_public: true   # 或 MLDOJO_METRICS_PUBLIC=1；默认 false
```

```yaml
# 方式一（推荐）：保持 metrics_public: false，让 Prometheus 带 token
scrape_configs:
  - job_name: mldojo
    metrics_path: /api/v1/metrics
    static_configs: [{targets: ["mldojo.internal:8765"]}]
    authorization: {type: Bearer, credentials: "mld_..."}   # `mldojo-api token create --name prometheus`
```

```yaml
# 方式二：metrics_public: true，但只让内网或反代能碰到这个路径
#   nginx: location = /api/v1/metrics { allow 10.0.0.0/8; deny all; proxy_pass ...; }
```

公网实例（例如 `mldojo.example.com` 走 SSH 反向隧道 + nginx）**不要**直接开
`metrics_public`，除非在 nginx 里限死来源。

抓取代价：run/node 计数带 15 秒缓存，`data_dir` 占用由后台每 5 分钟采样，
所以抓取本身不会打库、不会走盘。

### 告警 webhook

事件源在代码里的四个位置：run 转 `failed`（`backends.SetStatus`）、agent 断连
（`backends/hub.go`）、`data_dir` 超限、secrets 锁定超过 1 小时（后者两项在 `watchPlatform`）。
出口只有一个：POST 一段 JSON 到 webhook。

```yaml
api:
  alerts:
    webhook_url: https://open.feishu.cn/open-apis/bot/v2/hook/xxxx  # 或 MLDOJO_ALERTS_WEBHOOK_URL
    on: [run.failed, node.offline, disk.high, secrets.locked]       # 留空 = 全订阅；或 MLDOJO_ALERTS_ON（逗号分隔）
    min_interval_sec: 600                                           # 同一 key 的去重窗口，默认 600
    disk_limit_gb: 200                                              # data_dir 超过就报 disk.high；0 = 不检查
```

| 事件 | 触发 | 去重 key |
|---|---|---|
| `run.failed` | run 进入 failed | 每个 run id 一次 |
| `node.offline` | agent 连接断开 | 每个 node id 一次 |
| `disk.high` | `data_dir` > `disk_limit_gb` | 全局一个 |
| `secrets.locked` | secrets 连续锁定 > 1h | 全局一个 |

payload 一份 JSON 同时喂三种接收端：飞书自定义机器人读 `msg_type` / `content.text`，
Slack incoming webhook 读 `text`，其它系统读 `event` / `level` / `title` / `fields` / `instance`。

```json
{"msg_type":"text","content":{"text":"[MLDojo] node offline: gpu-a\n..."},
 "text":"[MLDojo] node offline: gpu-a\n...","source":"mldojo",
 "instance":"https://mldojo.example.com","event":"node.offline","level":"warning",
 "title":"node offline: gpu-a","fields":{"node":"gpu-a","agent_version":"0.1.0-dev"},
 "time":"2026-09-20T10:12:07+08:00"}
```

几条硬约束，配之前先知道：

- **不会拖垮主流程**：`Notify` 只往一个 64 长的 channel 里塞，满了就丢；真正的 POST 在
  单独 goroutine 里做，带 10 秒超时，失败只写一行 `WARN alert webhook` 的日志。
- **不会刷屏**：除了每个 key 的 `min_interval_sec`，还有一个全局令牌桶（一次最多 20 条、
  之后每分钟补 1 条）。一个节点挂掉导致 200 个 run 同时失败时，出去的是 20 条而不是 200 条。
  被压下的次数记在 `mldojo_alerts_total{outcome="suppressed"}`。
- **不会漏密**：payload 里只放标识符（run/project/experiment/node/target/exit_code），
  再过一层正则把 `mld_*` token、`Bearer *`、`postgres://...`、`token=`/`password=`/`api_key=`
  以及 32 位以上的 hex（agent token hash）替换成 `[redacted]`，并把每段截到 300 字节。
  **绝不要把 secret 值、env 或完整命令行加进 `Fields`。**
- 想验证配置对不对：`mldojo-api serve` 的启动日志里有 `alerts=true`；
  抓一次 `/api/v1/metrics` 看 `mldojo_alerts_total{event=...,outcome="sent"}` 有没有涨。

### 结构化日志

```bash
MLDOJO_LOG_FORMAT=json    # 每行一个 JSON 对象，可直接喂 Loki/ES；默认 text 不变
MLDOJO_LOG_LEVEL=debug    # 默认 info
```

api 和 agent 两个二进制都认这两个变量（`internal/logging`）。systemd 里加：

```ini
# ~/.config/systemd/user/mldojo-api.service（install.sh 生成）里加一行，然后
# systemctl --user daemon-reload && systemctl --user restart mldojo-api
Environment=MLDOJO_LOG_FORMAT=json
```

访问日志每行带 `id`（= 响应头 `X-Request-Id`），和审计记录里的 `request_id` 对得上。

## Agent 链路的 TLS

公网访问那条链路由公网机上的 nginx 终止 TLS，但 **agent ↔ API 走的是内网明文 HTTP/WS** ——
node token 和全部日志都是裸的。给 API 自己配上证书就能关掉这个口子：

```yaml
api:
  tls_cert: ~/.mldojo/tls/server.crt     # 或 MLDOJO_TLS_CERT
  tls_key:  ~/.mldojo/tls/server.key     # 或 MLDOJO_TLS_KEY
  tls_client_ca: ~/.mldojo/tls/ca.crt    # 可选：要求客户端证书（mTLS），或 MLDOJO_TLS_CLIENT_CA
```

- `tls_cert` 与 `tls_key` **必须成对**；只配一个会在启动时报错退出，而不是悄悄降级成明文。
- 配了 `tls_client_ca` 但没配服务端证书同样直接报错——mTLS 不可能脱离服务端 TLS 存在。
- 证书解析失败在启动时就暴露，不会等到第一个请求。

节点侧对应地告诉 agent 该信任什么（`~/.mldojo/agent/<node>.yaml`）：

```yaml
server_url: https://mldojo.internal:8765   # https 会自动让 WebSocket 走 wss
ca_file: ~/.mldojo/tls/ca.crt          # 自签名或内部 CA 时必需
client_cert: ~/.mldojo/tls/agent.crt   # 仅当服务端开了 mTLS
client_key:  ~/.mldojo/tls/agent.key
```

`ca_file` 是**追加**到系统根证书上的，所以内部 CA 和公网证书可以混用。
`client_cert` 与 `client_key` 也必须成对。

自签名证书（内网够用）：

```bash
mkdir -p ~/.mldojo/tls && cd ~/.mldojo/tls
openssl req -x509 -newkey rsa:2048 -nodes -days 3650 \
  -keyout server.key -out server.crt \
  -subj "/CN=mldojo" -addext "subjectAltName=DNS:mldojo.internal,DNS:localhost"
chmod 600 server.key
```

把 `server.crt` 同时当作各节点的 `ca_file` 即可（自签名证书就是自己的 CA）。


## 节点

```bash
mldojo node add --id local --local
mldojo node add --id gpu-a --ssh alice@192.0.2.11 --labels 5090
mldojo secret set ssh_keys/id_ed25519 --from-file ~/.ssh/id_ed25519
mldojo node add --id gpu-b --ssh root@203.0.113.10 --port 39670 --identity secret://ssh_keys/id_ed25519 \
    --reverse-tunnel --labels h20            # 公网节点连不到内网 API 时用反向隧道
mldojo node add --id bastion-gpu-1 --ssh "alice@alice@192.0.2.20@bastion.example.com" --port 2222 \
    --identity secret://ssh_keys/id_ed25519 --via gpu-a --via local --reverse-tunnel --labels 5090,8gpu
```

加上 `--dry-run` 只测连通性（路由、系统、GPU），不做任何部署。节点配置了 `--reverse-tunnel` 时，dry-run 还会检查
SSH 服务端是否允许远程转发。`mldojo node upgrade <id>` 会原地升级 agent，正在跑的 run 会被新 agent 接着接管。

注意：有些 JumpServer 类堡垒机会**静默丢弃**包含 `rm -f` 的 exec 命令（返回空输出、exit 0）。所以部署脚本不用 `rm`，
并且会校验每一步是否真的生效（二进制 checksum、启动和停止脚本的完成标记）。
