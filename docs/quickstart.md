English | [简体中文](quickstart.zh-CN.md)

# Quickstart: from zero to your first run

Goal: bring up MLDojo on a clean machine, attach one compute node, run `recipes/examples/hello/`,
and see its logs, metrics and artifacts in both the CLI and the Web UI. **About 10 minutes end to end**
(not counting image builds).

Every step shows the **expected output**. If yours does not match, go to
[docs/troubleshooting.md](troubleshooting.md), which is indexed by symptom.

---

## 0. Pick a path

MLDojo has two layers that can live on different machines:

- **Control plane**: `mldojo-api` (Go) + PostgreSQL + Web. Needs only a container runtime or the ability to run a Go binary.
- **Compute nodes**: machines running `mldojo-agent`; training actually runs here. **They need `python3`** (the hello recipe runs `python3 train.py`).

| Your situation | Path |
|---|---|
| You want to see something fast and have docker | **A: docker-compose** (this guide) |
| Training runs on this same machine, no root / no docker | **B: native deploy** → [docs/deploy.md](deploy.md) ("Native deploy") |
| You are changing code in the repo and want hot reload | `mldojo dev up` (runs postgres + API + Next.js dev server in the foreground; needs a checkout, go, npm, docker) |

> **The key difference between A and B is the "add a node" step**: with a native deploy the API runs
> on the machine you want to train on, so a single `mldojo node add --local` is enough; in compose mode
> the API runs in a container and `--local` means **the container itself**. See [step 4](#4-attach-your-first-compute-node).

---

## 1. Start the control plane (docker-compose)

`MLDOJO_PUBLIC_URL` is **the address agents on the nodes use to connect back to the API**, so it cannot be
`localhost`; use an address the nodes can reach. For a single-machine test, this machine's LAN IP is fine.

```bash
cd <repo-dir>
export MLDOJO_PUBLIC_URL=http://<LAN-IP>:8765
export POSTGRES_PASSWORD=$(openssl rand -hex 16)   # required; or put both in .env (see .env.example)
docker compose up -d
```

The first run builds two images (Go + Next.js), which can take a few minutes. Expected:

```
[+] Running 4/4
 ✔ Network mldojo_default       Created
 ✔ Container mldojo-postgres-1  Healthy
 ✔ Container mldojo-api-1       Started
 ✔ Container mldojo-web-1       Started
```

Check that the API is alive (this endpoint **does not need a token**):

```bash
curl -s http://localhost:8765/api/v1/health
```

```json
{"ok":true,"version":"94ddcb5","db":"ok","secrets":"unlocked","agents_online":0,"queue_plugins":{}}
```

What to look for:
- `"ok":true` and `"db":"ok"` → the control plane is up. If `db` is not `ok`, the database is not connected.
- `"agents_online":0` is expected; no nodes have been added yet.
- `"queue_plugins":{}` is expected; it means no queue plugins are configured (hello does not need one).

## 2. Get an API token

Tokens are stored in the database and issued by the API itself. `token issue` is idempotent: if a token already exists, it prints that one.

```bash
docker compose exec api /app/bin/mldojo-api token issue
```

```
mld_7Qk2xR9vNbF3hJ8pLwYz0aCdEsTuViMn
```

> To rotate, use `mldojo-api token rotate` (the old token stops working immediately; every node's agent needs `mldojo node upgrade` to pick up the new token).

## 3. Install the CLI and log in

The CLI is a single static binary. The easiest way is to copy it out of the container (`dist/mldojo-linux-amd64` is the Linux CLI):

```bash
docker compose cp api:/app/dist/mldojo-linux-amd64 ./mldojo   # Linux
chmod +x ./mldojo && sudo mv ./mldojo /usr/local/bin/mldojo
```

On macOS, or to build it yourself: run `make build`; the binary is at `bin/mldojo`.

```bash
mldojo login --server http://localhost:8765 --token mld_7Qk2xR9vNbF3hJ8pLwYz0aCdEsTuViMn
```

```
logged in to http://localhost:8765 (0 projects); saved to /home/you/.mldojo/config.yaml
```

`(0 projects)` shows the server actually accepted the token: `login` first calls `/projects` with it
and does not save it to the config if that fails.

This writes `~/.mldojo/config.yaml` (`0600`). After that, no command needs `--server/--token`.
Verify:

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

> With no server configured you get `no server configured: run `mldojo login --server URL --token TOKEN` or set MLDOJO_SERVER` and exit code 2.

## 4. Attach your first compute node

**The node must have `python3`**, otherwise the hello recipe in step 5 fails with exit 97.

### 4a. Native deploy: `--local`

When the API runs directly on this machine, one command is enough (`--local` = the host the API runs on):

```bash
mldojo node add --id local --local --labels dev
```

```
deploying agent to local (up to ~60s)...
node local online: -, 32 CPU, 124 GiB RAM (agent 94ddcb5)
```

`-` is the GPU summary and means no GPU was detected; with GPUs it looks like `1x RTX 5090`.

### 4b. docker-compose: attach the host as an SSH node

In compose mode, `--local` installs the agent into the **api container**, whose image is `debian:bookworm-slim`
with **only ca-certificates and git: no python3 and no systemd**. The hello recipe cannot run there.
Instead, attach the **host** (or any machine with python3) as an SSH node.

The API runs in a container and cannot read the private key under your `~/.ssh`, so first store the key as a secret and reference it via `secret://`:

```bash
mldojo secret set ssh_keys/mykey --from-file ~/.ssh/id_ed25519
```

```
stored secret://ssh_keys/mykey (464 bytes)
```

Use `--dry-run` first to test connectivity only, **without deploying anything**:

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

If the `python` line shows a version, step 5 will work. Once confirmed, drop `--dry-run` to deploy for real:

```bash
mldojo node add --id host --ssh $USER@<LAN-IP> --identity secret://ssh_keys/mykey --labels dev
```

```
deploying agent to host (up to ~60s)...
node host online: -, 32 CPU, 124 GiB RAM (agent 94ddcb5)
```

> `node add` is **all or nothing**: if any step fails, nothing is left in the database,
> so just fix the command and retry; there is no need to `node rm` first.

### Verify the node is online

```bash
mldojo node ls
```

```
ID    AGENT   GPUS  UTIL  ACTIVE  LABELS  HEARTBEAT
host  online  -     -     0       dev     3s ago
```

`AGENT` must be `online`, and `HEARTBEAT` should be within a few seconds (the agent sends a heartbeat every 5s).
If it is not online, see troubleshooting.md ("Symptom: agent stays offline / `node add` fails").

## 5. Submit your first run

The bundled hello recipe needs no GPU. It uses only `python3` to run 20 steps of fake training, writing jsonl metrics and a fake
checkpoint. Take a look at it first:

```bash
cat recipes/examples/hello/recipe.yaml
```

Its `params.seed` is a list `[0, 1]`. **Without `--matrix`, list parameters take only their first value**;
with `--matrix seed` it expands into 2 runs. Start with a single run:

```bash
mldojo run submit -f recipes/examples/hello/recipe.yaml --target node:host --wait
```

```
submitted 1 run(s): a3f1c2d8
ID        EXPERIMENT          NAME         TARGET      STATUS     EXIT  CREATED  DURATION
a3f1c2d8  smoke/hello-world   hello-world  node:host   succeeded  0     8s ago   6s
```

`STATUS` `succeeded` with `EXIT` `0` means it worked.

- `--wait` waits for all runs to finish; **if any run did not succeed, the CLI exits with code 4** (handy in CI).
- To watch logs while it runs, use `--follow` (for a single run it streams stdout+stderr+system directly).
- The target format is `node:<id>` or `queue:<plugin>/<queue>`.

If the status stays at `queued` or `starting`, see
troubleshooting.md ("Symptom: run stuck in queued").

## 6. View logs, metrics and artifacts

Take the run id from the previous step (a unique prefix works; 8 characters is plenty) as `$RUN`:

```bash
RUN=a3f1c2d8
```

**Logs** (stdout only by default; `--stream all` merges stdout/stderr/platform events, with system lines prefixed by `[mldojo]`):

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

While the run is in progress you can follow it to the end with `-f` (note that `-f` means `--follow` in `run logs`,
but `--file` in `run submit`).

**Metrics**. hello uses two paths: jsonl file scanning (`loss`, `lr`) and the Python SDK (`sdk/acc`); each should have 20 points:

```bash
mldojo run metrics $RUN
```

```
KEY      POINTS  LAST STEP  LAST      MIN       MAX
loss     20      20         0.144296  0.135335  0.914935
lr       20      20         0.001     0.001     0.001
sdk/acc  20      20         0.855704  0.085065  0.864665
```

For the full curve of one key, use `--key`:

```bash
mldojo run metrics $RUN --key loss
```

If the table is empty, see troubleshooting.md ("Symptom: logs not updating / metrics missing").

**Artifacts**:

```bash
mldojo run artifacts ls $RUN
```

```
KIND  SIZE  URI
ckpt  16 B  file:///home/you/.mldojo/runs/host/a3f1c2d8-.../outputs/model_step20.ckpt
```

Download to your machine (relayed through the agent; you do not need a direct connection to the node):

```bash
mldojo run artifacts get $RUN <URI-from-above> --dest /tmp
```

```
saved /tmp/model_step20.ckpt (16 B)
```

**Everything at once** (status, progress, latest metrics, anomalies, best checkpoint):

```bash
mldojo ai brief $RUN
```

## 7. Run a parameter matrix

This is what recipes are for: one submission, many runs.

```bash
mldojo run submit -f recipes/examples/hello/recipe.yaml --target node:host --matrix seed --wait
```

```
submitted 2 run(s): 7b3e91a4 c05d2f68
```

The two runs differ only in `seed`. Compare them:

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

`--matrix all` expands every list parameter; `--param k=v` overrides a parameter for this submission (values are parsed as YAML,
so `--param seed=[0,1,2]` is a list and `--param steps=100` is an integer).

## 8. Open the Web UI

```
http://localhost:3000
```

On first visit it asks you to paste the token (the one from step 2); after that it is stored in the browser. You should see:

- Home: projects / experiments / runs, with the runs you just submitted at the top
- Run page: live-updating logs (WebSocket), metric charts, artifacts, GPU usage
- `/nodes`: nodes and live GPU status

The Web UI is a pure static export + PWA, so you can install it on your phone's home screen.
If it does not load or keeps returning 401, see troubleshooting.md ("Symptom: Web UI does not load / keeps returning 401").

---

## Common detours

**No docker / cannot install docker** → use the native deploy, no root needed at any point:
[docs/deploy.md](deploy.md) ("Native deploy").
Requires Go ≥ 1.26 and Node ≥ 20.9; PostgreSQL is installed by `deploy/native/install-postgres.sh` as a
systemd --user service (port 55432), so no system-wide postgres is needed.

**Dependencies are slow to download from mainland China**:

```bash
make build dist GOPROXY=https://goproxy.cn,direct
npm_config_registry=https://registry.npmmirror.com make web
```

**No GPU**: the hello recipe's `resources.default.gpus` is `0`, so it runs on CPU-only machines.
When a real recipe needs GPUs, `run submit` checks node capacity at submit time and rejects it
(e.g. asking for 4 GPUs on a 1-GPU node exits with code 2).

**Only one machine, but you want to try an SSH node**: `--ssh $USER@127.0.0.1` is valid;
that is exactly what the e2e test does, as long as you can `ssh $USER@127.0.0.1` without a password.

**Try a queue (optional)**: the repo ships a mock queue plugin that runs jobs as local subprocesses:

```bash
python3 adapters/queue_sidecar/mock/server.py --host 127.0.0.1 --port 8766
```

Restart the API with `MLDOJO_QUEUE_PLUGINS=mock=http://127.0.0.1:8766`, then run
`mldojo queue add --id mock/default --backend mock` and submit with `--target queue:mock/default`.
For the protocol and writing your own plugin, see [docs/queue-plugins.md](queue-plugins.md).

**Want to confirm the whole stack works on this machine first**: just run `scripts/e2e.sh`.
It starts a fully isolated instance (its own ports, database, agent), covers CLI/API/agent/SSH/ProxyJump/
datasets/metrics/artifacts/cancel/restart-resume/Web routes, and cleans up afterwards. Needs Postgres + SSH to itself + systemd.

---

## Next steps

**Write your own recipe** → [docs/recipes.md](recipes.md). Starting from hello is fastest:
switch `code.source` to `git` (the CLI automatically includes the HEAD commit and uncommitted changes, so runs are reproducible),
switch `env.default.type` to `venv` or `conda`, and replace `run.cmd` with your training command.
Metrics are collected by file scanning by default (jsonl / tensorboard), with no code changes;
to report explicitly, use `import mldojo; mldojo.log({"loss": x}, step=i)`.
If you already use wandb, set `run.wandb: shim` and it runs unchanged.

**Add more nodes** → [docs/deploy.md](deploy.md) ("Nodes").
Supports ProxyJump (`--via a --via b`, tried in order), `@`-nested usernames for multi-hop bastions,
and `--reverse-tunnel` when the node cannot reach the API. Always `--dry-run` first.

**Use the API / write automation** → [docs/api.md](api.md) (~50 endpoints, including WebSocket frame formats and
the error code ↔ HTTP ↔ exit code table), [docs/ai-endpoints.md](ai-endpoints.md) (compact endpoints for agents).
Every CLI command supports `--json`, and exit codes are stable (0 success / 2 user error / 3 unreachable / 4 backend or run failure / 5 conflict),
so you can check the exit code if you do not want to parse JSON.

**Public access and SSO login** → [docs/deploy.md](deploy.md) ("Public access").

**Something went wrong** → [docs/troubleshooting.md](troubleshooting.md).
