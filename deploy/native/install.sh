#!/usr/bin/env bash
# Native MLDojo install as systemd --user services (no root, no docker).
# Run from a checkout after `make build dist web`:
#   MLDOJO_PUBLIC_URL=http://192.0.2.10:8765 deploy/native/install.sh
# Site settings can live in deploy/site.env (see deploy/site.env.example).
# Installs to ~/.mldojo/app, writes ~/.mldojo/config.yaml (api section) if
# missing, (re)starts mldojo-api, and runs <dir>/install.sh for every queue
# plugin checkout in MLDOJO_PLUGIN_DIRS.
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
if [ -f "$ROOT/deploy/site.env" ]; then set -a; . "$ROOT/deploy/site.env"; set +a; fi
APP=${MLDOJO_APP:-$HOME/.mldojo/app}
PORT=${MLDOJO_PORT:-8765}
UNITS="$HOME/.config/systemd/user"

for f in bin/mldojo-api bin/mldojo bin/mldojo-agent dist/mldojo-agent-linux-amd64; do
  [ -f "$ROOT/$f" ] || { echo "missing $f: run 'make build dist' first" >&2; exit 1; }
done
mkdir -p "$APP/bin" "$APP/dist" "$APP/web" "$UNITS" "$HOME/.local/bin"
install -m 755 "$ROOT"/bin/* "$APP/bin/"
install -m 755 "$ROOT"/dist/* "$APP/dist/"
if [ -f "$ROOT/web/out/index.html" ]; then
  rm -rf "$APP/web.new" && cp -r "$ROOT/web/out" "$APP/web.new" && rm -rf "$APP/web" && mv "$APP/web.new" "$APP/web"
else
  echo "warning: web/out not built (make web); the API will serve a placeholder page" >&2
fi
# Served as <public_url>/SKILL.md, so an agent on any node can learn the CLI.
# The {{...}} placeholders become this site's addresses.
SKILL_SERVER=${MLDOJO_DEFAULT_SERVER:-${MLDOJO_PUBLIC_URL:-http://localhost:$PORT}}
sed -e "s#{{MLDOJO_SERVER}}#$SKILL_SERVER#g" -e "s#{{MLDOJO_INTRANET_SERVER}}#${MLDOJO_INTRANET_URL:-$SKILL_SERVER}#g" \
  "$ROOT/skills/mldojo/SKILL.md" > "$APP/web/SKILL.md.new" && chmod 644 "$APP/web/SKILL.md.new" && mv "$APP/web/SKILL.md.new" "$APP/web/SKILL.md"
ln -sf "$APP/bin/mldojo" "$HOME/.local/bin/mldojo"
ln -sf "$APP/bin/mldojo-api" "$HOME/.local/bin/mldojo-api"

CONF="$HOME/.mldojo/config.yaml"
if ! grep -q "database_url" "$CONF" 2>/dev/null; then
  DB_URL=${MLDOJO_DATABASE_URL:-$(cat "$HOME/.mldojo/postgres.url" 2>/dev/null || true)}
  [ -n "$DB_URL" ] || { echo "no database: run deploy/native/install-postgres.sh or set MLDOJO_DATABASE_URL" >&2; exit 1; }
  PUBLIC=${MLDOJO_PUBLIC_URL:-http://$(hostname -I | awk '{print $1}'):$PORT}
  umask 077
  cat >> "$CONF" <<YAML
api:
  listen: 0.0.0.0:$PORT
  database_url: $DB_URL
  public_url: $PUBLIC
  web_dir: $APP/web
  agent_dist: $APP/dist
YAML
  echo "wrote $CONF"
fi

cat > "$UNITS/mldojo-api.service" <<UNIT
[Unit]
Description=MLDojo API server
After=network-online.target mldojo-postgres.service
Wants=mldojo-postgres.service

[Service]
# Optional overrides that survive a redeploy (this unit file does not):
# put MLDOJO_SSO_ALLOW, MLDOJO_SSO_ADMINS, MLDOJO_LOG_FORMAT ... in there.
EnvironmentFile=-$HOME/.mldojo/env
ExecStart=$APP/bin/mldojo-api serve
Restart=always
RestartSec=3

[Install]
WantedBy=default.target
UNIT

units="mldojo-api.service"
systemctl --user daemon-reload
for u in $units; do systemctl --user enable "$u" >/dev/null 2>&1 || true; systemctl --user restart "$u"; done
for d in ${MLDOJO_PLUGIN_DIRS:-}; do
  d=${d/#\~/$HOME}
  if [ -x "$d/install.sh" ]; then MLDOJO_APP="$APP" "$d/install.sh"; else echo "warning: no executable $d/install.sh (MLDOJO_PLUGIN_DIRS)" >&2; fi
done
if [ "$(loginctl show-user "$(id -un)" -p Linger --value 2>/dev/null)" != yes ]; then
  echo "note: lingering is off; services stop when you log out everywhere (loginctl enable-linger $(id -un))" >&2
fi
for i in $(seq 1 30); do
  if curl -fsS "http://127.0.0.1:$PORT/api/v1/health" >/dev/null 2>&1; then break; fi; sleep 1
done
curl -fsS "http://127.0.0.1:$PORT/api/v1/health"; echo
# Issue the token but do not print it. Every redeploy used to echo a live
# credential, which then lived in whatever captured the install output --
# a terminal scrollback, a CI log, an agent transcript.
"$APP/bin/mldojo-api" token issue >/dev/null
echo "API token: run \`mldojo-api token issue\` to print it"
