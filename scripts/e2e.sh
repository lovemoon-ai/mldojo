#!/usr/bin/env bash
# End-to-end test of a throwaway MLDojo instance on this host. It
# uses its own port, database, config dir and agents, a mock
# queue plugin, and cleans up after itself. Needs: make build dist (and
# optionally make web), a postgres URL in ~/.mldojo/postgres.url (or
# E2E_PG_URL), and key-based `ssh $USER@127.0.0.1` for the SSH/ProxyJump tests.
#
#   scripts/e2e.sh            # run everything
#   KEEP=1 scripts/e2e.sh     # leave the instance running for inspection
# No pipefail: `cmd | grep -q` legitimately SIGPIPEs cmd.
set -u
ROOT=$(cd "$(dirname "$0")/.." && pwd)
BIN="$ROOT/bin"
PORT=${E2E_PORT:-18765}
SIDECAR_PORT=${E2E_SIDECAR_PORT:-18766}
WORK=$(mktemp -d /tmp/mldojo-e2e.XXXXXX)
export MLDOJO_HOME="$WORK/home"
export MLDOJO_SERVER="http://127.0.0.1:$PORT"
export MLDOJO_NO_KEYCHAIN=1
mkdir -p "$MLDOJO_HOME" "$WORK/bin"
# A dev box may keep postgres installed but stopped; start it on demand so
# the suite still runs there.
if [ -z "${E2E_PG_URL:-}" ] && ! systemctl --user is-active --quiet mldojo-postgres 2>/dev/null; then
  systemctl --user start mldojo-postgres 2>/dev/null && sleep 2
fi
BASE_URL=${E2E_PG_URL:-$(cat "$HOME/.mldojo/postgres.url")}
DB_URL=$(echo "$BASE_URL" | sed -E 's#/[A-Za-z0-9_]+(\?|$)#/mldojo_e2e\1#')
MOUNT="/tmp/mldojo-e2e-mount-$$"
LOCAL=e2e-local
SSHN=e2e-ssh

pass=0; fail=0; failed=()
ok()  { echo "  ✓ $*"; pass=$((pass+1)); }
bad() { echo "  ✗ $*"; fail=$((fail+1)); failed+=("$*"); }
check() { local d=$1; shift; if "$@" >/dev/null 2>&1; then ok "$d"; else bad "$d"; fi; }
T0=$(date +%s)
section() { echo; echo "== $*  [+$(( $(date +%s) - T0 ))s]"; }
M() { "$BIN/mldojo" "$@"; }
# py <expr> : evaluate a python expression with the JSON on stdin as d
py() { python3 -c 'import json,sys; d=json.load(sys.stdin); r=eval(sys.argv[1]); print(r if not isinstance(r,(dict,list)) else json.dumps(r))' "$1"; }
exit_code() { "$@" >/dev/null 2>&1; echo $?; }
wait_status() { # run-id wanted-status timeout
  local id=$1 want=$2 t=${3:-60} s=""
  for _ in $(seq "$t"); do
    s=$(M run status "$id" --json | py 'd["status"]')
    [ "$s" = "$want" ] && return 0
    case "$s" in succeeded|failed|cancelled) [ "$s" != "$want" ] && { echo "    (status $s)"; return 1; };; esac
    sleep 1
  done
  echo "    (status $s after ${t}s)"; return 1
}
logs() { M run logs "$1" --stream "${2:-stdout}"; }
submit() { M run submit "$@" --json | py 'd["runs"][0]["id"]'; }

API_PID=""; SIDECAR_PID=""
start_api() {
  "$BIN/mldojo-api" serve >>"$WORK/api.log" 2>&1 &
  API_PID=$!
  for _ in $(seq 30); do curl -fsS "$MLDOJO_SERVER/api/v1/health" >/dev/null 2>&1 && return 0; sleep 0.5; done
  echo "API failed to start:"; tail -20 "$WORK/api.log"; exit 1
}
cleanup() {
  section "cleanup"
  if [ -z "${KEEP:-}" ]; then
    for n in $LOCAL $SSHN e2e-rt; do M node rm "$n" --force >/dev/null 2>&1 && echo "  removed node $n"; done
    [ -n "$API_PID" ] && kill "$API_PID" 2>/dev/null
    [ -n "$SIDECAR_PID" ] && kill "$SIDECAR_PID" 2>/dev/null
    sleep 1
    "$BIN/mldojo-api" db drop >/dev/null 2>&1
    rm -rf "$HOME/.mldojo/datasets/e2e-tiny" "$HOME/.mldojo/runs/$LOCAL" "$HOME/.mldojo/runs/$SSHN" "$MOUNT"
    rm -rf "$WORK"
  else
    echo "  KEEP=1: instance at $MLDOJO_SERVER, MLDOJO_HOME=$MLDOJO_HOME, logs in $WORK"
  fi
}
trap cleanup EXIT

section "setup ($WORK)"
cat > "$MLDOJO_HOME/config.yaml" <<YAML
api:
  listen: 127.0.0.1:$PORT
  database_url: $DB_URL
  public_url: http://127.0.0.1:$PORT
  data_dir: $WORK/data
  web_dir: $ROOT/web/out
  agent_dist: $ROOT/dist
  queue_plugins: {mock: "http://127.0.0.1:$SIDECAR_PORT"}
  master_key_file: $WORK/master.key
  queue_poll_sec: 1
YAML
"$BIN/mldojo-api" db drop >/dev/null 2>&1
check "create database" "$BIN/mldojo-api" db create
python3 "$ROOT/adapters/queue_sidecar/mock/server.py" --host 127.0.0.1 --port "$SIDECAR_PORT" --state-dir "$WORK/sidecar" >"$WORK/sidecar.log" 2>&1 &
SIDECAR_PID=$!
start_api
# Migrations run at startup; if they failed the schema is half-built and every
# later assertion fails for the wrong reason. Stop here instead.
if ! "$BIN/mldojo-api" migrate >/dev/null 2>"$WORK/migrate.err"; then
  echo "  ✗ migrations failed -- refusing to run the suite against a half-built schema:"
  sed 's/^/      /' "$WORK/migrate.err"
  exit 1
fi
export MLDOJO_TOKEN=$("$BIN/mldojo-api" token issue)
[ -n "$MLDOJO_TOKEN" ] || { echo "  ✗ no API token issued; the rest of the suite would only show 401s"; exit 1; }
M health --json | grep -q '"ok": true' && ok "health ok" || bad "health ok"
check "queue plugin (mock) healthy" bash -c "curl -fsS $MLDOJO_SERVER/api/v1/health | grep -q '\"queue_plugins\":{\"mock\":true}'"

section "CLI basics & exit codes"
check "project create" M project create e2e --description "e2e test"
[ "$(exit_code M project create e2e)" = 5 ] && ok "duplicate project -> exit 5" || bad "duplicate project -> exit 5"
[ "$(exit_code M run show deadbeef)" = 2 ] && ok "unknown run -> exit 2" || bad "unknown run -> exit 2"
[ "$(exit_code env MLDOJO_SERVER=http://127.0.0.1:1 "$BIN/mldojo" project ls)" = 3 ] && ok "unreachable server -> exit 3" || bad "unreachable server -> exit 3"
[ "$(exit_code M run submit -f /nonexistent.yaml --target node:x)" = 2 ] && ok "bad recipe path -> exit 2" || bad "bad recipe path -> exit 2"
[ "$(M project ls --json | py 'd[0]["name"]')" = e2e ] && ok "project ls --json" || bad "project ls --json"

section "secrets"
check "secret set (ssh key from file)" M secret set ssh_keys/e2e --from-file "$HOME/.ssh/id_rsa" --description "e2e key"
echo -n "s3cr3t-value" | M secret set passwords/e2e --stdin >/dev/null && ok "secret set --stdin" || bad "secret set --stdin"
out=$(M secret ls --json)
echo "$out" | grep -q 'secret://ssh_keys/e2e' && ok "secret ls lists refs" || bad "secret ls lists refs"
echo "$out" | grep -q -e 'PRIVATE KEY' -e 's3cr3t' && bad "secret values leaked in ls" || ok "no secret values in ls"
check "secret rm" M secret rm passwords/e2e
check "secret unlock (key file)" M secret unlock --key-file "$WORK/master.key"

section "nodes: local agent, SSH agent (key from secret), ProxyJump, fallback"
check "node add --local" M node add --id $LOCAL --local --labels e2e
check "node add --ssh (identity secret://)" M node add --id $SSHN --ssh "$USER@127.0.0.1" --identity secret://ssh_keys/e2e --labels ssh
route=$(M node add --id e2e-jump --ssh "$USER@127.0.0.1" --identity secret://ssh_keys/e2e --via $SSHN --dry-run --json | py 'd["route"]')
[[ "$route" == *"$SSHN -> e2e-jump"* ]] && ok "ProxyJump via $SSHN ($route)" || bad "ProxyJump via $SSHN (got '$route')"
route=$(M node add --id e2e-fb --ssh "$USER@127.0.0.1" --via no-such-node --via local --dry-run --json | py 'd["route"]')
[ "$route" = "local -> e2e-fb" ] && ok "via fallback to local" || bad "via fallback (got '$route')"
[ "$(exit_code M node add --id e2e-bad --ssh nobody@10.255.255.1 --dry-run)" = 3 ] && ok "unreachable node -> exit 3" || bad "unreachable node -> exit 3"
cat > "$WORK/node.yaml" <<YAML
id: e2e-yaml
labels: [yaml]
connection: {type: ssh, host: 127.0.0.1, user: $USER, identity: "secret://ssh_keys/e2e", via: [{node: no-such-node}, {node: $SSHN}]}
capacity: {cpu: 1}
YAML
route=$(M node add -f "$WORK/node.yaml" --dry-run --json | py 'd["route"]')
[ "$route" = "local -> $SSHN -> e2e-yaml" ] && ok "node add -f node.yaml" || bad "node add -f (got '$route')"
[ "$(exit_code M node show e2e-bad)" = 2 ] && ok "failed node add stores nothing" || bad "failed node add stores nothing"
# Where ~/.mldojo goes is decided before anything is deployed, and a dry run
# shows it. This host already has one, which must be left where it is.
M node add --id e2e-dh --ssh "$USER@127.0.0.1" --identity secret://ssh_keys/e2e --dry-run --json |
  py 'd["probe"]["data_home"]["reason"]' | grep -q "already exists" &&
  ok "dry run says where ~/.mldojo would go (kept: it exists)" || bad "dry run reports the data home"
check "node test $SSHN" M node test $SSHN
check "node add --reverse-tunnel (agent dials back through SSH -R)" M node add --id e2e-rt --ssh "$USER@127.0.0.1" --identity secret://ssh_keys/e2e --reverse-tunnel
id=$(submit -f "$ROOT/recipes/examples/hello/recipe.yaml" --target node:e2e-rt --param steps=3 --exp reverse-tunnel --wait)
[ "$(M run status "$id" --json | py 'd["status"]')" = succeeded ] && ok "run over reverse tunnel" || bad "run over reverse tunnel"
check "node rm e2e-rt" M node rm e2e-rt
n=$(M node ls --json | py 'len([x for x in d if x["agent_status"]=="online"])')
[ "$n" = 2 ] && ok "2 agents online" || bad "agents online = $n"
sleep 6
# Only assert GPU stats where the host actually has a working GPU: a broken
# driver reports no devices, and an empty list is then the correct answer.
if nvidia-smi -L 2>/dev/null | grep -q "^GPU 0"; then
  [ "$(M node gpu $LOCAL --json | py 'len(d)')" -ge 1 ] && ok "node gpu (heartbeat stats)" || bad "node gpu"
else
  ok "node gpu (skipped: no usable GPU on this host)"
fi
M node show $LOCAL --json | py 'd["capacity"]["mldojo_dir"]' | grep -q '^/' &&
  ok "agent reports where ~/.mldojo really lives" || bad "agent reports mldojo_dir"

section "M1 smoke: hello-world matrix on node:$LOCAL"
resp=$(M run submit -f "$ROOT/recipes/examples/hello/recipe.yaml" --target node:$LOCAL --matrix seed --project e2e --wait --json)
[ "$(echo "$resp" | py 'len([r for r in d["runs"] if r["status"]=="succeeded"])')" = 2 ] && ok "2 matrix runs succeeded" || bad "matrix runs: $(echo "$resp" | py '[r["status"] for r in d["runs"]]')"
A=$(echo "$resp" | py 'd["runs"][0]["id"]'); B=$(echo "$resp" | py 'd["runs"][1]["id"]')
logs "$A" | grep -q '^done$' && ok "stdout shipped" || bad "stdout shipped"
logs "$A" stderr | grep -q 'warning-ish' && ok "stderr shipped" || bad "stderr shipped"
logs "$A" system | grep -q 'status -> succeeded' && ok "system log records status" || bad "system log"
[ "$(M run metrics "$A" --key loss --json | py 'len(d["points"])')" = 20 ] && ok "20 loss points (jsonl scan)" || bad "loss points"
[ "$(M run metrics "$A" --key sdk/acc --json | py 'len(d["points"])')" = 20 ] && ok "20 sdk/acc points (python SDK)" || bad "sdk points"
M run artifacts ls "$A" --json | grep -q 'model_step20.ckpt' && ok "checkpoint artifact listed" || bad "checkpoint artifact"
# The sha256 column existed from the start but nothing ever filled it.
M run artifacts ls "$A" --json |
  py '[a["sha256"] for a in d if a["uri"].endswith("model_step20.ckpt")][0]' | grep -qE '^[0-9a-f]{64}$' &&
  ok "artifact checksum recorded" || bad "artifact checksum recorded"
uri=$(M run artifacts ls "$A" --json | py '[a["uri"] for a in d if a["kind"]=="ckpt"][0]')
M run artifacts get "$A" "$uri" --dest "$WORK" >/dev/null && grep -q 'fake checkpoint' "$WORK/model_step20.ckpt" && ok "artifact download via agent" || bad "artifact download"
M run show "$A" --json | py 'd["metadata"]["params"]["seed"]' | grep -qE '^[01]$' && ok "params recorded" || bad "params recorded"
M run logs "$A" --stream all --json | head -1 | grep -q '"stream"' && ok "logs --json frames" || bad "logs --json frames"

section "code provenance: git commit + dirty patch, inline-patch"
R="$WORK/repo"; mkdir -p "$R" && cd "$R" && git init -q -b main && git config user.email e2e@x && git config user.name e2e
echo 'print("v1")' > train.py && git add train.py && git commit -qm init && HEAD_SHA=$(git rev-parse HEAD)
echo 'print("v2 dirty")' > train.py && echo "untracked-ok" > extra.txt
cat > recipe.yaml <<'YAML'
apiVersion: mldojo/v1
kind: Experiment
metadata: {project: e2e, name: git-src}
code: {source: git}
run: {cmd: "python3 train.py && cat extra.txt"}
YAML
cd "$ROOT"
id=$(submit -f "$R/recipe.yaml" --target node:$SSHN --wait)
logs "$id" | grep -q 'v2 dirty' && logs "$id" | grep -q 'untracked-ok' && ok "dirty worktree bundled (SSH agent)" || bad "dirty worktree bundled"
[ "$(M run show "$id" --json | py 'd["code_commit"]')" = "$HEAD_SHA" ] && ok "commit recorded" || bad "commit recorded"
curl -fsS -H "Authorization: Bearer $MLDOJO_TOKEN" "$MLDOJO_SERVER/api/v1/runs/$id/code" | py 'd["patch"]' | grep -q 'v2 dirty' && ok "dirty patch stored" || bad "dirty patch stored"
(cd "$R" && git diff > "$WORK/p.diff" && git checkout -q train.py)
sed -i 's/v2 dirty/v3 patched/' "$WORK/p.diff"
cat > "$WORK/inline.yaml" <<YAML
apiVersion: mldojo/v1
kind: Experiment
metadata: {project: e2e, name: inline-patch}
code: {source: inline-patch, repo: "file://$R", ref: main, patch: p.diff}
run: {cmd: python3 train.py}
YAML
id=$(submit -f "$WORK/inline.yaml" --target node:$LOCAL --wait)
logs "$id" | grep -q 'v3 patched' && ok "inline-patch: agent cloned repo + applied patch" || bad "inline-patch"

section "datasets: registry + first-use relay sync + mount"
mkdir -p "$WORK/data-src/tiny" && echo "dataset-ok" > "$WORK/data-src/tiny/hello.txt"
check "dataset register" M dataset register --name e2e-tiny --version v1 --mount "$MOUNT" --location "node:$LOCAL:$WORK/data-src/tiny" --authoritative
cat > "$WORK/ds.yaml" <<YAML
apiVersion: mldojo/v1
kind: Experiment
metadata: {project: e2e, name: dataset}
datasets: [{name: e2e-tiny, version: v1}]
run: {cmd: 'cat \$MLDOJO_DATASET_E2E_TINY/hello.txt && cat $MOUNT/hello.txt'}
YAML
id=$(submit -f "$WORK/ds.yaml" --target node:$SSHN --wait)
[ "$(logs "$id" | grep -c dataset-ok)" = 2 ] && ok "dataset synced to $SSHN and mounted" || bad "dataset sync/mount: $(logs "$id" system | tail -3)"
M dataset show e2e-tiny@v1 --json | grep -q "\"node\": \"$SSHN\"" && ok "cache location recorded" || bad "cache location recorded"
M dataset push e2e-tiny@v1 --to node:$SSHN --json | grep -q '"path"' && ok "dataset push (cache hit)" || bad "dataset push"
printf 'id: e2e-tiny\nversion: v2\nmount: /data/tiny\nlocations:\n  - {kind: node_path, node: %s, path: %s, authoritative: true}\n  - {kind: bucket, provider: p, bucket: b, path: x/y}\n' $LOCAL "$WORK/data-src/tiny" > "$WORK/ds-v2.yaml"
[ "$(M dataset register -f "$WORK/ds-v2.yaml" --json | py 'len(d["locations"])')" = 2 ] && ok "dataset register -f" || bad "dataset register -f"

section "cancel"
id=$(submit -f "$ROOT/recipes/examples/hello/recipe.yaml" --target node:$LOCAL --param steps=1000 --param seed=7 --exp cancel)
wait_status "$id" running 30 && ok "long run running" || bad "long run running"
sleep 1
check "run cancel" M run cancel "$id"
wait_status "$id" cancelled 30 && ok "run cancelled" || bad "run cancelled"
sleep 2
pgrep -f "mldojo/runs/$LOCAL/$id" >/dev/null && bad "process group killed" || ok "process group killed"

section "run rm + retention"
logdir="$WORK/data/runs/$id"
[ -d "$logdir" ] && ok "run log dir exists before rm" || bad "run log dir exists before rm"
check "run rm" M run rm "$id"
M run show "$id" >/dev/null 2>&1 && bad "run gone after rm" || ok "run gone after rm"
[ -d "$logdir" ] && bad "run logs removed with the run" || ok "run logs removed with the run"
rmid=$(submit -f "$ROOT/recipes/examples/hello/recipe.yaml" --target node:$LOCAL --param steps=1000 --param seed=8 --exp rmforce)
wait_status "$rmid" running 30 && ok "run for rm --force is running" || bad "run for rm --force is running"
M run rm "$rmid" >/dev/null 2>&1 && bad "active run refused without --force" || ok "active run refused without --force"
check "run rm --force cancels then deletes" M run rm "$rmid" --force
sleep 2
pgrep -f "mldojo/runs/$LOCAL/$rmid" >/dev/null && bad "rm --force killed the process" || ok "rm --force killed the process"

section "resilience: agent restart and API restart mid-run"
cat > "$WORK/slow.yaml" <<YAML
apiVersion: mldojo/v1
kind: Experiment
metadata: {project: e2e, name: resilience}
code: {source: local, path: $ROOT/recipes/examples/hello}
run: {cmd: python3 train.py 40, env: {STEP_SLEEP: "0.4"}}
outputs: {metrics: [{type: jsonl, path: outputs/metrics.jsonl}]}
YAML
id=$(submit -f "$WORK/slow.yaml" --target node:$LOCAL)
wait_status "$id" running 30 >/dev/null
sleep 2
systemctl --user restart "mldojo-agent-$LOCAL" && ok "agent restarted mid-run" || bad "agent restart"
sleep 2
kill "$API_PID"; wait "$API_PID" 2>/dev/null; start_api && ok "API restarted mid-run"
wait_status "$id" succeeded 90 && ok "run survived both restarts" || bad "run survived restarts"
sleep 1
n=$(logs "$id" | grep -c '^step ')
[ "$n" = 40 ] && ok "log complete after reconnects (40 step lines)" || bad "log lines after reconnect = $n"
[ "$(M run metrics "$id" --key loss --json | py 'len(d["points"])')" = 40 ] && ok "metrics complete (40 points)" || bad "metrics after reconnect"
id=$(submit -f "$WORK/slow.yaml" --target node:$LOCAL)
wait_status "$id" running 30 >/dev/null
check "node upgrade mid-run (fresh token)" M node upgrade $LOCAL $SSHN
wait_status "$id" succeeded 90 && [ "$(logs "$id" | grep -c '^step ')" = 40 ] && ok "run survived agent upgrade" || bad "run survived agent upgrade"

section "queue plugin (mock sidecar)"
cat > "$WORK/q.yaml" <<'YAML'
docker_image: registry.example.com/ml/cuda-torch:latest
cpu_per_worker: 4
cpu_mem_ratio: 4
wall_time_min: 60
input_bucket: team_lab
output_bucket: team_lab
YAML
check "queue add" M queue add --id mock/mock-q --project-id e2e --defaults-file "$WORK/q.yaml"
curl -fsS -H "Authorization: Bearer $MLDOJO_TOKEN" "$MLDOJO_SERVER/api/v1/queues/resources" |
  py '[(q["plugin"], q["name"], q["unit"]) for q in d["queues"]]' | grep -q '"mock", "default", "cpu"' && ok "queue resources (mock)" || bad "queue resources (mock)"
id=$(submit -f "$ROOT/recipes/examples/hello/recipe.yaml" --target queue:mock/mock-q --exp queue --param steps=10 --wait)
[ "$(M run status "$id" --json | py 'd["status"]')" = succeeded ] && ok "queue run succeeded" || bad "queue run: $(logs "$id" system | tail -3)"
logs "$id" | grep -q 'step 10/10' && ok "queue log snapshot diffed into stdout" || bad "queue log"
[ "$(logs "$id" | grep -c 'step 10/10')" = 1 ] && ok "no duplicated log lines" || bad "duplicated queue log lines"
M run show "$id" --json | grep -q '"log_mode": "near-realtime"' && ok "near-realtime badge metadata" || bad "near-realtime metadata"
[ "$(M run metrics "$id" --key loss --json | py 'len(d["points"])')" -ge 1 ] && ok "queue metrics via sidecar jsonl scan" || bad "queue metrics"
auri=$(M run artifacts ls "$id" --json | py '[a["uri"] for a in d if a["kind"]=="ckpt"][0]')
[[ "$auri" == mock://*/outputs/model_step10.ckpt ]] && ok "queue artifacts listed via sidecar ($auri)" || bad "queue artifacts (got '$auri')"
rm -f "$WORK/model_step10.ckpt"; M run artifacts get "$id" "$auri" --dest "$WORK" >/dev/null && grep -q 'fake checkpoint' "$WORK/model_step10.ckpt" \
  && ok "queue artifact temporary download (preview)" || bad "queue artifact download"
id=$(submit -f "$ROOT/recipes/examples/hello/recipe.yaml" --target queue:mock/mock-q --exp queue-cancel --param steps=1000)
wait_status "$id" running 30 >/dev/null
check "queue cancel" M run cancel "$id"
[ "$(M run status "$id" --json | py 'd["status"]')" = cancelled ] && ok "queue run cancelled" || bad "queue cancel"

section "wandb shim + anomaly detection"
mkdir -p "$WORK/wb" && cat > "$WORK/wb/train.py" <<'PY'
import wandb
run = wandb.init(project="x", config={"lr": 0.1})
for i in range(6):
    wandb.log({"loss": float("nan") if i == 3 else 1.0 / (i + 1), "acc": i / 6})
wandb.finish()
print("wandb-shim-ok", wandb.__version__)
PY
cat > "$WORK/wb/recipe.yaml" <<'YAML'
apiVersion: mldojo/v1
kind: Experiment
metadata: {project: e2e, name: wandb}
run: {cmd: python3 train.py, wandb: shim}
YAML
id=$(submit -f "$WORK/wb/recipe.yaml" --target node:$LOCAL --wait)
logs "$id" | grep -q 'wandb-shim-ok 0.0.0-mldojo-shim' && ok "import wandb -> mldojo shim" || bad "wandb shim import"
[ "$(M run metrics "$id" --key loss --json | py 'len(d["points"])')" = 6 ] && ok "wandb.log -> 6 loss points (incl. NaN)" || bad "wandb metrics"
M ai brief "$id" --json | grep -q 'nan_loss@step_3' && ok "ai brief flags nan_loss@step_3" || bad "nan anomaly"
check "ai anomaly-check" M ai anomaly-check "$id"

section "env: venv"
mkdir -p "$WORK/venv" && : > "$WORK/venv/requirements.txt"
cat > "$WORK/venv/recipe.yaml" <<'YAML'
apiVersion: mldojo/v1
kind: Experiment
metadata: {project: e2e, name: venv}
env: {default: {type: venv, spec: requirements.txt}}
run: {cmd: 'python -c "import sys; print(sys.prefix)"'}
YAML
id=$(submit -f "$WORK/venv/recipe.yaml" --target node:$LOCAL --wait)
logs "$id" | grep -q '.mldojo/envs/venv-' && ok "venv created and activated" || bad "venv: $(logs "$id" stderr | tail -2)"
# The resolved dependency list, not just the name of a spec file: the same
# environment.yaml resolves to different versions later.
for _ in $(seq 20); do
  M run show "$id" --json | py 'd["metadata"].get("env_lock","")' | grep -q . && break
  sleep 1
done
M run show "$id" --json | py 'd["metadata"].get("env_lock","")' | grep -q . &&
  ok "environment lock captured" || bad "environment lock captured"

section "compare / AI endpoints"
M compare "$A" "$B" --json | grep -q 'params.seed' && ok "compare shows param diff" || bad "compare"
# Comparison used to stop at two runs and return no curves at all.
third=$(M run ls --limit 3 --json | py 'd[2]["id"]')
[ "$(M compare "$A" "$B" "$third" --json 2>/dev/null | py 'len(d["runs"])')" = 3 ] &&
  ok "compare takes more than two runs" || bad "compare takes more than two runs"
curl -fsS -H "Authorization: Bearer $MLDOJO_TOKEN" "$MLDOJO_SERVER/api/v1/compare/many?runs=$A,$B&max_points=50" |
  py 'len(d["runs"][0]["series"]) > 0' | grep -q True && ok "compare/many returns metric series" || bad "compare/many series"
curl -fsS -H "Authorization: Bearer $MLDOJO_TOKEN" "$MLDOJO_SERVER/api/v1/compare/many?runs=$A,$B" |
  py 'd["param_keys"]' | grep -q seed && ok "compare/many exposes param keys" || bad "compare/many param keys"

M ai summarize-exp e2e/hello-world --json | py 'd["best_run"]["id"]' | grep -qE "$A|$B" && ok "ai summarize-exp picks a best run" || bad "summarize-exp"
M ai brief "$A" --json | py 'd["best_ckpt"]["uri"]' | grep -q ckpt && ok "ai brief best_ckpt" || bad "ai brief best_ckpt"
M ai free-nodes --json | grep -q "$LOCAL" && ok "ai free-nodes" || bad "ai free-nodes"
# The experiment page pivots on this: every run's params against one metric,
# which is what turns four rows in a list into a 2x2 anyone can read.
curl -fsS -H "Authorization: Bearer $MLDOJO_TOKEN" "$MLDOJO_SERVER/api/v1/ai/experiments/e2e/hello-world/summary" |
  py 'all("seed" in (r.get("params") or {}) for r in d["ranking"]) and len(d["ranking"]) >= 2' | grep -q True &&
  ok "experiment summary carries every run's params for the matrix" || bad "experiment summary params"
curl -fsS -H "Authorization: Bearer $MLDOJO_TOKEN" "$MLDOJO_SERVER/api/v1/ai/experiments/e2e/hello-world/summary" |
  py 'd["metric"] != ""' | grep -q True &&
  ok "experiment summary names the metric the cells hold" || bad "experiment summary metric"
# A run can have no number at all -- cancelled, or dead before it logged
# anything. It sorts last, and reading the last run as "the worst" panicked
# the whole endpoint on the first real experiment that had one, which is
# every experiment anybody has iterated on.
M run submit -f "$ROOT/recipes/examples/hello/recipe.yaml" --target node:$LOCAL --matrix seed \
  --param steps=3 --project e2e --exp mixed --wait --json >/dev/null
cat > "$WORK/noresult.yaml" <<YAML
apiVersion: mldojo/v1
kind: Experiment
metadata: {project: e2e, name: mixed}
code: {source: none}
env: {default: {type: none}}
run: {cmd: "exit 1"}
resources: {default: {gpus: 0}}
YAML
nr=$(submit -f "$WORK/noresult.yaml" --target node:$LOCAL)
wait_status "$nr" failed 30 >/dev/null
mixed=$(curl -sS -H "Authorization: Bearer $MLDOJO_TOKEN" "$MLDOJO_SERVER/api/v1/ai/experiments/e2e/mixed/summary")
echo "$mixed" | py 'len(d["ranking"]) == 3 and d["ranking"][-1]["value"] is None and d["best_run"] is not None' | grep -q True &&
  ok "a run with no result does not take the summary down with it" ||
  bad "summary survives a run with no result: $(echo "$mixed" | head -c 400)"
id=$(M ai submit '{"project":"e2e","exp":"ai-loose","target":"node:'$LOCAL'","cmd":"echo loose-$SEED","seeds":[1,2]}' --json | py 'd["runs"][0]["id"]')
wait_status "$id" succeeded 30 && logs "$id" | grep -qE 'loose-[12]' && ok "ai loose submit (seeds matrix, no code)" || bad "ai loose submit"
[ "$(exit_code M run submit -f "$ROOT/recipes/examples/hello/recipe.yaml" --target node:$LOCAL --gpus 4)" = 2 ] && ok "resource check rejects gpus=4 on 1-GPU node" || bad "resource check"
[ "$(M run submit -f "$ROOT/recipes/examples/hello/recipe.yaml" --target queue:mock/mock-q --matrix seed --dry-run --json | py '[r["status"] for r in d["runs"]]')" = '["dry-run", "dry-run"]' ] \
  && ok "dry-run resolves the matrix without creating runs" || bad "dry-run"

section "rerun"
rer=$(M run rerun "$A" --json | py 'd["runs"][0]["id"]')
[ -n "$rer" ] && ok "rerun created a run" || bad "rerun created a run"
[ "$(M run show "$rer" --json | py 'd["metadata"]["cmd"]')" = "$(M run show "$A" --json | py 'd["metadata"]["cmd"]')" ] &&
  ok "rerun reuses the recorded command" || bad "rerun reuses the recorded command"
[ "$(M run show "$rer" --json | py 'd["target"]')" = "$(M run show "$A" --json | py 'd["target"]')" ] &&
  ok "rerun keeps the original target" || bad "rerun keeps the original target"
wait_status "$rer" succeeded 60 && ok "rerun actually runs" || bad "rerun actually runs"
rer2=$(M run rerun "$A" --param seed=99 --json | py 'd["runs"][0]["id"]')
[ "$(M run show "$rer2" --json | py 'd["metadata"]["params"]["seed"]')" = 99 ] &&
  ok "rerun --param overrides" || bad "rerun --param overrides"

section "scheduling: pools and limits"
# A pool target names a capability, not a machine. The local node already
# carries the label "e2e" (see the nodes section).
[ "$(exit_code M run submit -f "$ROOT/recipes/examples/hello/recipe.yaml" --target pool:no-such-label --exp pool)" = 2 ] &&
  ok "a pool nothing matches is refused at submit" || bad "impossible pool target is refused"
pid=$(submit -f "$ROOT/recipes/examples/hello/recipe.yaml" --target "pool:e2e" --param steps=3 --exp pool)
[ -n "$pid" ] && ok "pool run submitted" || bad "pool run submitted"
wait_status "$pid" succeeded 60 && ok "scheduler placed and ran a pool run" || bad "scheduler placed a pool run"
[ "$(M run show "$pid" --json | py 'd["backend_id"]')" = "$LOCAL" ] &&
  ok "pool run records where it landed" || bad "pool run records where it landed"

# Concurrency limits: the second run waits instead of piling on.
check "set node limit" M node limit "$LOCAL" --max-runs 1
a1=$(submit -f "$ROOT/recipes/examples/hello/recipe.yaml" --target node:$LOCAL --param steps=600 --exp limits)
wait_status "$a1" running 30 && ok "first run started" || bad "first run started"
a2=$(submit -f "$ROOT/recipes/examples/hello/recipe.yaml" --target node:$LOCAL --param steps=600 --exp limits)
sleep 5
[ "$(M run show "$a2" --json | py 'd["status"]')" = queued ] &&
  ok "node limit keeps the second run queued" || bad "node limit keeps the second run queued"
M run cancel "$a1" >/dev/null 2>&1
# The reconcile loop must pick it up once there is room, with no agent reconnect.
for _ in $(seq 45); do
  [ "$(M run show "$a2" --json | py 'd["status"]')" != queued ] && break
  sleep 1
done
[ "$(M run show "$a2" --json | py 'd["status"]')" != queued ] &&
  ok "reconcile starts the queued run when room frees up" || bad "reconcile starts the queued run"
M run cancel "$a2" >/dev/null 2>&1

# Two runs queued at the same instant, which is what a matrix submit or a
# sweep produces. Counting queued runs against the limit refused every one of
# them, and since none could start the count never dropped: the node sat idle
# with work waiting on it, forever.
burst=$(M run submit -f "$ROOT/recipes/examples/hello/recipe.yaml" --target node:$LOCAL --matrix seed \
  --param steps=600 --exp burst --json | py '" ".join(r["id"] for r in d["runs"])')
[ "$(echo "$burst" | wc -w)" -ge 2 ] && ok "matrix queued two runs at once" || bad "matrix queued two runs at once"
running=""
for _ in $(seq 45); do
  for i in $burst; do
    [ "$(M run status "$i" --json | py 'd["status"]')" = running ] && running=$i && break
  done
  [ -n "$running" ] && break
  sleep 1
done
[ -n "$running" ] && ok "a burst of queued runs still starts under a node limit" ||
  bad "a burst of queued runs deadlocked the node"
# ...and only one of them. Admission is a read then a write; submitted
# together, all of them counted the node before any had marked itself
# starting, and a node limited to one run started three.
started=0
for i in $burst; do
  case "$(M run status "$i" --json | py 'd["status"]')" in starting|running) started=$((started+1));; esac
done
[ "$started" -le 1 ] && ok "the node limit admits one at a time" || bad "node limit admitted $started at once"
for i in $burst; do M run cancel "$i" >/dev/null 2>&1; done
check "clear node limit" M node limit "$LOCAL" --max-runs 0

section "retry and node disk"
# A run the platform lost is retried; a run that failed on its own exit code
# is not, because retrying a bug just runs it again.
cat > "$WORK/retry.yaml" <<YAML
apiVersion: mldojo/v1
kind: Experiment
metadata: {project: e2e, name: retry}
code: {source: none}
env: {default: {type: none}}
run: {cmd: "exit 3"}
retry: {max: 2}
resources: {default: {gpus: 0}}
YAML
rt=$(submit -f "$WORK/retry.yaml" --target node:$LOCAL)
wait_status "$rt" failed 30 >/dev/null
sleep 2
[ "$(M run ls --project e2e --exp retry --json | py 'len(d)')" = 1 ] &&
  ok "a job that failed on its own is not retried" || bad "job failure is not retried"
M run logs "$rt" --stream system | grep -q "not retrying" &&
  ok "the log says why it was not retried" || bad "log says why not retried"
# retry.on any opts in; two retries means three runs in total.
sed 's/retry: {max: 2}/retry: {max: 2, on: any}/' "$WORK/retry.yaml" > "$WORK/retry-any.yaml"
ra=$(submit -f "$WORK/retry-any.yaml" --target node:$LOCAL --exp retry-any)
for _ in $(seq 60); do
  n=$(M run ls --project e2e --exp retry-any --json | py 'len(d)')
  [ "$n" = 3 ] && break
  sleep 1
done
[ "$n" = 3 ] && ok "retry.on any produces three attempts" || bad "retry.on any (got $n runs)"
M run ls --project e2e --exp retry-any --json | py '[r["name"] for r in d if "attempt 3" in r["name"]] != []' | grep -q True &&
  ok "attempts are named" || bad "attempts are named"
M run show "$ra" --json | py 'd["metadata"]["attempt"]' | grep -q '^1$' &&
  ok "the first attempt is attempt 1" || bad "first attempt numbered"

# min_mem_gb used to be a comment: on a node: target it was compared against
# the card's total memory, once, at submit time. A card somebody else was
# holding passed the check and the job OOMed on arrival. Admission now asks
# what is free at the moment the run would start -- and a card one of our own
# runs is on is not free either.
if nvidia-smi -L 2>/dev/null | grep -q "^GPU 0"; then
  hog=$(submit -f "$ROOT/recipes/examples/hello/recipe.yaml" --target node:$LOCAL --gpus 1 --param steps=600 --exp gpu-admit)
  wait_status "$hog" running 30 && ok "a run took the card" || bad "a run took the card"
  sleep 7   # let a heartbeat report the assignment
  wait2=$(submit -f "$ROOT/recipes/examples/hello/recipe.yaml" --target node:$LOCAL --gpus 1 --param steps=5 --exp gpu-admit)
  sleep 5
  [ "$(M run show "$wait2" --json | py 'd["status"]')" = queued ] &&
    ok "the second run waits for the card instead of oversubscribing it" || bad "second GPU run was admitted anyway"
  M run show "$wait2" --json | py 'd["metadata"]["message"]' | grep -qi "cards" &&
    ok "the run says which resource it is waiting for" || bad "waiting message names the resource"
  M run cancel "$hog" >/dev/null 2>&1
  wait_status "$wait2" succeeded 90 && ok "it starts once the card is free" || bad "queued GPU run never started"
else
  echo "  (no GPU on $LOCAL; GPU admission skipped)"
fi

M node disk $LOCAL --json | py 'len(d["usage"]["entries"]) > 0' | grep -q True &&
  ok "node disk lists run directories" || bad "node disk lists run directories"
M node disk $LOCAL --json | py 'any(e["run_id"] == "'"$A"'" for e in d["usage"]["entries"])' | grep -q True &&
  ok "node disk accounts a known run" || bad "node disk accounts a known run"
# The accounting commands had no coverage at all, and `mldojo usage` failed
# with no flags: its own default window is "7d", which Go durations reject.
[ "$(exit_code M usage)" = 0 ] && ok "usage runs with its default window" || bad "usage with no flags"
[ "$(exit_code M usage --by node --since 2d)" = 0 ] && ok "usage takes a window in days" || bad "usage --since 2d"
[ "$(exit_code M gpu idle --since 7d)" = 0 ] && ok "gpu idle takes a window in days" || bad "gpu idle --since 7d"
[ "$(exit_code M node history $LOCAL --since 1d)" = 0 ] && ok "node history takes a window in days" || bad "node history --since 1d"
purged=$(submit -f "$ROOT/recipes/examples/hello/recipe.yaml" --target node:$LOCAL --param steps=2 --exp purge)
wait_status "$purged" succeeded 60 >/dev/null
M run rm "$purged" --purge-node --json | py 'd[0].get("purged_node")' | grep -q "$LOCAL" &&
  ok "run rm --purge-node reclaims the node workdir" || bad "run rm --purge-node"
M node disk $LOCAL --json | py 'any(e["run_id"] == "'"$purged"'" for e in d["usage"]["entries"])' | grep -q False &&
  ok "the purged directory is gone from the node" || bad "purged directory gone"

section "evaluation: episodes"
# A policy's result is a rate over trials, not a curve over steps. The
# harness writes a manifest and named video files and knows nothing about
# MLDojo; outputs.episodes is what picks them up.
ev=$(M run submit -f "$ROOT/recipes/examples/eval/recipe.yaml" --target node:$LOCAL \
  --matrix setting --exp eval --json | py '" ".join(r["id"] for r in d["runs"])')
[ "$(echo "$ev" | wc -w)" = 2 ] && ok "eval matrix expanded to clean and randomized" || bad "eval matrix expanded"
evok=1
for i in $ev; do wait_status "$i" succeeded 90 || evok=""; done
[ -n "$evok" ] && ok "both evaluations ran" || bad "both evaluations ran"
clean=$(echo "$ev" | cut -d' ' -f1)
rand=$(echo "$ev" | cut -d' ' -f2)
[ "$(M run episodes "$clean" --json | py 'd["summary"]["total"]')" = 6 ] &&
  ok "episodes collected from the manifest" || bad "episodes collected"
# The seed comes from the CSV and the video from the filenames, so getting
# both on one row proves the two sources merged.
M run episodes "$clean" --json | py 'd["episodes"][0]["seed"]' | grep -q '^100000$' &&
  ok "episode seed read from the manifest" || bad "episode seed read"
M run episodes "$clean" --json | py 'sum(1 for e in d["episodes"] if e.get("video_uri"))' | grep -q '^6$' &&
  ok "every episode carries its video" || bad "episodes carry videos"
M run episodes "$clean" --result failure --json | py 'all(not e["success"] for e in d["episodes"])' | grep -q True &&
  ok "episodes filter by result" || bad "episodes filter by result"
# The result is published as a metric, which is what makes ranking and the
# AI summaries work on an evaluation without knowing about episodes.
M run metrics "$clean" --key eval/success_rate --json | py 'd["points"][-1]["value"] > 0' | grep -q True &&
  ok "success rate published as a metric" || bad "success rate published"
M ai brief "$clean" --json | py 'd["eval"]["total"]' | grep -q '^6$' &&
  ok "ai brief reports the evaluation" || bad "ai brief reports the evaluation"
# Clean and randomized use different odds, so trials must disagree.
M compare episodes "$clean" "$rand" --json | py 'd["flipped"] > 0' | grep -q True &&
  ok "compare finds the trials that flipped" || bad "compare finds flipped trials"
M compare episodes "$clean" "$rand" --json | py 'all(e["seed"] >= 100000 for e in d["episodes"])' | grep -q True &&
  ok "evaluations are joined on seed" || bad "evaluations joined on seed"

section "model registry"
ckpt=$(M run artifacts ls "$A" --json | py '[a["uri"] for a in d if a["uri"].endswith(".ckpt")][0]')
M model register e2e/policy --run "$A" --uri "$ckpt" --json | py 'd["version"]' | grep -q '^1$' &&
  ok "model version registered" || bad "model version registered"
# Lineage: which run produced this, with its metrics frozen alongside.
M model show e2e/policy --json | py 'd["versions"][0]["run_id"]' | grep -q . &&
  ok "model version records its run" || bad "model version records its run"
M model show e2e/policy --json | py 'len(d["versions"][0].get("metrics") or {}) > 0' | grep -q True &&
  ok "model version snapshots the run metrics" || bad "model version snapshots metrics"
M model show e2e/policy --json | py 'd["versions"][0]["sha256"]' | grep -qE '^[0-9a-f]{64}$' &&
  ok "model version carries the artifact checksum" || bad "model version checksum"
curl -fsS -H "Authorization: Bearer $MLDOJO_TOKEN" "$MLDOJO_SERVER/api/v1/runs/$A/models" | py 'len(d)' | grep -q '^1$' &&
  ok "run reports what it produced" || bad "run reports what it produced"
M model register e2e/policy --run "$B" --uri "$ckpt" --json | py 'd["version"]' | grep -q '^2$' &&
  ok "versions increment per model" || bad "versions increment"
check "promote v1 to production" M model promote e2e/policy 1 --stage production
check "promote v2 to production" M model promote e2e/policy 2 --stage production
# Production holds one version, so "what is deployed" has a single answer.
[ "$(M model show e2e/policy --json | py '[v["stage"] for v in d["versions"]]')" = '["production", "archived"]' ] &&
  ok "promoting archives the previous production version" || bad "promotion is exclusive"
[ "$(exit_code M model promote e2e/policy 1 --stage nonsense)" = 2 ] &&
  ok "unknown stage is refused" || bad "unknown stage is refused"

# Lineage, closed. The registry existed before this and the first real task
# went around it: pasting a checkpoint path is cheaper than registering one,
# so registering has to cost nothing and referencing has to be the easy way.
cat > "$WORK/produce.yaml" <<YAML
apiVersion: mldojo/v1
kind: Experiment
metadata: {project: e2e, name: lineage-train}
code: {source: local, path: $ROOT/recipes/examples/hello}
env: {default: {type: none}}
run: {cmd: "python3 train.py 3", params: {}}
resources: {default: {gpus: 0}}
outputs:
  model: policy-auto
  checkpoints: outputs/*.ckpt
  metrics: [{type: jsonl, path: outputs/metrics.jsonl}]
YAML
tr1=$(submit -f "$WORK/produce.yaml" --target node:$LOCAL)
wait_status "$tr1" succeeded 60 && ok "a run that declares outputs.model finished" || bad "outputs.model run finished"
sleep 2
M model show e2e/policy-auto --json | py 'd["versions"][0]["version"]' | grep -q '^1$' &&
  ok "success registers a version with no extra step" || bad "auto-registered a version"
M model show e2e/policy-auto --json | py 'd["versions"][0]["run_id"]' | grep -q . &&
  ok "the auto-registered version records its run" || bad "auto-registered version records its run"
M model show e2e/policy-auto --json | py 'len(d["versions"][0].get("metrics") or {}) > 0' | grep -q True &&
  ok "the auto-registered version snapshots the metrics" || bad "auto-registered metrics"
M run logs "$tr1" --stream system | grep -q "registered e2e/policy-auto@1" &&
  ok "the run log says what it registered" || bad "run log names the registration"

# ...and the other side: an evaluation names what it evaluates.
cat > "$WORK/consume.yaml" <<YAML
apiVersion: mldojo/v1
kind: Experiment
metadata: {project: e2e, name: lineage-eval}
code: {source: none}
env: {default: {type: none}}
models: [{name: policy-auto, version: latest, as: ckpt}]
run: {cmd: "echo evaluating \${ckpt}"}
resources: {default: {gpus: 0}}
YAML
ev1=$(submit -f "$WORK/consume.yaml" --target node:$LOCAL)
wait_status "$ev1" succeeded 60 && ok "a run that references a model started" || bad "model reference run"
M run logs "$ev1" | grep -q '\.ckpt' &&
  ok "the reference resolved to the checkpoint path" || bad "model reference resolved"
M run show "$ev1" --json | py 'd["metadata"]["models"][0]["version"]' | grep -q '^1$' &&
  ok "the run records which version it read" || bad "run records the version"
# The question the registry could not answer: what was done with this model?
M model show e2e/policy-auto --json | py 'd["used_by"][0]["run_id"]' | grep -q "$ev1" &&
  ok "the model lists the runs that evaluated it" || bad "model lists its consumers"
# A rerun replays the reference, pinned, rather than the path it resolved to.
rev=$(M run rerun "$ev1" --json | py 'd["runs"][0]["id"]')
M run show "$rev" --json | py 'd["metadata"]["models"][0]["version"]' | grep -q '^1$' &&
  ok "rerun replays the model reference" || bad "rerun replays the model reference"
# Overriding the resolved path would leave the run claiming a version it
# never read.
[ "$(exit_code M run submit -f "$WORK/consume.yaml" --target node:$LOCAL --exp lineage-override --param ckpt=/tmp/other)" = 2 ] &&
  ok "--param cannot silently replace a model reference" || bad "--param overrides a model reference"
# An unregistered model fails at submit, not three hours into a job.
sed 's|policy-auto|no-such-model|' "$WORK/consume.yaml" > "$WORK/consume-bad.yaml"
[ "$(exit_code M run submit -f "$WORK/consume-bad.yaml" --target node:$LOCAL --exp lineage-bad)" = 2 ] &&
  ok "referencing an unregistered model is refused at submit" || bad "unregistered model refused"

section "sweep"
cat > "$WORK/sweep.yaml" <<YAML
apiVersion: mldojo/v1
kind: Sweep
metadata: {project: e2e, name: sweep-test}
recipe: $ROOT/recipes/examples/hello/recipe.yaml
target: node:$LOCAL
method: random
metric: {name: loss, goal: min}
budget: {max_runs: 2, max_parallel: 2}
space:
  seed: {int_uniform: [0, 100]}
  steps: {values: [5]}
YAML
M sweep create -f "$WORK/sweep.yaml" --json | grep -q sweep-test && ok "sweep created" || bad "sweep created"
# The controller launches against the budget instead of dumping every point
# on the cluster at once, the way --matrix did.
for _ in $(seq 40); do
  [ "$(M sweep show e2e/sweep-test --json | py 'd["launched"]')" -ge 2 ] && break
  sleep 1
done
[ "$(M sweep show e2e/sweep-test --json | py 'd["launched"]')" = 2 ] &&
  ok "sweep launched its budget" || bad "sweep launched its budget"
[ "$(M run ls --project e2e --exp sweep-test --limit 10 --json | py 'len(d)')" = 2 ] &&
  ok "sweep runs land in their experiment" || bad "sweep runs land in their experiment"
# Sampled values must be inside the declared space.
M run ls --project e2e --exp sweep-test --limit 10 --json |
  py 'all(0 <= r["metadata"]["params"]["seed"] <= 100 for r in d)' | grep -q True &&
  ok "sampled params are inside the space" || bad "sampled params are inside the space"
for _ in $(seq 60); do
  [ "$(M run ls --project e2e --exp sweep-test --status succeeded --limit 10 --json | py 'len(d)')" = 2 ] && break
  sleep 1
done
M sweep show e2e/sweep-test --json | py 'len(d["results"])' | grep -q 2 &&
  ok "sweep leaderboard ranks its runs" || bad "sweep leaderboard ranks its runs"
check "sweep stop" M sweep stop e2e/sweep-test
[ "$(M sweep ls --json | py 'd[0]["status"]')" = stopped ] && ok "stopped sweep stays stopped" || bad "sweep stop"
# A definition that cannot work must be refused, not silently burn GPU hours.
printf 'apiVersion: mldojo/v1\nkind: Sweep\nmetadata: {project: e2e, name: bad}\nrecipe_yaml: "x"\ntarget: node:%s\nmethod: grid\nmetric: {name: loss, goal: min}\nbudget: {max_runs: 1, max_parallel: 1}\nspace:\n  lr: {uniform: [0.1, 0.2]}\n' "$LOCAL" > "$WORK/bad-sweep.yaml"
[ "$(exit_code M sweep create -f "$WORK/bad-sweep.yaml")" = 2 ] &&
  ok "grid over a continuous axis is refused" || bad "grid over a continuous axis is refused"

section "sdk init (a run nobody submitted)"
cat > "$WORK/ext.py" <<'PYEOF'
import os
import sys

sys.path.insert(0, os.environ["SDK_PATH"])
import mldojo

rid = mldojo.init(project="e2e", experiment="sdk-external", name="from-script",
                  config={"lr": 0.001, "arch": "resnet"})
with open(os.environ["OUT"], "w") as f:
    f.write(rid or "")
for i in range(5):
    mldojo.log({"loss": 1.0 / (i + 1)}, step=i)
mldojo.summary["best_loss"] = 0.2
mldojo.finish()
PYEOF
MLDOJO_API_URL="$MLDOJO_SERVER" MLDOJO_TOKEN="$MLDOJO_TOKEN" SDK_PATH="$ROOT/sdk/python" OUT="$WORK/ext.id" \
  python3 "$WORK/ext.py" >"$WORK/ext.log" 2>&1
extid=$(cat "$WORK/ext.id" 2>/dev/null)
[ -n "$extid" ] && ok "sdk init created a run without submitting" || bad "sdk init created a run"
M run show "$extid" --json | py 'd["target"]' | grep -q '^external:' &&
  ok "external run has an external target" || bad "external run target"
[ "$(M run show "$extid" --json | py 'd["experiment"]')" = "sdk-external" ] &&
  ok "sdk init placed the run in its experiment" || bad "sdk init experiment"
# Hyperparameters defined in code, which a recipe never sees.
M run show "$extid" --json | py 'd["metadata"]["params"]["arch"]' | grep -q resnet &&
  ok "sdk config recorded as params" || bad "sdk config recorded"
M run show "$extid" --json | py 'd["metadata"]["params"]' | grep -q 'summary/best_loss' &&
  ok "sdk summary recorded" || bad "sdk summary recorded"
[ "$(M run metrics "$extid" --json | py 'len([p for p in d["points"] if p["key"]=="loss"])')" = 5 ] &&
  ok "sdk metrics arrived over http" || bad "sdk metrics arrived"
[ "$(M run show "$extid" --json | py 'd["status"]')" = succeeded ] &&
  ok "sdk finish closed the run" || bad "sdk finish closed the run"
# Cancelling an external run must not pretend the process was killed.
M run rm "$extid" --force >/dev/null 2>&1 && ok "external run can be removed" || bad "external run removal"

section "run list: sort, page, search"
[ "$(M run ls --limit 2 --json | py 'len(d)')" = 2 ] && ok "limit works" || bad "limit works"
[ "$(M run ls --limit 1 --offset 1 --json | py 'd[0]["id"]')" != "$(M run ls --limit 1 --json | py 'd[0]["id"]')" ] &&
  ok "offset pages past the newest" || bad "offset pages past the newest"
M run ls --sort duration --limit 3 --json >/dev/null 2>&1 && ok "sort by duration" || bad "sort by duration"
[ "$(exit_code M run ls --sort nonsense)" = 2 ] && ok "unknown sort is a user error" || bad "unknown sort is a user error"
[ "$(M run ls --search hello --limit 50 --json | py 'len(d) > 0')" = True ] && ok "name search" || bad "name search"


section "web app"
if [ -f "$ROOT/web/out/index.html" ]; then
  for p in / /run /nodes /experiment /settings /manifest.json /sw.js; do
    code=$(curl -s -o /dev/null -w '%{http_code}' "$MLDOJO_SERVER$p")
    [ "$code" = 200 ] && ok "GET $p" || bad "GET $p -> $code"
  done
  curl -s "$MLDOJO_SERVER/run" | grep -qi '<html' && ok "/run serves run.html" || bad "/run html"
  [ "$(curl -s -o /dev/null -w '%{http_code}' "$MLDOJO_SERVER/nope")" = 404 ] && ok "unknown page -> 404" || bad "404 page"
else
  echo "  (web/out not built; skipped)"
fi
[ "$(curl -s -o /dev/null -w '%{http_code}' "$MLDOJO_SERVER/api/v1/projects")" = 401 ] && ok "API requires token" || bad "API auth"

section "auth endpoints (SSO off in this instance)"
curl -s "$MLDOJO_SERVER/api/v1/auth/config" | grep -q '"sso_enabled":false' && ok "auth/config reports SSO off" || bad "auth/config"
curl -s "$MLDOJO_SERVER/api/v1/auth/me" | grep -q '"authenticated":false' && ok "auth/me anonymous" || bad "auth/me"
[ "$(curl -s -o /dev/null -w '%{http_code}' "$MLDOJO_SERVER/api/v1/auth/login")" = 400 ] && ok "auth/login refuses without SSO config" || bad "auth/login without config"
curl -s -H "Authorization: Bearer $MLDOJO_TOKEN" "$MLDOJO_SERVER/api/v1/auth/me" | grep -q '"mode":"token"' && ok "auth/me reports token mode" || bad "auth/me token mode"

section "browser hardening"
# A cookie-authenticated POST from another origin must be refused; the same
# call with a bearer token is fine, since a cross-site page cannot read it.
[ "$(curl -s -o /dev/null -w '%{http_code}' -X POST -H "Origin: https://evil.example.com" "$MLDOJO_SERVER/api/v1/projects")" = 403 ] &&
  ok "cross-origin cookie POST refused" || bad "cross-origin cookie POST refused"
[ "$(curl -s -o /dev/null -w '%{http_code}' -X POST -H "Origin: https://evil.example.com" -H "Authorization: Bearer $MLDOJO_TOKEN" \
     -d '{"name":"csrf-probe"}' "$MLDOJO_SERVER/api/v1/projects")" = 201 ] &&
  ok "token POST unaffected by origin" || bad "token POST unaffected by origin"
M project rm csrf-probe --force >/dev/null 2>&1
# CORS must not echo an origin this instance does not serve.
curl -s -D- -o /dev/null -H "Origin: https://evil.example.com" "$MLDOJO_SERVER/api/v1/health" |
  grep -qi 'access-control-allow-origin' && bad "CORS echoes any origin" || ok "CORS refuses unknown origin"
curl -s -D- -o /dev/null "$MLDOJO_SERVER/api/v1/health" | grep -qi 'x-content-type-options: nosniff' &&
  ok "security headers present" || bad "security headers present"
curl -s -D- -o /dev/null "$MLDOJO_SERVER/api/v1/health" | grep -qi 'x-request-id:' &&
  ok "request id echoed" || bad "request id echoed"
# Logout used to accept GET, which any page could fire with an <img>.
[ "$(curl -s -o /dev/null -w '%{http_code}' "$MLDOJO_SERVER/api/v1/auth/logout")" = 404 ] &&
  ok "GET logout is gone" || bad "GET logout is gone"

section "authorization + audit"
# The API token is an administrator, so the admin-only routes work with it.
check "admin can list the audit log" bash -c "curl -fsS -H 'Authorization: Bearer $MLDOJO_TOKEN' '$MLDOJO_SERVER/api/v1/audit' | grep -q '\\['"
curl -fsS -H "Authorization: Bearer $MLDOJO_TOKEN" "$MLDOJO_SERVER/api/v1/audit?action=project.delete" |
  grep -q '"actor":"api-token"' && ok "deletes are attributed in the audit log" || bad "audit records the actor"
curl -fsS -H "Authorization: Bearer $MLDOJO_TOKEN" "$MLDOJO_SERVER/api/v1/audit" | grep -q '"action":"run.create"' &&
  ok "run submissions are audited" || bad "run submissions are audited"
curl -fsS -H "Authorization: Bearer $MLDOJO_TOKEN" "$MLDOJO_SERVER/api/v1/audit" | grep -q '"action":"run.delete"' &&
  ok "run deletions are audited" || bad "run deletions are audited"

section "named api tokens"
newtok=$(curl -fsS -X POST -H "Authorization: Bearer $MLDOJO_TOKEN" -H 'Content-Type: application/json' \
  -d '{"name":"e2e-member","role":"member"}' "$MLDOJO_SERVER/api/v1/tokens" | py 'd["token"]')
case "$newtok" in mld_*) ok "named token issued" ;; *) bad "named token issued" ;; esac
curl -fsS -H "Authorization: Bearer $newtok" "$MLDOJO_SERVER/api/v1/runs" >/dev/null 2>&1 &&
  ok "named token authenticates" || bad "named token authenticates"
# A member may read and submit, but not reach the admin-only routes.
[ "$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $newtok" "$MLDOJO_SERVER/api/v1/audit")" = 403 ] &&
  ok "member token refused on an admin route" || bad "member token refused on an admin route"
[ "$(curl -s -o /dev/null -w '%{http_code}' -X POST -H "Authorization: Bearer $newtok" -H 'Content-Type: application/json' \
     -d '{"id":"nope","connection":{"kind":"local"}}' "$MLDOJO_SERVER/api/v1/nodes")" = 403 ] &&
  ok "member token cannot add nodes" || bad "member token cannot add nodes"
# The plaintext must not be readable back out.
curl -fsS -H "Authorization: Bearer $MLDOJO_TOKEN" "$MLDOJO_SERVER/api/v1/tokens" | grep -q "$newtok" &&
  bad "token list leaks the plaintext" || ok "token list does not leak the plaintext"
tid=$(curl -fsS -H "Authorization: Bearer $MLDOJO_TOKEN" "$MLDOJO_SERVER/api/v1/tokens" | py 'd[0]["id"]')
curl -fsS -X DELETE -H "Authorization: Bearer $MLDOJO_TOKEN" "$MLDOJO_SERVER/api/v1/tokens/$tid" >/dev/null 2>&1 &&
  ok "token revoked" || bad "token revoked"
[ "$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $newtok" "$MLDOJO_SERVER/api/v1/runs")" = 401 ] &&
  ok "revoked token stops working" || bad "revoked token stops working"

echo
echo "e2e: $pass passed, $fail failed"
for f in "${failed[@]}"; do echo "  FAILED: $f"; done
[ "$fail" = 0 ]
