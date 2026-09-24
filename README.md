English | [简体中文](README.zh-CN.md)

# MLDojo

An experiment management and dispatch platform for model training and algorithm iteration.
It doesn't tie you to a training framework and doesn't build its own scheduler. It handles the training loop
itself: **keep runs organized, get them running, record them, and make them reproducible, shareable and remotely
accessible**. **The CLI and the Web UI are equivalent**, so both people and AI agents can use it.

```
Project → Experiment → Run            node:local / node:gpu-a / node:bastion-gpu-1 (ProxyJump)
   │         (recipe)    (logs/code/metrics/artifacts/videos)     queue:<plugin>/<queue>  (queue plugin sidecar)
   └── mldojo CLI (--json, stable exit codes) ═══ REST+WS API (Go) ═══ Web (Next.js PWA)
                                                 ▲ reverse connection (agent → server)
                                           mldojo-agent (one per node)
```

## Components

| Directory | Contents |
|---|---|
| `api/` | `mldojo-api` in Go: PostgreSQL control plane, REST/WS, secrets (age + keychain), agent hub, NodeBackend/QueueBackend, datasets, SSO, `/ai/*` |
| `agent/` | `mldojo-agent`: reverse WebSocket connection, run execution (none/venv/conda/docker), GPU stats, logs/metrics/artifacts |
| `cli/` | `mldojo` CLI (cobra) |
| `web/` | Next.js static export + PWA, served by `mldojo-api` or nginx |
| `adapters/` | `node_local` (local execution + agent deploy), `node_ssh` (SSH/ProxyJump/multi-`via` fallback), `queue_sidecar` (Go client for queue plugins + reference mock plugin) |
| `recipes/` | Recipe parsing, validation, overrides, parameter matrices, plus the JSON Schema and examples |
| `datasets/` | Dataset references, location syntax, node caches, sync source selection |
| `proto/` | JSON schema shared by agent ↔ api and REST (Go types) |
| `sdk/python/` | `mldojo.log()` SDK and a wandb-compatible shim |
| `deploy/` | docker-compose, native `systemd --user` install scripts, public SSH reverse tunnel |

## Quick start

This is a cheat sheet for when you already know your way around. **First time?** Follow
[docs/quickstart.md](docs/quickstart.md): the full path from zero to your first run, with the expected output of
every step. If you get stuck, see [docs/troubleshooting.md](docs/troubleshooting.md) (indexed by symptom).

**docker-compose (recommended)**
```bash
export POSTGRES_PASSWORD=$(openssl rand -hex 16)   # or put it in .env (see .env.example)
MLDOJO_PUBLIC_URL=http://<server-ip>:8765 docker compose up -d
docker compose exec api /app/bin/mldojo-api token issue     # API token
# Web: http://localhost:3000   API/agent: :8765
```

**Native (no root, no docker)**
```bash
cp deploy/site.env.example deploy/site.env   # set MLDOJO_PUBLIC_URL etc.
make build dist web                          # optional mirrors: GOPROXY=https://goproxy.cn,direct npm_config_registry=https://registry.npmmirror.com
deploy/native/install-postgres.sh            # PostgreSQL 16 (systemd --user, port 55432)
deploy/native/install.sh
```

Site-specific settings (public URL, intranet URL, the CLI's default server, queue plugin dirs) live in the
gitignored `deploy/site.env`; see [docs/deploy.md](docs/deploy.md).

**Use**
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

More: [docs/quickstart.md](docs/quickstart.md) · [docs/troubleshooting.md](docs/troubleshooting.md) ·
[docs/deploy.md](docs/deploy.md) · [docs/recipes.md](docs/recipes.md) · [docs/queue-plugins.md](docs/queue-plugins.md) · [docs/api.md](docs/api.md) ·
[docs/ai-endpoints.md](docs/ai-endpoints.md) · [docs/architecture.md](docs/architecture.md) ·
skill for AI agents: [skills/mldojo/SKILL.md](skills/mldojo/SKILL.md)

## Development and testing

- `make test`: unit tests; `make test-sidecar`: tests for the mock queue plugin
- `scripts/e2e.sh`: starts an isolated instance on the current host (its own ports, database and agent) covering
  CLI/API/agent/SSH/ProxyJump/reverse tunnel/datasets/mock queue plugin/wandb shim/tunnel/Web routes, and cleans
  up afterwards
- `mldojo dev up`: runs postgres (docker), the api and the web dev server locally in the foreground

See [CONTRIBUTING.md](CONTRIBUTING.md) to contribute, and report security issues privately as described in
[SECURITY.md](SECURITY.md).

## License

[MIT](LICENSE) © lovemoon-ai
