#!/usr/bin/env bash
# Restore an MLDojo bundle written by backup.sh: database + data_dir
# (roadmap R3). This DESTROYS the current database and data_dir, so it asks
# before touching anything.
#
#   MLDOJO_RESTORE_YES=1 deploy/native/restore.sh ~/.mldojo/backups/mldojo-backup-20260920T030000Z.tar
#
# Env (all optional):
#   MLDOJO_RESTORE_YES=1  skip the interactive confirmation (for scripts)
#   MLDOJO_DATABASE_URL   overrides ~/.mldojo/postgres.url and config.yaml
#   MLDOJO_DATA_DIR       where to restore the data_dir (default: config.yaml)
#   MLDOJO_PSQL           psql binary, or a wrapper for the compose setup:
#                         "docker compose exec -T postgres psql"
#
# The old data_dir is moved to <data_dir>.pre-restore-<ts> instead of being
# deleted, so a bad restore is still recoverable. mldojo-api is stopped first
# and started again at the end.
set -euo pipefail

BUNDLE=${1:-}
MLDOJO_DIR=${MLDOJO_HOME:-$HOME/.mldojo}
CONF=${MLDOJO_CONFIG:-$MLDOJO_DIR/config.yaml}
BACKUP_DIR=${MLDOJO_BACKUP_DIR:-$MLDOJO_DIR/backups}
UNIT="$HOME/.config/systemd/user/mldojo-api.service"
TS=$(date -u +%Y%m%dT%H%M%SZ)

die() { echo "restore: $*" >&2; exit 1; }
redact() { sed -E 's#(://[^:/@]+):[^@]*@#\1:***@#g'; }
conf() {
  [ -f "$CONF" ] || return 0
  sed -n "s/^[[:space:]]*$1:[[:space:]]*//p" "$CONF" | head -1
}
expand_home() { case "$1" in \~/*) echo "$HOME/${1#\~/}" ;; *) echo "$1" ;; esac; }
if command -v sha256sum >/dev/null 2>&1; then
  sha256check() { sha256sum -c "$@"; }
elif command -v shasum >/dev/null 2>&1; then
  sha256check() { shasum -a 256 -c "$@"; }
else
  die "need sha256sum or shasum"
fi

if [ -z "$BUNDLE" ]; then
  echo "usage: $0 <bundle.tar>" >&2
  echo "available bundles in $BACKUP_DIR:" >&2
  find "$BACKUP_DIR" -maxdepth 1 -type f -name 'mldojo-backup-*.tar' 2>/dev/null | sort -r | head -10 >&2 ||
    echo "  (none)" >&2
  exit 2
fi
[ -f "$BUNDLE" ] || die "no such bundle: $BUNDLE"
BUNDLE=$(cd "$(dirname "$BUNDLE")" && pwd)/$(basename "$BUNDLE")

DB_URL=${MLDOJO_DATABASE_URL:-}
[ -n "$DB_URL" ] || DB_URL=$(cat "$MLDOJO_DIR/postgres.url" 2>/dev/null || true)
[ -n "$DB_URL" ] || DB_URL=$(conf database_url)
[ -n "$DB_URL" ] || die "no database url: set MLDOJO_DATABASE_URL"

DATA_DIR=${MLDOJO_DATA_DIR:-}
[ -n "$DATA_DIR" ] || DATA_DIR=$(conf data_dir)
[ -n "$DATA_DIR" ] || DATA_DIR="$MLDOJO_DIR/data"
DATA_DIR=$(expand_home "$DATA_DIR")

# psql is only needed for pg_dump-format bundles; mldojo-format bundles are read back in by
# mldojo-api itself (the native install's embedded PostgreSQL has neither pg_dump nor psql).
# Step 4 picks the right tool based on the dump_format in MANIFEST.
PSQL=()
if [ -n "${MLDOJO_PSQL:-}" ]; then
  read -r -a PSQL <<<"$MLDOJO_PSQL"
elif command -v psql >/dev/null 2>&1; then
  PSQL=(psql)
else
  for c in "$HOME"/opt/pgsql-*/bin/psql; do
    if [ -x "$c" ]; then PSQL=("$c"); fi
  done
fi

umask 077
STAGE=$(mktemp -d "${TMPDIR:-/tmp}/mldojo-restore-XXXXXX")
trap 'rm -rf "$STAGE"' EXIT

# 1. Verify integrity: check the outer file first, then each member inside the bundle.
if [ -f "$BUNDLE.sha256" ]; then
  (cd "$(dirname "$BUNDLE")" && sha256check --status "$(basename "$BUNDLE").sha256") ||
    die "checksum mismatch: $BUNDLE is corrupt"
  echo "restore: checksum ok"
else
  echo "restore: note: no $BUNDLE.sha256 next to the bundle; skipping the outer checksum" >&2
fi
tar -xf "$BUNDLE" -C "$STAGE" || die "cannot read $BUNDLE (not a backup.sh bundle?)"
for f in MANIFEST SHA256SUMS db.sql.gz data.tar.gz; do
  [ -f "$STAGE/$f" ] || die "bundle is missing $f"
done
(cd "$STAGE" && sha256check --status SHA256SUMS) || die "bundle contents are corrupt (SHA256SUMS mismatch)"
if ! gzip -t "$STAGE/db.sql.gz" || ! gzip -t "$STAGE/data.tar.gz"; then
  die "bundle contains a corrupt archive"
fi
echo "restore: bundle verified"
echo "--- MANIFEST ---"
cat "$STAGE/MANIFEST"
echo "----------------"

# 2. Confirm: after this step the data gets overwritten, so an explicit yes is required.
echo "restore: this will OVERWRITE"
echo "restore:   database $(printf '%s' "$DB_URL" | redact)"
echo "restore:   data_dir $DATA_DIR (moved to $DATA_DIR.pre-restore-$TS)"
if [ ! -f "$UNIT" ]; then
  echo "restore: no $UNIT here; stop the API yourself first (compose: docker compose stop api)" >&2
fi
if [ "${MLDOJO_RESTORE_YES:-}" = 1 ]; then
  echo "restore: MLDOJO_RESTORE_YES=1, proceeding"
elif [ -t 0 ]; then
  printf 'restore: type "yes" to continue: '
  read -r answer
  [ "$answer" = yes ] || die "aborted"
else
  die "refusing to overwrite without confirmation: set MLDOJO_RESTORE_YES=1, or run this on a terminal"
fi

# 3. Stop the API so it isn't writing logs while data_dir gets swapped out from under it.
api_stopped=0
if command -v systemctl >/dev/null 2>&1 && [ -f "$UNIT" ]; then
  echo "restore: stopping mldojo-api"
  systemctl --user stop mldojo-api.service || die "cannot stop mldojo-api.service"
  api_stopped=1
fi

# 4. Database. Format is decided by MANIFEST: a pg_dump dump carries --clean --if-exists and
# drops same-named objects before recreating them; mldojo's logical dump has mldojo-api migrate first, then TRUNCATE+COPY.
DUMP_FORMAT=$(sed -n 's/^dump_format //p' "$STAGE/MANIFEST" | head -1)
[ -n "$DUMP_FORMAT" ] || DUMP_FORMAT=pg_dump # bundle predates this field
echo "restore: loading database (dump format: $DUMP_FORMAT)"
if [ "$DUMP_FORMAT" = pg_dump ]; then
  [ ${#PSQL[@]} -gt 0 ] || die "this bundle needs psql: set MLDOJO_PSQL=/path/to/psql"
  if ! gzip -dc "$STAGE/db.sql.gz" |
    "${PSQL[@]}" -v ON_ERROR_STOP=1 -q -w "$DB_URL" >"$STAGE/psql.log" 2>&1; then
    redact <"$STAGE/psql.log" >&2
    die "psql failed (see above); mldojo-api is still stopped"
  fi
else
  API_BIN=${MLDOJO_API_BIN:-}
  if [ -z "$API_BIN" ]; then
    for c in "$MLDOJO_DIR/app/bin/mldojo-api" "$(dirname "$0")/../../bin/mldojo-api"; do
      if [ -x "$c" ]; then API_BIN=$c; break; fi
    done
  fi
  [ -n "$API_BIN" ] || die "this bundle needs the mldojo-api binary: set MLDOJO_API_BIN"
  if ! gzip -dc "$STAGE/db.sql.gz" |
    MLDOJO_DATABASE_URL="$DB_URL" "$API_BIN" db restore >"$STAGE/psql.log" 2>&1; then
    redact <"$STAGE/psql.log" >&2
    die "mldojo-api db restore failed (see above); mldojo-api is still stopped"
  fi
fi

# 5. data_dir. Move the old one aside instead of deleting it, so a bad restore can still be undone.
echo "restore: unpacking data_dir"
if [ -d "$DATA_DIR" ]; then
  mv "$DATA_DIR" "$DATA_DIR.pre-restore-$TS"
fi
mkdir -p "$DATA_DIR"
tar -xzf "$STAGE/data.tar.gz" -C "$DATA_DIR"
# cache/ and tmp/ aren't in the backup (they're rebuildable); logstore.New also creates them on
# startup -- creating them here just keeps the restored layout consistent with a running instance.
mkdir -p "$DATA_DIR/cache" "$DATA_DIR/tmp"

# 6. Master key: the secrets table is age-encrypted; the wrong key can't decrypt it.
KEYFILE=$(expand_home "${MLDOJO_MASTER_KEY_FILE:-$MLDOJO_DIR/master.key}")
if [ -f "$STAGE/master.key" ]; then
  if [ ! -f "$KEYFILE" ]; then
    cp "$STAGE/master.key" "$KEYFILE"
    chmod 600 "$KEYFILE"
    echo "restore: wrote $KEYFILE from the bundle"
  elif cmp -s "$STAGE/master.key" "$KEYFILE"; then
    echo "restore: master key already matches $KEYFILE"
  else
    cp "$STAGE/master.key" "$KEYFILE.restored"
    chmod 600 "$KEYFILE.restored"
    echo "restore: warning: $KEYFILE differs from the bundle's key and was kept." >&2
    echo "restore: warning: the backed up one is at $KEYFILE.restored; swap it in if secrets fail to decrypt." >&2
  fi
else
  echo "restore: this bundle has no master key (backups exclude it unless MLDOJO_BACKUP_INCLUDE_KEY=1)." >&2
  echo "restore: put $KEYFILE back yourself, or unlock from the OS keychain, or secrets stay unreadable." >&2
  echo "restore: warning: secrets cannot be decrypted unless that keychain entry is also restored." >&2
fi

# 7. Start the API.
if [ "$api_stopped" = 1 ]; then
  echo "restore: starting mldojo-api"
  systemctl --user start mldojo-api.service
  systemctl --user --no-pager status mldojo-api.service | head -5
fi
echo "restore: done; previous data_dir kept at $DATA_DIR.pre-restore-$TS"
