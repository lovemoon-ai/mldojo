English | [简体中文](deploy.zh-CN.md)

# Deployment

## Site settings: deploy/site.env

Site-specific settings live in `deploy/site.env`. The file is gitignored and never committed; create it from the template and edit it:

```bash
cp deploy/site.env.example deploy/site.env
```

| Key | Meaning |
|---|---|
| `MLDOJO_PUBLIC_URL` | URL that agents and users use to reach the API, e.g. `https://mldojo.example.com` |
| `MLDOJO_INTRANET_URL` | Optional second address for nodes that cannot reach the public one, e.g. `http://mldojo.internal:8765` |
| `MLDOJO_DEFAULT_SERVER` | Baked into the CLI at `make build` as the default `--server`; empty means `http://localhost:8765` |
| `MLDOJO_PLUGIN_DIRS` | Queue plugin directories to install (see "Native deployment" below and [queue-plugins.md](queue-plugins.md)) |

- `MLDOJO_DEFAULT_SERVER` takes effect at **build time**: after changing it, rerun `make build` to get it into the CLI.
- SKILL.md is served by the API at `/SKILL.md`. `deploy/native/install.sh` renders its placeholders:
  `{{MLDOJO_SERVER}}` -> `MLDOJO_PUBLIC_URL`, `{{MLDOJO_INTRANET_SERVER}}` -> `MLDOJO_INTRANET_URL` (the public URL if unset).

## docker-compose

```bash
export MLDOJO_PUBLIC_URL=http://<server-ip>:8765   # used by agents to connect back
export POSTGRES_PASSWORD=$(openssl rand -hex 16)   # required, no default; keep it (or use .env)
docker compose up -d                                   # postgres + api + web(nginx)
docker compose exec api /app/bin/mldojo-api token issue
```

Notes:
- Inside the container the master key is stored in the file `/data/master.key` on the `apidata` volume (there is no keychain in the container). It can also be injected with `MLDOJO_MASTER_KEY`.
- compose does not include any queue plugin. If you need a queue backend, run the plugin (sidecar) separately and set `MLDOJO_QUEUE_PLUGINS` (format `name=url,name2=url2`) on the api container. See [queue-plugins.md](queue-plugins.md) for the protocol.

## Example deployment topology

```
phone/browser ──https──▶ mldojo.example.com (public host)
                              │ nginx, TLS issued by certbot
                              ▼ 127.0.0.1:18765
                        SSH reverse tunnel (dialed out by the API host, kept alive by systemd --user)
                              ▲
                    API host mldojo.internal:8765  ← API + PostgreSQL + Web + local agent
                              ▲ agents connect back (over the LAN, not through the tunnel)
                    other compute nodes
```

## Native deployment (no root, no docker)

On Ubuntu 24.04 (no sudo required):

```bash
mkdir -p ~/ws && cd ~/ws && git clone https://github.com/lovemoon-ai/mldojo.git && cd mldojo
cp deploy/site.env.example deploy/site.env && $EDITOR deploy/site.env
# Requires a Go toolchain in PATH (see go.mod for the version); if go.dev is unreachable, use GOPROXY=https://goproxy.cn,direct
# Next requires node >= 20.9; if the system one is too old, install one from npmmirror:
#   curl -sSL -o /tmp/node.tar.xz https://registry.npmmirror.com/-/binary/node/v22.21.1/node-v22.21.1-linux-x64.tar.xz
#   mkdir -p ~/opt/node-v22 && tar -xJf /tmp/node.tar.xz -C ~/opt/node-v22 --strip-components=1
export PATH=/usr/local/go/bin:$HOME/opt/node-v22/bin:$PATH
make build dist GOPROXY=https://goproxy.cn,direct
npm_config_registry=https://registry.npmmirror.com make web
loginctl enable-linger $(id -un)                                # keep services running after logout
PG_VERSION=16.15.0 deploy/native/install-postgres.sh             # first run only
deploy/native/install.sh
```

`deploy/native/install.sh` runs `<dir>/install.sh` for each directory listed in `MLDOJO_PLUGIN_DIRS`;
a private queue plugin ships its own installer and systemd unit. With no plugins configured,
`"queue_plugins"` in `/api/v1/health` is `{}`, which is completely normal.

| systemd --user unit | Description |
|---|---|
| `mldojo-postgres` | PostgreSQL 16 (zonky binaries in `~/opt/pgsql-16`, data in `~/.mldojo/pgdata`, 127.0.0.1:55432) |
| `mldojo-api` | `~/.mldojo/app/bin/mldojo-api serve`, reads the `api:` section of `~/.mldojo/config.yaml` |
| queue plugin units | Created by the `install.sh` of each plugin directory in `MLDOJO_PLUGIN_DIRS` (see [queue-plugins.md](queue-plugins.md)) |
| `mldojo-agent-<node>` | Created by `mldojo node add` (on this host, or on a remote node with linger enabled) |
| `mldojo-tunnel` | SSH reverse tunnel that connects 127.0.0.1:18765 on the public host to local port 8765 (see below) |
| `mldojo-backup` | Daily backup timer + oneshot service, created by `deploy/native/install-backup.sh` (see "Backup and restore") |

To upgrade, rerun `make build dist web && deploy/native/install.sh`; an existing `config.yaml` is not overwritten.
The agent binary is updated on `mldojo node add` (redeploy).

`~/.mldojo/config.yaml`:

```yaml
server: http://127.0.0.1:8765      # CLI
token: mld_...                     # CLI
api:
  listen: 0.0.0.0:8765
  database_url: postgres://mldojo:***@127.0.0.1:55432/mldojo?sslmode=disable
  public_url: https://mldojo.example.com
  web_dir: ~/.mldojo/app/web
  agent_dist: ~/.mldojo/app/dist
  queue_plugins:                   # optional: <plugin name>: <sidecar url>
    mock: http://127.0.0.1:8766
  queue_poll_sec: 5                # poll interval for queue runs, default 5
```

Every field can be overridden by an environment variable: `MLDOJO_LISTEN`, `MLDOJO_DATABASE_URL`, `MLDOJO_PUBLIC_URL`,
`MLDOJO_WEB_DIR`, `MLDOJO_AGENT_DIST`, `MLDOJO_QUEUE_PLUGINS` (`name=url,name2=url2`), `MLDOJO_API_TOKEN`,
`MLDOJO_MASTER_KEY(_FILE)`, `MLDOJO_NO_KEYCHAIN=1`, `MLDOJO_HOME`.

For the queue plugin protocol, how to register queues (`mldojo queue add`), and the mock plugin for trying queues locally, see [queue-plugins.md](queue-plugins.md).

## Managing the allowlist with ~/.mldojo/env

`deploy/native/install.sh` **rewrites the systemd units on every deploy**, so environment variables written
directly into a unit are lost on the next deploy. The unit contains the line `EnvironmentFile=-~/.mldojo/env`;
that file is never overwritten and is the place for this kind of setting:

```bash
cat > ~/.mldojo/env <<ENV
MLDOJO_SSO_ALLOW=you@example.com
MLDOJO_SSO_ADMINS=you@example.com
ENV
chmod 600 ~/.mldojo/env
systemctl --user restart mldojo-api
```

Environment variables **override** the same settings in config.yaml, so setting both only causes confusion.
It is best to leave them out of config.yaml entirely and let the env file be the single source.

Check the values actually in effect (read the running process's environment, not the file):

```bash
tr '\0' '\n' < /proc/$(systemctl --user show -p MainPID --value mldojo-api)/environ | grep MLDOJO_SSO
```

### Two behaviors to know

- **An empty allowlist disables SSO** (it does not mean "let everyone in"). If the env file is lost or a variable
  is misspelled, the result is that nobody can log in via SSO, not that everybody can: it fails closed.
  API tokens still work, so you cannot lock yourself out completely.
- **Removing someone from the allowlist immediately ends their existing sessions.** The allowlist is checked on
  every request, and a session that no longer matches is deleted. There is no need to clean up the `sessions` table by hand.

For a single-user private deployment, set both variables to just yourself:

```bash
MLDOJO_SSO_ALLOW=you@example.com
MLDOJO_SSO_ADMINS=you@example.com
```
## Backup and restore

Without backups, one disk failure means losing the entire experiment history permanently.
`deploy/native/backup.sh` packs the two things worth keeping into a single UTC-timestamped bundle (a plain tar):

| Bundle member | Source | Notes |
|---|---|---|
| `db.sql.gz` | `pg_dump --clean --if-exists`, gzip -9 | Everything: runs / projects / metrics / secrets / sessions |
| `data.tar.gz` | data_dir (default `~/.mldojo/data`) | `runs/*.log` logs and `blobs/` code bundles; **`cache/` and `tmp/` are excluded**: the former is a content-addressed artifact cache and the latter holds unfinished uploads. Both are rebuilt by `New` in `api/internal/logstore` on every start, so backing them up only makes the archive bigger and slower |
| `master.key` | `~/.mldojo/master.key` | **Not included by default**; set `MLDOJO_BACKUP_INCLUDE_KEY=1` explicitly to include it. The `secrets` table is age-encrypted and cannot be decrypted after restore without this key; but backups exist to be copied elsewhere, and including the master key means "whoever has this one file has every SSH private key and bucket credential", so by default it is kept separately from the backup |
| `MANIFEST` + `SHA256SUMS` | Generated by the script | Metadata and per-member checksums; **no connection strings or passwords** |

Bundles go to `$MLDOJO_BACKUP_DIR` (default `~/.mldojo/backups`, directory `0700`, files `0600`), each with a
`.sha256` file next to it. The latest 7 are kept by default (`MLDOJO_BACKUP_KEEP`); older ones are deleted automatically.
The script takes a lock (`flock` if available, otherwise `mkdir`), so when scheduled runs overlap, the later one
skips and exits 0 instead of two dumps stepping on each other. Database passwords are replaced with `***` in all output.

### Installing daily scheduled backups (native deployment)

```bash
deploy/native/install-backup.sh                        # once a day, keep 7
MLDOJO_BACKUP_KEEP=30 deploy/native/install-backup.sh  # keep 30
```

The installer **runs one backup immediately** as a check: a misconfiguration fails right there instead of
silently failing at 3 a.m. The script creates `mldojo-backup.service` (`Type=oneshot`) + `mldojo-backup.timer`
(`OnCalendar=daily`, `RandomizedDelaySec=1h`, `Persistent=true`, so a run missed while the machine was off
runs after boot). `ExecStart` points at `deploy/native/backup.sh` in the current checkout, so fixes picked up by
`git pull` take effect automatically; **if you move the checkout, rerun `install-backup.sh`**.

```bash
systemctl --user list-timers mldojo-backup.timer    # when is the next run
systemctl --user start mldojo-backup.service        # back up right now
journalctl --user -u mldojo-backup.service -n 50    # how did the last run go
```

As with the other units, the timer does not run after logout unless `loginctl enable-linger` is on.

### Manual backup

```bash
deploy/native/backup.sh
```

The database connection string is resolved in the order `MLDOJO_DATABASE_URL` -> `~/.mldojo/postgres.url` ->
`database_url` in `config.yaml`; data_dir in the order `MLDOJO_DATA_DIR` -> `data_dir` in `config.yaml` ->
`~/.mldojo/data`; `pg_dump` is looked up via `MLDOJO_PGDUMP` -> `PATH` -> `~/opt/pgsql-*/bin/pg_dump`
(a natively installed PostgreSQL is not on PATH).

### Restore

**Destructive**: it overwrites the current database and data_dir. The script requires explicit confirmation;
in a non-interactive environment it refuses to run without `MLDOJO_RESTORE_YES=1`.

```bash
deploy/native/restore.sh                                     # no argument: list available bundles
deploy/native/restore.sh ~/.mldojo/backups/mldojo-backup-20260920T030000Z.tar   # interactive, asks you to type yes
MLDOJO_RESTORE_YES=1 deploy/native/restore.sh ~/.mldojo/backups/mldojo-backup-*.tar  # for scripts
```

What it does, in order:

1. Verify the adjacent `.sha256` file -> unpack -> verify `SHA256SUMS` inside the bundle -> `gzip -t`; if any step fails it exits **without touching existing data**;
2. Print the MANIFEST and the targets that will be overwritten (connection string redacted), then wait for confirmation;
3. `systemctl --user stop mldojo-api`;
4. Load the dump with `psql -v ON_ERROR_STOP=1` (the dump includes `DROP ... IF EXISTS`, so objects with the same name are dropped first);
5. **Rename** the existing data_dir to `<data_dir>.pre-restore-<ts>` (not delete, so a bad restore can be rolled back), then unpack and recreate `cache/` and `tmp/`;
6. Master key: if there is none locally, write it to `~/.mldojo/master.key`; if there is a local one that differs from the bundle's, keep the local one, put the backup's copy at `~/.mldojo/master.key.restored`, and warn;
7. `systemctl --user start mldojo-api`.

In environments without `mldojo-api.service` (compose, macOS), the script tells you to stop the API yourself first
(`docker compose stop api`) and start it again after the restore.

The master key is unlocked in the order keychain -> `MLDOJO_MASTER_KEY` -> key file, and **if one does not match
it automatically tries the next** (`AutoUnlock` in `api/internal/secrets`). So when restoring onto a new machine,
putting the bundle's `master.key` back at `~/.mldojo/master.key` is enough; a stale keychain entry left on the old
machine will not get in the way. If none match, the API still starts, but secrets are locked and the log shows
`secrets: master key rejected` / `secrets: locked`.

### Restore drill

A backup that has never been test-restored is not a backup. Run through the full process on a **non-production** machine every quarter:

> This must be done on a **machine that is not running production MLDojo**: `install-postgres.sh` always writes the
> connection string to `~/.mldojo/postgres.url` (regardless of `MLDOJO_HOME`), so running it on the production machine overwrites production's.

```bash
# 1. Install a separate PostgreSQL on the drill machine (use another port to avoid clashing with other instances)
PGPORT=55433 deploy/native/install-postgres.sh

# 2. Copy the production bundles over (together with the .sha256 files)
mkdir -p ~/drill/backups && scp 'prod-host:~/.mldojo/backups/mldojo-backup-*.tar*' ~/drill/backups/

# 3. Restore into the drill environment (the connection string comes from ~/.mldojo/postgres.url, i.e. the instance installed above)
MLDOJO_DATA_DIR=~/drill/data MLDOJO_RESTORE_YES=1 \
  deploy/native/restore.sh ~/drill/backups/mldojo-backup-<ts>.tar

# 4. Start a temporary API against the drill database and watch the startup log
MLDOJO_DATA_DIR=~/drill/data MLDOJO_DATABASE_URL=$(cat ~/.mldojo/postgres.url) \
  MLDOJO_LISTEN=127.0.0.1:8799 mldojo-api serve
```

Acceptance checklist; the drill passes only if **everything matches**:

| Check | Command | Expected |
|---|---|---|
| API starts | `curl -s 127.0.0.1:8799/api/v1/health` | healthy |
| Master key came back | Startup log from the previous step | **No** `secrets: master key rejected` or `secrets: locked` (the item most likely to fail in a drill) |
| Metadata intact | `MLDOJO_SERVER=http://127.0.0.1:8799 mldojo run ls` | Same count as production |
| Metrics intact | `mldojo run metrics <run-id>` | Same number of points as production |
| data_dir really came back | `mldojo run logs <run-id>` | Non-empty log (verifies `runs/*.log`) |
| Secrets usable | `mldojo secret ls` | Names are listed |
| RTO | Time the whole process | Write it down; this is your real recovery time |

After the drill, delete `~/drill`, `~/.mldojo/pgdata`, and the temporary PG service.

### docker compose

In compose the database is `postgres:16-alpine` (named volume `pgdata`) and data_dir is `/data` on the `apidata` volume.
The host usually has no pg_dump, so use the one in the container:

```bash
cd <repo-dir>          # docker compose exec needs to find docker-compose.yml
export MLDOJO_PGDUMP='docker compose exec -T postgres pg_dump'
export MLDOJO_PSQL='docker compose exec -T postgres psql'
export MLDOJO_DATABASE_URL="postgres://mldojo:$POSTGRES_PASSWORD@127.0.0.1:5432/mldojo?sslmode=disable"
```

data_dir lives in a named volume and reading it directly from the host needs root, so copy it out first and back up the copy:

```bash
docker compose cp api:/data /tmp/mldojo-data
MLDOJO_DATA_DIR=/tmp/mldojo-data deploy/native/backup.sh
rm -rf /tmp/mldojo-data
```

To skip this copy, replace `apidata:/data` in `docker-compose.yml` with a bind mount (e.g. `./data:/data`);
then `MLDOJO_DATA_DIR=./data deploy/native/backup.sh` backs it up directly. Restoring works the same way: first
`docker compose stop api`, run `restore.sh`, then `docker compose start api`.

### What this does not cover yet

- **Full backups only, no PITR**: WAL archiving is not enabled, so the recovery point is the last backup (worst case 24 hours of loss by default).
  Minute-level RPO needs `archive_command` + basebackup.
- **By default backups sit on the same disk as the data**: `~/.mldojo/backups` is local, which protects against accidental deletion but not disk failure.
  You need at least one off-site sync (`rsync` to another machine or object storage) to really have a backup.
- The bundle contains API tokens and sessions and is **as sensitive as the database itself**; encrypt it before storing it off-site.
  With `MLDOJO_BACKUP_INCLUDE_KEY=1` it also contains the master key that decrypts all secrets,
  in which case encryption is mandatory, and it must not be stored in the same place as the other copy of the master key.

## Public access: SSH reverse tunnel + nginx on a public host

When the API host is on an internal network with no public entry point (or solutions such as Tailscale or a
Cloudflare quick tunnel are not usable), you can relay through a public host of your own:

```bash
# 1. API host: generate a dedicated key and install the tunnel service
REMOTE_HOST=root@<public-host-ip> REMOTE_PORT=18765 deploy/native/install-tunnel.sh
# 2. Public host: add the line printed by the previous step to ~/.ssh/authorized_keys (restricted to binding only this one port)
#    restrict,port-forwarding,permitlisten="127.0.0.1:18765",command="/usr/bin/false" ssh-ed25519 ...
# 3. API host: start the tunnel
systemctl --user enable --now mldojo-tunnel
# 4. Public host: nginx site proxying to the tunnel port (mind the WebSocket Upgrade header; logs and metrics depend on it)
#    server_name mldojo.example.com; proxy_pass http://127.0.0.1:18765;
# 5. Add a DNS A record pointing at the public host; once it resolves, issue the certificate:
certbot --nginx -d mldojo.example.com --non-interactive --redirect --keep-until-expiring
```

Properties: the API host only makes outbound connections and opens no ports to the internet; agents still connect
to the API directly over the LAN, not through the tunnel. Authentication is still only MLDojo's own tokens; once
exposed publicly, run `mldojo-api token rotate` periodically.

## Login: Conductor SSO

Browsers log in with Conductor single sign-on; the CLI and agents keep using API tokens. Both paths work at the same time.

```yaml
# api: section of ~/.mldojo/config.yaml
api:
  web_url: https://mldojo.example.com             # public entry point; the reverse proxy hides the real Host, so all redirects are built from this
  sso:
    base_url: https://conductor.example.com
    client_id: mldojo
    client_secret: <random-32-bytes>              # or MLDOJO_SSO_CLIENT_SECRET
    # Put allow / admins in ~/.mldojo/env (see above) rather than here:
    # install.sh rewrites the systemd unit every time but never touches that file.
    allow: [you@example.com]                      # required allowlist (id/email/phone/name)
    admins: [you@example.com]                     # subset of allow that gets the admin role
                                                  # logged-in users not in admins are members: they can see everything and submit runs,
                                                  # but can only delete their own, and cannot touch nodes / queues / secrets (see "Permissions" in docs/api.md)
```

**An empty `allow` disables SSO** (`Enabled() = Configured() && len(Allow) > 0`). An empty allowlist used to let
every Conductor user in, which is not a safe default for a public deployment. If only the four OAuth settings are
configured without `allow`, the SSO button disappears from the login page, leaving only token login, and the startup log says
`Conductor SSO is configured but disabled: sso.allow is empty`.
For troubleshooting see [troubleshooting.md](troubleshooting.md) ("Symptom: SSO is configured but there is no SSO button on the login page").

Flow (`api/internal/auth`):

| Endpoint | Description |
|---|---|
| `GET /api/v1/auth/config` | No auth; the login page uses it to decide whether to show the SSO button |
| `GET /api/v1/auth/login?next=/x` | 302 to `<base>/oauth/authorize`; state is stored in a 10-minute HttpOnly cookie |
| `GET /api/v1/auth/callback` | Check state -> exchange code for identity -> create session -> redirect to `next` |
| `GET /api/v1/auth/me` | Current identity; `mode` is `sso` or `token` |
| `POST /api/v1/auth/logout` | Log out |

Sessions are stored in the `sessions` table; the cookie is HttpOnly + Secure + SameSite=Lax and valid for 30 days.
The table stores the cookie's sha256, so a leaked database cannot be turned into sessions. Conductor's access_token
is **not stored**: MLDojo never calls Conductor APIs on the user's behalf, so there is nothing to persist. The logged-in
user is recorded in `runs.metadata.submitter` and `projects.owner` (recorded only, not used for authorization).

Registering on the Conductor side: add an entry
`{"client_id":"mldojo","display_name":"MLDojo","client_secret":"…","redirect_uris":["https://mldojo.example.com/api/v1/auth/callback"]}`
to `CONDUCTOR_SSO_CLIENTS_JSON` (**must be a single line**), then restart the Conductor web process so it reloads. Some pitfalls:

- `redirect_uri` must be byte-for-byte identical in three places (authorize link, registry, token request); one extra slash gives `invalid_grant`.
- `state` is required and must be non-empty, otherwise Conductor's authorize page errors out and never redirects back.
- No OIDC library: no discovery, no JWKS, no PKCE; the access_token is an opaque string with no expiry.
- Verifying without a browser: hit the token endpoint with the real secret and a fake code. `invalid_grant` means
  client_id/secret/redirect_uri are all correct; `invalid_client` means it is not registered yet or Conductor was not restarted.

## Observability: metrics, alerts, and log format

### Prometheus scraping

`GET /api/v1/metrics` outputs the standard text format (see [api.md](api.md) for the metric list).
**It requires an admin token by default**, because it exposes project sizes, node counts, and all routes. Scrapers usually do not carry a token; there are two ways to set it up:

```yaml
# api: section of ~/.mldojo/config.yaml
api:
  metrics_public: true   # or MLDOJO_METRICS_PUBLIC=1; default false
```

```yaml
# Option 1 (recommended): keep metrics_public: false and have Prometheus send a token
scrape_configs:
  - job_name: mldojo
    metrics_path: /api/v1/metrics
    static_configs: [{targets: ["mldojo.internal:8765"]}]
    authorization: {type: Bearer, credentials: "mld_..."}   # `mldojo-api token create --name prometheus`
```

```yaml
# Option 2: metrics_public: true, but only let the internal network or the reverse proxy reach this path
#   nginx: location = /api/v1/metrics { allow 10.0.0.0/8; deny all; proxy_pass ...; }
```

On a public instance (e.g. `mldojo.example.com` behind the SSH reverse tunnel + nginx), **do not** simply turn on
`metrics_public` unless nginx restricts the source.

Scrape cost: run/node counts are cached for 15 seconds, and `data_dir` usage is sampled in the background every 5 minutes,
so scraping itself does not hit the database or the disk.

### Alert webhook

Events come from four places in the code: a run turning `failed` (`backends.SetStatus`), an agent disconnecting
(`backends/hub.go`), `data_dir` over its limit, and secrets locked for more than 1 hour (the last two in `watchPlatform`).
There is a single output: POST a JSON body to a webhook.

```yaml
api:
  alerts:
    webhook_url: https://open.feishu.cn/open-apis/bot/v2/hook/xxxx  # or MLDOJO_ALERTS_WEBHOOK_URL
    on: [run.failed, node.offline, disk.high, secrets.locked]       # empty = subscribe to all; or MLDOJO_ALERTS_ON (comma-separated)
    min_interval_sec: 600                                           # dedup window per key, default 600
    disk_limit_gb: 200                                              # report disk.high when data_dir exceeds this; 0 = no check
```

| Event | Trigger | Dedup key |
|---|---|---|
| `run.failed` | A run enters failed | Once per run id |
| `node.offline` | An agent connection drops | Once per node id |
| `disk.high` | `data_dir` > `disk_limit_gb` | One global key |
| `secrets.locked` | Secrets locked continuously > 1h | One global key |

A single JSON payload serves three kinds of receivers: Feishu custom bots read `msg_type` / `content.text`,
Slack incoming webhooks read `text`, and other systems read `event` / `level` / `title` / `fields` / `instance`.

```json
{"msg_type":"text","content":{"text":"[MLDojo] node offline: gpu-a\n..."},
 "text":"[MLDojo] node offline: gpu-a\n...","source":"mldojo",
 "instance":"https://mldojo.example.com","event":"node.offline","level":"warning",
 "title":"node offline: gpu-a","fields":{"node":"gpu-a","agent_version":"0.1.0-dev"},
 "time":"2026-09-20T10:12:07+08:00"}
```

A few hard constraints to know before configuring it:

- **It cannot drag down the main path**: `Notify` only pushes into a channel of length 64 and drops when it is full;
  the actual POST happens in a separate goroutine with a 10-second timeout, and a failure only logs a `WARN alert webhook` line.
- **It does not flood**: besides the per-key `min_interval_sec`, there is a global token bucket (at most 20 at once,
  then 1 more per minute). When a dead node makes 200 runs fail at once, 20 alerts go out, not 200.
  The number suppressed is recorded in `mldojo_alerts_total{outcome="suppressed"}`.
- **It does not leak secrets**: the payload only carries identifiers (run/project/experiment/node/target/exit_code),
  and a regex pass replaces `mld_*` tokens, `Bearer *`, `postgres://...`, `token=`/`password=`/`api_key=`,
  and hex strings of 32+ characters (agent token hashes) with `[redacted]`, truncating each field to 300 bytes.
  **Never add secret values, env, or full command lines to `Fields`.**
- To check the configuration: the `mldojo-api serve` startup log contains `alerts=true`;
  scrape `/api/v1/metrics` once and see whether `mldojo_alerts_total{event=...,outcome="sent"}` increases.

### Structured logs

```bash
MLDOJO_LOG_FORMAT=json    # one JSON object per line, ready for Loki/ES; default text is unchanged
MLDOJO_LOG_LEVEL=debug    # default info
```

Both the api and agent binaries honor these two variables (`internal/logging`). In systemd, add:

```ini
# Add one line to ~/.config/systemd/user/mldojo-api.service (generated by install.sh), then
# systemctl --user daemon-reload && systemctl --user restart mldojo-api
Environment=MLDOJO_LOG_FORMAT=json
```

Every access log line carries `id` (= the `X-Request-Id` response header), which matches `request_id` in the audit records.

## TLS for the agent link

On the public access path, TLS is terminated by nginx on the public host, but **agent <-> API traffic is plain HTTP/WS on the internal network**:
node tokens and all logs travel unencrypted. Configuring a certificate on the API itself closes this gap:

```yaml
api:
  tls_cert: ~/.mldojo/tls/server.crt     # or MLDOJO_TLS_CERT
  tls_key:  ~/.mldojo/tls/server.key     # or MLDOJO_TLS_KEY
  tls_client_ca: ~/.mldojo/tls/ca.crt    # optional: require client certificates (mTLS), or MLDOJO_TLS_CLIENT_CA
```

- `tls_cert` and `tls_key` **must be set together**; setting only one fails at startup instead of silently falling back to plaintext.
- Setting `tls_client_ca` without a server certificate also fails immediately: mTLS cannot exist without server-side TLS.
- Certificate parse errors surface at startup, not on the first request.

On the node side, tell the agent what to trust (`~/.mldojo/agent/<node>.yaml`):

```yaml
server_url: https://mldojo.internal:8765   # https automatically makes the WebSocket use wss
ca_file: ~/.mldojo/tls/ca.crt          # required for self-signed or internal CAs
client_cert: ~/.mldojo/tls/agent.crt   # only when the server has mTLS enabled
client_key:  ~/.mldojo/tls/agent.key
```

`ca_file` is **appended** to the system root certificates, so internal CAs and public certificates can be mixed.
`client_cert` and `client_key` must also be set together.

Self-signed certificate (good enough for an internal network):

```bash
mkdir -p ~/.mldojo/tls && cd ~/.mldojo/tls
openssl req -x509 -newkey rsa:2048 -nodes -days 3650 \
  -keyout server.key -out server.crt \
  -subj "/CN=mldojo" -addext "subjectAltName=DNS:mldojo.internal,DNS:localhost"
chmod 600 server.key
```

Use `server.crt` as the `ca_file` on each node as well (a self-signed certificate is its own CA).


## Nodes

```bash
mldojo node add --id local --local
mldojo node add --id gpu-a --ssh alice@192.0.2.11 --labels 5090
mldojo secret set ssh_keys/id_ed25519 --from-file ~/.ssh/id_ed25519
mldojo node add --id gpu-b --ssh root@203.0.113.10 --port 39670 --identity secret://ssh_keys/id_ed25519 \
    --reverse-tunnel --labels h20            # use a reverse tunnel when a public node cannot reach the internal API
mldojo node add --id bastion-gpu-1 --ssh "alice@alice@192.0.2.20@bastion.example.com" --port 2222 \
    --identity secret://ssh_keys/id_ed25519 --via gpu-a --via local --reverse-tunnel --labels 5090,8gpu
```

Add `--dry-run` to only test connectivity (route, OS, GPU) without deploying anything. For a node configured with
`--reverse-tunnel`, dry-run also checks whether the SSH server allows remote forwarding. If it does not, use
`--reverse-tunnel-mode stdio`: the connection is relayed over the stdin/stdout of a plain exec session instead
(`adapters/stdio_relay`), which works wherever `ssh host cmd` does. `mldojo node upgrade <id>`
upgrades the agent in place; running runs are taken over by the new agent. If the node came back on another
address (a container restarted on a new SSH port), `mldojo node upgrade <id> --port N` (or `--ssh user@host`)
deploys there and records the new address once the agent connects.

Note: some JumpServer-style bastion hosts **silently drop** exec commands containing `rm -f` (empty output, exit 0).
So the deploy scripts do not use `rm`, and they verify that each step actually took effect (binary checksum, completion markers of the start and stop scripts).
