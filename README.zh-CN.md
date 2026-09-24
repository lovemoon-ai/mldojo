[English](README.md) | 简体中文

# MLDojo

面向模型训练与算法迭代的实验管理与调度平台。
平台不绑定训练框架，也不自研调度器，只负责训练本身的这些环节：**组织好、跑起来、记录下来、可回溯、可分享、可远程访问**。
**CLI 与 Web 等价**，人和 AI 都能用。

```
Project → Experiment → Run            node:local / node:gpu-a / node:bastion-gpu-1 (ProxyJump)
   │         (recipe)    (logs/code/metrics/artifacts/videos)     queue:<plugin>/<queue>  (queue plugin sidecar)
   └── mldojo CLI (--json, stable exit codes) ═══ REST+WS API (Go) ═══ Web (Next.js PWA)
                                                 ▲ reverse connection (agent → server)
                                           mldojo-agent (one per node)
```

## 组件

| 目录 | 内容 |
|---|---|
| `api/` | `mldojo-api`，Go 实现：PostgreSQL 控制面、REST/WS、secrets（age + keychain）、agent hub、NodeBackend/QueueBackend、数据集、SSO、`/ai/*` |
| `agent/` | `mldojo-agent`：反向 WebSocket 连接、run 执行（none/venv/conda/docker）、GPU 采集、日志/指标/产物 |
| `cli/` | `mldojo` CLI（cobra） |
| `web/` | Next.js 静态导出 + PWA，由 `mldojo-api` 或 nginx 提供 |
| `adapters/` | `node_local`（本机执行 + agent 部署）、`node_ssh`（SSH/ProxyJump/多 via 回退）、`queue_sidecar`（队列插件的 Go 客户端 + 参考 mock 插件） |
| `recipes/` | recipe 解析、校验、overrides、参数矩阵，以及 JSON Schema 和示例 |
| `datasets/` | 数据集引用、location 语法、节点缓存、同步源选择 |
| `proto/` | agent ↔ api 与 REST 共用的 JSON schema（Go 类型） |
| `sdk/python/` | `mldojo.log()` SDK 和 wandb 兼容 shim |
| `deploy/` | docker-compose、原生 systemd --user 安装脚本、公网 SSH 反向隧道 |

## 快速开始

下面是已经熟悉环境时的速查。**第一次用**请走 [docs/quickstart.zh-CN.md](docs/quickstart.zh-CN.md)：
从零到跑通第一个 run 的完整路径，每步都给了期望输出。卡住了看
[docs/troubleshooting.zh-CN.md](docs/troubleshooting.zh-CN.md)（按症状索引）。

**docker-compose（推荐）**
```bash
export POSTGRES_PASSWORD=$(openssl rand -hex 16)   # 或写进 .env（见 .env.example）
MLDOJO_PUBLIC_URL=http://<server-ip>:8765 docker compose up -d
docker compose exec api /app/bin/mldojo-api token issue     # API token
# Web: http://localhost:3000   API/agent: :8765
```

**原生部署（无 root、无 docker）**
```bash
cp deploy/site.env.example deploy/site.env   # set MLDOJO_PUBLIC_URL etc.
make build dist web                          # optional mirrors: GOPROXY=https://goproxy.cn,direct npm_config_registry=https://registry.npmmirror.com
deploy/native/install-postgres.sh            # PostgreSQL 16 (systemd --user, port 55432)
deploy/native/install.sh
```

站点相关的设置（对外地址、内网地址、CLI 默认 server、队列插件目录）都放在不入库的 `deploy/site.env` 里，
见 [docs/deploy.zh-CN.md](docs/deploy.zh-CN.md)。

**使用**
```bash
mldojo login --server http://<server-ip>:8765 --token <token>
mldojo node add --id local --local
mldojo node add --id gpu-a --ssh alice@192.0.2.11 --labels 5090
mldojo secret set ssh_keys/id_ed25519 --from-file ~/.ssh/id_ed25519
mldojo node add --id bastion-gpu-1 --ssh "alice@alice@192.0.2.20@bastion.example.com" --port 2222 \
    --identity secret://ssh_keys/id_ed25519 --via gpu-a --via local --labels 5090,8gpu --dry-run

mldojo run submit -f recipes/examples/hello/recipe.yaml --target node:gpu-a --matrix seed --wait
mldojo run logs <run> -f --stream all
mldojo run metrics <run>
mldojo ai brief <run>
```

更多：[docs/quickstart.zh-CN.md](docs/quickstart.zh-CN.md) · [docs/troubleshooting.zh-CN.md](docs/troubleshooting.zh-CN.md) ·
[docs/deploy.zh-CN.md](docs/deploy.zh-CN.md) · [docs/recipes.zh-CN.md](docs/recipes.zh-CN.md) · [docs/queue-plugins.zh-CN.md](docs/queue-plugins.zh-CN.md) · [docs/api.zh-CN.md](docs/api.zh-CN.md) ·
[docs/ai-endpoints.zh-CN.md](docs/ai-endpoints.zh-CN.md) · [docs/architecture.zh-CN.md](docs/architecture.zh-CN.md) ·
AI agent 用的 skill：[skills/mldojo/SKILL.zh-CN.md](skills/mldojo/SKILL.zh-CN.md)

## 开发与测试

- `make test`：单元测试；`make test-sidecar`：mock 队列插件的测试
- `scripts/e2e.sh`：在当前主机起一个隔离实例（独立端口、数据库和 agent），覆盖 CLI/API/agent/SSH/ProxyJump/反向隧道/数据集/mock 队列插件/wandb shim/隧道/Web 路由，结束后自动清理
- `mldojo dev up`：本地前台启动 postgres（docker）、api 和 web dev server

贡献方式见 [CONTRIBUTING.md](CONTRIBUTING.md)，安全问题请按 [SECURITY.md](SECURITY.md) 私下报告。

## 许可证

[MIT](LICENSE) © lovemoon-ai
