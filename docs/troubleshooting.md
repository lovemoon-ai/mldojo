English | [简体中文](troubleshooting.zh-CN.md)

# Troubleshooting

Organized by **symptom**. Each entry follows the same structure: symptom → how to confirm → how to fix.
All error messages are quoted verbatim from the code, so you can `grep` for them directly.

For a fresh install, start with [docs/quickstart.md](quickstart.md); deployment details are in [docs/deploy.md](deploy.md).

Symptom index:

- [Agent stays offline / `node add` fails](#symptom-agent-stays-offline--node-add-fails)
- [Run stuck in queued](#symptom-run-stuck-in-queued)
- [Run stuck in starting](#symptom-run-stuck-in-starting)
- [Run fails immediately with exit code 96 or 97](#symptom-run-fails-immediately-with-exit-code-96-or-97)
- [secrets are locked](#symptom-secrets-are-locked)
- [Logs not updating / metrics missing](#symptom-logs-not-updating--metrics-missing)
- [Web UI won't load / always 401](#symptom-web-ui-wont-load--always-401)
- [SSO login redirects back to localhost](#symptom-sso-login-redirects-back-to-localhost)
- [SSO is configured, but the login page has no SSO button](#symptom-sso-is-configured-but-the-login-page-has-no-sso-button)
- [Queue plugin unavailable](#symptom-queue-plugin-unavailable)
- [Cannot connect to the database / migration fails](#symptom-cannot-connect-to-the-database--migration-fails)

---

## Run these three first

They pinpoint most problems:

```bash
mldojo health                 # control plane: db / secrets / online agent count / queue plugins
mldojo node ls                # whether each node's agent is online, how long ago it last sent a heartbeat
mldojo run logs <run> --stream system   # what the platform did for this run (not your training output)
```

`--stream system` is the most underrated one: scheduling, dispatch, dataset sync, agent disconnects and
reaping are all written to this stream, prefixed with `[mldojo]`.

### CLI exit codes

This line is also printed at the top of `mldojo <any command> --help`. In scripts, checking the exit code is more reliable than parsing output:

| Code | Meaning | Typical cases |
|---|---|---|
| 0 | Success | |
| 2 | User error | Bad arguments, resource not found (404), invalid token (401), invalid recipe |
| 3 | Unreachable | Cannot reach the API, node SSH fails, agent offline |
| 4 | Backend error / run failed | Server 5xx; **with `run submit --wait`, any run that did not succeed also yields 4** |
| 5 | Conflict | A project / node with the same name already exists |
| 130 | Ctrl-C | |

**There is no 1** - the CLI never returns 1 on any path. `mldojo-api` is the opposite: any startup failure exits with 1.
With `--json`, errors are JSON too, printed to **stdout**: `{"error": ..., "code": ..., "exit_code": N}`.

> Don't confuse these: the table above is the exit code of the **CLI process**. The `EXIT` column in `mldojo run ls` is the exit code of the **training process**;
> the two are completely unrelated (see [exit code 96 / 97](#symptom-run-fails-immediately-with-exit-code-96-or-97)).

---

## Symptom: agent stays offline / `node add` fails

### How to confirm

```bash
mldojo node test <id>        # reports two steps: whether SSH works, whether the agent connected back
mldojo node ls               # AGENT and HEARTBEAT columns
```

`node test` has only three messages: `ok`, `agent is not connected`, `agent ping failed: ...`.
If you are unsure about routing, use a probe that does not write to the database:

```bash
mldojo node add --id <id> --ssh <user@host> --identity secret://ssh_keys/<k> --dry-run
```

> `node add` is **all or nothing**: if any step fails, nothing is written to the database (the `AddNode` comment says
> "Any failure returns an error and stores nothing"). So after a failure you do **not** need to `node rm` first; just fix the command and retry.

### Fix by error message

| Error (verbatim) | Cause | Fix |
|---|---|---|
| `no ssh auth method: set identity or password` | Neither `--identity` nor `--password` was given | Provide one. For private keys, a secret is recommended: `mldojo secret set ssh_keys/k --from-file ~/.ssh/id_ed25519`, then `--identity secret://ssh_keys/k` |
| `identity secret://ssh_keys/k: ...` | The private key cannot be read or parsed | Make sure the secret exists (`mldojo secret ls`) and secrets are not locked; **passphrase-protected keys are not supported**, use one without a passphrase |
| `host key for <host> changed (edit ~/.mldojo/known_hosts if expected): ...` | The host fingerprint changed after it was first trusted (reinstall / machine swap / MITM) | Once you've confirmed the change is expected, edit `~/.mldojo/known_hosts` (the one **on the API host**, not your `~/.ssh/known_hosts`) and delete the old line |
| `all routes to <id> failed: via local: ...; via gpu-a: ...` | None of the `--via` routes worked; each semicolon-separated part is the reason for one route | Check them one by one. `via local` means a direct connection from the API host |
| `via chain too deep (cycle?)` | The `--via` chain forms a cycle | Check the via chain; a node cannot point back to itself indirectly |
| `node cannot be its own via` | `--via` names the node itself | Remove it |
| `node "x" already exists` | The id is taken (409, exit code 5) | Use another id, or `mldojo node rm x` |
| `connection.host is required for ssh nodes` | `--ssh` has no host | `--ssh user@host` |
| `reverse_tunnel requires an ssh node` | `--local` was combined with `--reverse-tunnel` | Local nodes don't need a tunnel; remove it |

### `agent ... did not connect back within 60s`

Full message:

```
agent on <route> did not connect back to <url> within 60s (is the server reachable from the node?
set connection.agent_server_url or reverse_tunnel). agent log:
<tail of the agent log on the node>
```

This means **SSH works and the agent was installed, but it cannot connect back to the API**. The error already includes the tail of the agent log from the node; read that first.

Check whether the node can reach the API (`<url>` is the one in the error):

```bash
ssh <user@host> "curl -sS -m 5 <url>/api/v1/health"
```

Fixes, by case:

- **`public_url` is wrong** (most common): it is set to `localhost` or an address only the API host can resolve.
  The agent uses `api.public_url` / `MLDOJO_PUBLIC_URL`. If it is not set, you get
  `server public_url is not configured; set api.public_url (MLDOJO_PUBLIC_URL) to a URL agents can reach`.
- **A single node needs a different address**: `mldojo node add ... --agent-server-url http://<other-address>:8765`.
- **The node cannot reach the API at all** (public cloud machine, one-way firewall): add `--reverse-tunnel` so the agent connects back through an SSH reverse tunnel.
  Verify with `--dry-run` first; the output gains a `reverse tunnel:` line: `ok` means the server allows `-R`,
  `not allowed: ...` means it is blocked (common on bastion hosts).

The agent log on the node (after `node add` fails the agent is stopped, but the log remains):

```bash
ssh <user@host> "tail -50 ~/.mldojo/agent/<node-id>.log"
```

> There is no `mldojo node logs` command - agent logs exist only on the node.

### Bastion host: commands silently dropped

Error:

```
upload agent to <host>: checksum mismatch after upload (a bastion command filter may have dropped the command)
```

Some JumpServer bastion hosts **silently drop** exec commands containing `rm -f` (empty output, exit 0).
For this reason the deploy scripts avoid `rm` and verify that every step actually took effect (binary sha256, completion markers of the start/stop scripts).
This error is that check catching it. Related errors:

- `start script did not complete on <host> (output: "...")`
- `stop script did not complete on <host> (output: "...")`
- `unexpected probe output from <host>: "..."`

Fix: look at what the bastion actually returned in `output:`. Usually either the command was filtered, or a login banner / interactive prompt got mixed into the output.

### Usernames with multi-level bastion hosts

`--ssh` splits user/host at the **last `@`**, so nested usernames can be written as-is:

```bash
mldojo node add --id bastion-gpu-1 --ssh "alice@alice@192.0.2.20@bastion.example.com" --port 2222 \
    --identity secret://ssh_keys/id_ed25519 --via gpu-a --via local --dry-run
```

`--via` can be repeated; routes are **tried in order, falling back automatically on failure**; `local` means a direct connection from the API host.
The `route:` line in the `--dry-run` output tells you which route finally worked, e.g. `local -> gpu-a -> bastion-gpu-1`.

### Agent installed but goes offline after a while

How the server decides:

- the agent sends a heartbeat every **5 seconds**;
- if the WebSocket sees no message for **60 seconds** it is closed and the node is immediately marked offline (a ping is sent every 20s);
- as soon as the connection drops, `nodes.agent_status` is set to `offline`.

So a `HEARTBEAT` older than 1 minute almost always means a real disconnect. Check whether the agent on the node is still alive:

```bash
ssh <user@host> "systemctl --user is-active mldojo-agent-<id>; tail -30 ~/.mldojo/agent/<id>.log"
```

The agent log will contain `disconnected from server` with `retry_in` (it backs off and reconnects on its own).

**The most common cause is linger not being enabled**: `systemd --user` services are killed after you log out of SSH.

```bash
ssh <user@host> "loginctl show-user \$(id -un) -p Linger --value"   # expect yes
ssh <user@host> "loginctl enable-linger \$(id -un)"
```

Nodes without a usable `systemctl --user` fall back to `nohup setsid`; in that case the agent is gone after a reboot,
and you just run `mldojo node add` again.

---

## Symptom: run stuck in queued

`queued` means the platform accepted the run but has not yet dispatched it to any agent. **A queued run is never automatically declared dead**,
so it can wait indefinitely.

### How to confirm

```bash
mldojo run logs <run> --stream system
mldojo run show <run>            # look at the message field
mldojo node ls                   # is the target node online?
```

Typical system log:

```
[mldojo] node <id> agent is offline; the run stays queued until it reconnects
```

The run's message is then `waiting for agent on <node>`.

### How to fix

1. **The target node is offline** - this is the vast majority of cases. Bring the node back following
   [agent stays offline](#symptom-agent-stays-offline--node-add-fails) above;
   the run continues on its own (no need to resubmit).
2. **The target is wrong**. This is rejected at submit time (exit code 2): ``unknown node "x" (see `mldojo node ls`)``.
   There are only two target formats: `node:<id>` and `queue:<backend>/<queue>`.
3. **You don't want to wait**: `mldojo run cancel <run>`.

> Insufficient resources do **not** leave a run stuck in queued - `run submit` checks and rejects at submit time:
> `node <id> cannot satisfy gpus=4 gpu_type="" min_mem_gb=0 (node has: ...)`, exit code 2.

---

## Symptom: run stuck in starting

`starting` covers **code upload → image pull → environment setup**, and the first run can genuinely be slow
(conda env create and docker pull both happen here).

### How to confirm

```bash
mldojo run logs <run> --stream system   # dispatch progress
mldojo run logs <run> --stream stderr   # pip / conda / docker output is here
```

On entering starting, the system stream shows `[mldojo] dispatching to <node>`.

### How to fix

- **Wait**. starting is capped at **2 hours**; after that the reaper marks the run failed with the reason
  `stuck in starting for more than 2h0m0s`.
- **It really is stuck**: `mldojo run cancel <run>`, then check the agent log on the node.
  Common causes are docker pull failing to fetch an internal image, or conda still resolving dependencies.
- **Avoid repeated setup cost**: venv/conda environments are cached on the node under
  `~/.mldojo/envs/`, keyed by the hash of the spec file, so the second run with the same spec does not rebuild.

---

## Symptom: run fails immediately with exit code 96 or 97

These are exit codes defined by the **agent wrapper script**; they are not returned by your training script and are unrelated to CLI exit codes.
They appear in the `EXIT` column of `mldojo run ls` and in the `message` of `mldojo run show`.

| EXIT | message (verbatim) | Meaning |
|---|---|---|
| 96 | `workdir not found` | The wrapper's `cd <workdir>` failed |
| 97 | `environment setup failed (see stderr)` | venv / conda environment setup failed |

### 96: workdir not found

`run.workdir` is **relative to the code root** (with `code.source: git` it is the repository root, not the directory containing the recipe file).

```bash
mldojo run show <run>                   # confirm workdir and code source
mldojo run logs <run> --stream system
```

Fix: change `run.workdir` in the recipe to a relative path that actually exists in the code bundle.
With `code.source: local`, also make sure that directory is not filtered out by `.gitignore` / `.mldojoignore` -
inside a git repository, `local` mode **filters by `.gitignore`** when packaging.

### 97: environment setup failed

Check stderr first; the agent prints the reason, always prefixed with `[mldojo]`:

```bash
mldojo run logs <run> --stream stderr
```

| Line in stderr | Fix |
|---|---|
| `[mldojo] conda not found on this node` | No conda on the node. The agent looks for `conda` (PATH), `~/miniconda3`, `~/miniforge3`, `~/anaconda3`, `/opt/conda`. Install one, or switch to `env.type: venv` |
| `[mldojo] conda env <name> not found` | `env.spec` is an environment name that doesn't exist on the node. Use a spec file (`environment.yaml`) instead so the agent builds it |
| `[mldojo] creating venv ...` followed by pip errors | Dependencies fail to install, usually a network issue; give the node a proxy: `mldojo node add ... --proxy http://...` (the agent passes `http(s)_proxy` to the run) |
| `[mldojo] venv setup failed` / `[mldojo] conda env setup failed` | Same as above; look at the actual errors in the lines before it |

When environment setup fails, the half-built directory is deleted, so once you fix the problem you can simply resubmit without manual cleanup.

> `env.type: none` (used by the hello recipe) never produces 97 - it only does `cd` and runs the command.
> In that case `python3: command not found` makes the run fail with **127**.

---

## Symptom: secrets are locked

Exact message (note the plural `secrets are locked`, not `is`):

```
secrets are locked: run `mldojo secret unlock` or set MLDOJO_MASTER_KEY
```

Secrets are encrypted with an age master key. While locked, **every** path that reads or writes a secret fails -
not just `mldojo secret set`, but any `secret://` reference: SSH identity/password,
queue plugin credentials, bucket credentials. So the symptom often looks like "`node add` suddenly fails authentication".

### How to confirm

```bash
mldojo health                  # secrets: locked
mldojo secret ls               # first line: master key: locked
```

Normally it shows `master key: unlocked (<source>)`, where source tells you where the key came from.

The corresponding two lines in the server log:

```
secrets: master key rejected            # a key was found, but it's not the one used to encrypt
secrets: locked; unlock with `mldojo secret unlock`
```

### How to fix

The server tries to auto-unlock in the order **keychain → `MLDOJO_MASTER_KEY` → key file**
(default `~/.mldojo/master.key`, override with `MLDOJO_MASTER_KEY_FILE`),
and **if one key doesn't match it automatically tries the next**. It tries once at startup, then **retries every minute** while still locked
(the keyring may only become available after the user logs in).

```bash
mldojo secret unlock                              # uses the CLI machine's keychain / MLDOJO_MASTER_KEY
mldojo secret unlock --key-file ~/.mldojo/master.key
```

On success it prints `secrets unlocked (key from <source>)`.

By scenario:

- **Locked after a server restart**: usually the keychain is unavailable in a headless session.
  On containers / headless machines set `MLDOJO_NO_KEYCHAIN=1` and use a key file or `MLDOJO_MASTER_KEY` instead.
  compose does this by default (`MLDOJO_MASTER_KEY_FILE=/data/master.key`).
- **Locked after moving machines / restoring from backup**: put the backed-up `master.key` back at `~/.mldojo/master.key`;
  leftover keychain entries from the old machine won't get in the way (a mismatch just falls through to the next source).
  Note that backups **do not include the master key by default**; set `MLDOJO_BACKUP_INCLUDE_KEY=1` to include it -
  see [deploy.md](deploy.md) (backup and restore section).
- **The key is lost for good**: `master key does not match the key secrets were encrypted with`.
  The encrypted secrets **cannot be recovered**; delete them and `mldojo secret set` them again. Runs / metrics / logs are unaffected.

> On first startup, when the database has no secrets yet, the API **generates** a master key automatically (stored in the keychain first,
> falling back to a key file), logging `secrets: generated master key`. This is normal.

---

## Symptom: logs not updating / metrics missing

### Logs not updating

Live log streaming goes over WebSocket (`/api/v1/runs/{id}/logs/ws`). First figure out whether the logs were "never produced" or "not delivered":

```bash
mldojo run logs <run> --stream all        # one-shot fetch, no WebSocket
mldojo run logs <run> --stream all -f     # follow, over WebSocket
```

Visible with a one-shot fetch but not with `-f` → **the WebSocket is being cut by something in between**. It is almost always a reverse proxy missing the Upgrade headers:

```nginx
proxy_http_version 1.1;
proxy_set_header Upgrade $http_upgrade;
proxy_set_header Connection $connection_upgrade;
proxy_read_timeout 1h;
proxy_buffering off;
```

Not visible either way → the logs really were not produced:

- **Python buffering**. This is the most common cause and has nothing to do with MLDojo. Add
  `run.env: {PYTHONUNBUFFERED: "1"}` to the recipe, or use `print(..., flush=True)`.
- All output goes to stderr: use `--stream stderr` (the default shows only stdout).
- Runs on a queue plugin are **polled**, not real-time: logs lag by a few seconds, and `log_mode` in `run show` is `near-realtime`.

> `--stream all` is only supported over WebSocket. Calling REST `GET /runs/{id}/logs?stream=all` directly
> returns `stream must be stdout|stderr|system` - that endpoint only accepts a single stream.

### Metrics missing

```bash
mldojo run metrics <run>            # empty table = not a single point was collected
```

Metrics come from three sources; make sure you know which one your recipe uses:

1. **File scanning (default, no code changes)**: the path must be declared in `outputs.metrics`, otherwise the agent never scans it:
   ```yaml
   outputs:
     metrics:
       - {type: jsonl, path: outputs/metrics.jsonl}
       - {type: tensorboard, path: outputs/tb}
   ```
   Paths are relative to workdir. jsonl requires **one JSON object per line**; the step is the first present of
   `step`/`_step`/`global_step`/`iteration`/`iter`/`epoch`, falling back to the line number.
   All other numeric fields count as metrics. **The training script must flush promptly**, otherwise the file stays empty.
2. **SDK**: `import mldojo; mldojo.log({"loss": x}, step=i)`. The agent puts the SDK on `PYTHONPATH` automatically;
   in other environments (e.g. inside a docker image) use `pip install sdk/python`.
3. **wandb shim**: set `run.wandb: shim` in the recipe, and `wandb.init/log/finish` write to MLDojo.
   To verify: `wandb.__version__` in the logs should be `0.0.0-mldojo-shim`, not a real wandb version.

Fewer curve points than expected: `GET /metrics` defaults to `max_points=2000` and **downsamples evenly per key** (keeping first and last);
`sampled: true` in the response means it was downsampled. In the CLI, fetch a single key in full with `--key <key>`.

---

## Symptom: Web UI won't load / always 401

### The page just says "The web app is not built"

This means the API is alive but did not find the static pages:

```
MLDojo API <version>
The web app is not built (set api.web_dir / MLDOJO_WEB_DIR to web/out). API: /api/v1/health
```

Fix: run `make web` to build `web/out`, then point `api.web_dir` at it (in a native deployment this is
`~/.mldojo/app/web`, which `deploy/native/install.sh` replaces atomically).
In mainland China: `npm_config_registry=https://registry.npmmirror.com make web`.
Note that Next requires Node >= 20.9.

### The page loads but every API call returns 401

```bash
curl -s -o /dev/null -w '%{http_code}\n' http://<server>/api/v1/projects        # expect 401
curl -s -o /dev/null -w '%{http_code}\n' -H "Authorization: Bearer $MLDOJO_TOKEN" \
     http://<server>/api/v1/projects                                            # expect 200
```

401 is **expected behavior** (every endpoint except `/api/v1/health` requires a token). The web UI asks you to paste a token the first time it opens.
Still 401 with a token → the token is wrong; run `mldojo-api token issue` again (idempotent, prints the existing one).
If you just ran `token rotate`, every node needs `mldojo node upgrade <id>` to pick up the new token.

Browsers and WebSockets cannot set headers, so they use a `?token=<token>` query parameter - **every endpoint accepts it**.

### SPA routes return 404

Paths like `/run` and `/nodes` return 404 while the home page works → the reverse proxy's try_files rule is wrong.
See `deploy/compose/nginx.conf`:

```nginx
location / { try_files $uri.html $uri $uri/index.html =404; }
```

(In the Next static export, `/run` maps to `run.html`, and a directory with the same name also exists, so the order must not be reversed.)

---

## Symptom: SSO login redirects back to localhost

After completing the Conductor login you land on `http://localhost:8765/...`, or the callback URL is wrong.

### How to confirm

```bash
curl -s https://<your-domain>/api/v1/auth/config
```

### How to fix

A reverse proxy hides the real Host, so MLDojo **does not infer** the callback URL from the request; it is built entirely from configuration.
Set `api.web_url` (or `MLDOJO_WEB_URL`) to the public address users actually visit:

```yaml
api:
  web_url: https://mldojo.example.com
```

Without `web_url` it falls back to `public_url` - but `public_url` is the internal address agents use to connect back,
so you get redirected to the internal address / localhost. The two addresses serve different purposes; a public deployment needs both.

The callback URL is `<web_url>/api/v1/auth/callback`, and it **must match byte for byte in three places** (the authorize link,
the `redirect_uris` registered with Conductor, and the token request); a single extra slash gives `invalid_grant`.

Other SSO errors (all are passed through unchanged in the login page URL's `?error=`):

| Error | Cause |
|---|---|
| `token exchange failed (HTTP 401): ... (client_id/client_secret do not match the ones registered with the SSO provider)` | `invalid_client`: not registered, wrong secret, or Conductor was not restarted to load it |
| `token exchange failed (HTTP 400): ... (the code expired or was already used, or redirect_uri does not match the registered value byte for byte)` | `invalid_grant` |
| `login state expired, please try again` | The state cookie expired (10 minutes) or was dropped |
| `<user> is not on this instance's allowlist` | Login succeeded but the user is not in `sso.allow` |

To verify without a browser: hit the token endpoint with the real secret and a fake code.
`invalid_grant` means client_id/secret/redirect_uri are all correct;
`invalid_client` means it is not registered yet or Conductor has not been restarted.

---

## Symptom: SSO is configured, but the login page has no SSO button

**This behavior changed recently; instances whose config was not touched will hit it after upgrading.**

The login page shows only a token input box, and the prompt has changed from "Sign in with your Conductor account, or use an API token."
to the English `Paste your MLDojo API token (MLDOJO_TOKEN).`;
visiting `/api/v1/auth/login` directly returns 400 `SSO is not configured on this server (api.sso in config.yaml)`.

### How to confirm

```bash
curl -s http://<server>/api/v1/auth/config      # {"sso_enabled":false,...}
```

Then check the API startup log; if it contains this line, that's the cause:

```
Conductor SSO is configured but disabled: sso.allow is empty. An empty allowlist would let
every account of the provider in with full access. Set sso.allow (or MLDOJO_SSO_ALLOW) to
the ids/emails that may sign in.
```

```bash
journalctl --user -u mldojo-api | grep "SSO is configured but disabled"    # native deployment
docker compose logs api | grep "SSO is configured but disabled"           # compose
```

### Cause

**An empty `sso.allow` now means SSO is disabled.** Previously an empty allowlist meant "let everyone in" - not a
safe default for a public deployment (any account on Conductor would get full access). The rule now is:

```go
func (c Config) Enabled() bool { return c.Configured() && len(c.Allow) > 0 }
```

That is, SSO is enabled only when all four OAuth settings (`base_url` / `client_id` / `client_secret` / `web_url`) are present **and**
the allowlist is non-empty. If only the former are set and `allow` is empty or `[]`, SSO is silently turned off
and only token login remains - which is exactly what "the login option disappeared" looks like.

### How to fix

List the people allowed to sign in explicitly. `id`, `email`, `phone` and `name` all work, matched case-insensitively:

```yaml
# the api: section of ~/.mldojo/config.yaml
api:
  web_url: https://mldojo.example.com
  sso:
    base_url: https://conductor.example.com
    client_id: mldojo
    client_secret: <32 random bytes>
    allow:
      - you@example.com
      - teammate@example.com
```

Or use an environment variable (comma-separated; it **completely overrides** `allow` from the config file):

```bash
MLDOJO_SSO_ALLOW=you@example.com,teammate@example.com
```

Restart the API; the startup log should now say `Conductor SSO enabled`, with `allowlist=<count>`. Check again:

```bash
curl -s http://<server>/api/v1/auth/config      # expect "sso_enabled":true
```

> The allowlist only controls **who can sign in**. There is no fine-grained authorization after login; the signed-in user is recorded in
> `runs.metadata.submitter` and `projects.owner` (recorded only, not enforced).

---

## Symptom: queue plugin unavailable

In `/api/v1/health` (and `mldojo health`), some plugin in `"queue_plugins"` is `false`, e.g.
`"queue_plugins": {"<name>": false}`. Every queue backend is a queue plugin - an HTTP
sidecar process implementing the queue-plugin protocol; see [queue-plugins.md](queue-plugins.md) for the protocol.

**With no plugins configured it is `{}`, which is completely normal** and can be ignored. A plugin showing `false` only affects
targets of the form `queue:<plugin>/*`; `node:*` runs are not affected at all.

### How to confirm

```bash
mldojo health                                        # queue_plugins: {"<name>": false}
curl -s <plugin-url>/health                          # ask the plugin process directly
```

`<plugin-url>` is the address configured for that plugin: `api.queue_plugins: {<name>: <url>}` in config.yaml,
or the environment variable `MLDOJO_QUEUE_PLUGINS="name=url,name2=url2"` (if the name is in neither, it won't appear in health at all).

### How to fix

- **The plugin process is not running**: start it. A private plugin usually ships its own installer and systemd unit (in a native
  deployment `deploy/native/install.sh` installs it via `MLDOJO_PLUGIN_DIRS`); troubleshoot with `systemctl --user status <unit>`
  and the plugin's own logs.
- **Wrong URL**: make sure the address in `api.queue_plugins` / `MLDOJO_QUEUE_PLUGINS` is reachable from the **API host**
  (the api container under compose), then restart the API.
- **Queue not registered**: submitting fails with ``unknown queue "x" (see `mldojo queue ls`)``; first run
  `mldojo queue add --id <plugin>/<queue> --backend <plugin> ...`.

**Note for compose users**: compose does not include any plugin. Run the plugin separately, then set
`MLDOJO_QUEUE_PLUGINS` on the api container.

### Verify the whole path with the mock plugin

When you can't tell whether the problem is in the plugin or in MLDojo, use the bundled mock plugin (it runs jobs as local subprocesses):

```bash
python3 adapters/queue_sidecar/mock/server.py --host 127.0.0.1 --port 8766
MLDOJO_QUEUE_PLUGINS=mock=http://127.0.0.1:8766     # set on the API, then restart it
mldojo queue add --id mock/default --backend mock
mldojo run submit -f recipe.yaml --target queue:mock/default
```

If the mock works, the API side is fine and the problem is in your plugin or the scheduler behind it.

---

## Symptom: cannot connect to the database / migration fails

Any API startup failure exits with 1, with the error on stderr prefixed with `mldojo-api:`.
Migrations run both at `serve` startup and in the `migrate` subcommand; **a failed migration means a failed startup**.

### How to confirm

```bash
journalctl --user -u mldojo-api -n 50      # native deployment
docker compose logs api --tail 50          # compose
curl -s http://<server>/api/v1/health       # when the "db" field is not "ok", it is the raw driver error
```

### Fix by error message

| Error | Cause / fix |
|---|---|
| `mldojo-api: database_url is not set (api.database_url in ~/.mldojo/config.yaml or MLDOJO_DATABASE_URL)` | No connection string. In a native deployment `install-postgres.sh` writes it to `~/.mldojo/postgres.url`, and `install.sh` then writes it into the config |
| `connect postgres: ...` | Cannot connect. The ping timeout is 10 seconds |
| `database url: ...` | The connection string itself is malformed (`pgxpool.ParseConfig` failed) |
| `migration <NNN_xxx.sql>: ...` | A migration failed. Each migration runs **in its own transaction** and is rolled back on failure, so the database is never left half-migrated |

To check the connection itself (the natively installed PostgreSQL lives in `~/opt/pgsql-16/bin`, **not on PATH**):

```bash
systemctl --user status mldojo-postgres
cat ~/.mldojo/postgres.url
~/opt/pgsql-16/bin/psql "$(cat ~/.mldojo/postgres.url)" -c 'select 1'
```

compose:

```bash
docker compose ps postgres                 # expect healthy
docker compose exec postgres pg_isready -U mldojo
```

Applied migrations are recorded in the `schema_migrations` table and run in lexicographic filename order:

```bash
psql "$MLDOJO_DATABASE_URL" -c 'select * from schema_migrations order by version'
```

**Do not edit tables by hand after a failed migration.** First find out which file failed; to go back to a known good state, use
the restore procedure in [deploy.md](deploy.md) (restore section).

> `mldojo-api db create` / `db drop` exist but are mainly for tests (`scripts/e2e.sh` uses them to create a temporary database).
> **`db drop` deletes the entire database**; don't slip up in production.

---

## Still not solved

1. **Use e2e as a baseline**: `scripts/e2e.sh` starts a fully isolated instance on the current host
   (its own port, database, agents and mock queue plugin) and cleans up afterwards.
   If it passes, this machine's infrastructure is fine and the problem is in your config or recipe; if it fails, look at the section where the first `✗` appears.
   `KEEP=1 scripts/e2e.sh` keeps the instance around for manual inspection.
2. **Enable server debug logs**: `MLDOJO_LOG_LEVEL=debug`, then restart the API.
3. **`--json`**: every command supports it, and errors are structured too (`{"error": ..., "code": ..., "exit_code": N}`),
   which is easier to work with than human-readable output.
4. **Check versions**: if the `version` in `mldojo health` doesn't match the agent version in `mldojo node ls`,
   upgrade the agent with `mldojo node upgrade <id>` (running runs are taken over by the new agent without interruption).
