#!/usr/bin/env bash
# Back up MLDojo into one timestamped bundle (roadmap R3): the PostgreSQL
# database plus the data_dir (run logs, code blobs) that the API writes.
#
#   deploy/native/backup.sh                         # native install, defaults
#   MLDOJO_BACKUP_KEEP=30 deploy/native/backup.sh   # keep a month of bundles
#
# Env (all optional):
#   MLDOJO_BACKUP_DIR    where bundles land (default ~/.mldojo/backups)
#   MLDOJO_BACKUP_KEEP   bundles to keep, 0 = keep everything (default 7)
#   MLDOJO_DATABASE_URL  overrides ~/.mldojo/postgres.url and config.yaml
#   MLDOJO_DATA_DIR      overrides config.yaml (default ~/.mldojo/data)
#   MLDOJO_PGDUMP        pg_dump binary, or a wrapper for the compose setup:
#                        "docker compose exec -T postgres pg_dump"
#
# The bundle is a plain tar of: MANIFEST, db.sql.gz, data.tar.gz
# (and master.key only when MLDOJO_BACKUP_INCLUDE_KEY=1).
# (when it lives in a file) and SHA256SUMS. Restore it with restore.sh.
# The database URL is only ever printed with its password redacted. Safe to
# run from a timer: a lock stops overlapping runs from piling up.
set -euo pipefail

MLDOJO_DIR=${MLDOJO_HOME:-$HOME/.mldojo}
CONF=${MLDOJO_CONFIG:-$MLDOJO_DIR/config.yaml}
BACKUP_DIR=${MLDOJO_BACKUP_DIR:-$MLDOJO_DIR/backups}
KEEP=${MLDOJO_BACKUP_KEEP:-7}
TS=$(date -u +%Y%m%dT%H%M%SZ)

die() { echo "backup: $*" >&2; exit 1; }
# redact turns the password in a postgres:// URL into ***; anywhere a URL is printed must pipe through it.
redact() { sed -E 's#(://[^:/@]+):[^@]*@#\1:***@#g'; }
# conf reads a single scalar key (database_url / data_dir) from config.yaml.
conf() {
  [ -f "$CONF" ] || return 0
  sed -n "s/^[[:space:]]*$1:[[:space:]]*//p" "$CONF" | head -1
}
expand_home() { case "$1" in \~/*) echo "$HOME/${1#\~/}" ;; *) echo "$1" ;; esac; }
# macOS uses shasum, Linux uses sha256sum.
if command -v sha256sum >/dev/null 2>&1; then
  sha256() { sha256sum "$@"; }
elif command -v shasum >/dev/null 2>&1; then
  sha256() { shasum -a 256 "$@"; }
else
  die "need sha256sum or shasum"
fi

case "$KEEP" in '' | *[!0-9]*) die "MLDOJO_BACKUP_KEEP must be a number (got '$KEEP')" ;; esac

DB_URL=${MLDOJO_DATABASE_URL:-}
[ -n "$DB_URL" ] || DB_URL=$(cat "$MLDOJO_DIR/postgres.url" 2>/dev/null || true)
[ -n "$DB_URL" ] || DB_URL=$(conf database_url)
[ -n "$DB_URL" ] || die "no database url: set MLDOJO_DATABASE_URL, or run deploy/native/install-postgres.sh first"

DATA_DIR=${MLDOJO_DATA_DIR:-}
[ -n "$DATA_DIR" ] || DATA_DIR=$(conf data_dir)
[ -n "$DATA_DIR" ] || DATA_DIR="$MLDOJO_DIR/data"
DATA_DIR=$(expand_home "$DATA_DIR")
[ -d "$DATA_DIR" ] || die "data_dir not found: $DATA_DIR (set MLDOJO_DATA_DIR)"

# Backup tool: pg_dump first. But the zonky embedded PostgreSQL used by the
# native install only bundles three binaries -- initdb / pg_ctl / postgres --
# and has **no pg_dump**, so it must fall back to mldojo-api's own logical backup
# (`mldojo-api db dump`, see api/internal/models/dump.go).
DUMP_FORMAT=pg_dump
PGDUMP=()
if [ -n "${MLDOJO_PGDUMP:-}" ]; then
  read -r -a PGDUMP <<<"$MLDOJO_PGDUMP"
elif command -v pg_dump >/dev/null 2>&1; then
  PGDUMP=(pg_dump)
else
  for c in "$HOME"/opt/pgsql-*/bin/pg_dump; do
    if [ -x "$c" ]; then PGDUMP=("$c"); fi
  done
fi
if [ ${#PGDUMP[@]} -eq 0 ]; then
  DUMP_FORMAT=mldojo
  API_BIN=${MLDOJO_API_BIN:-}
  if [ -z "$API_BIN" ]; then
    for c in "$MLDOJO_DIR/app/bin/mldojo-api" "$(dirname "$0")/../../bin/mldojo-api"; do
      if [ -x "$c" ]; then API_BIN=$c; break; fi
    done
  fi
  [ -n "$API_BIN" ] || die "no pg_dump and no mldojo-api binary: set MLDOJO_PGDUMP or MLDOJO_API_BIN"
fi

umask 077
mkdir -p "$BACKUP_DIR"
chmod 700 "$BACKUP_DIR" 2>/dev/null || true

# Concurrency guard: skip outright if a scheduled run overlaps with another, rather than letting two dumps clobber each other.
LOCK="$BACKUP_DIR/.backup.lock"
LOCK_DIR="$BACKUP_DIR/.backup.lock.d"
mkdir_lock=0
if command -v flock >/dev/null 2>&1; then
  exec 9>"$LOCK"
  flock -n 9 || { echo "backup: another run holds $LOCK; skipping" >&2; exit 0; }
else
  # macOS has no flock(1); mkdir is atomic on every platform.
  mkdir "$LOCK_DIR" 2>/dev/null || { echo "backup: another run holds $LOCK_DIR; skipping" >&2; exit 0; }
  mkdir_lock=1
fi

STAGE=$(mktemp -d "${TMPDIR:-/tmp}/mldojo-backup-XXXXXX")
BUNDLE="$BACKUP_DIR/mldojo-backup-$TS.tar"
cleanup() {
  rm -rf "$STAGE" "$BUNDLE.partial"
  if [ "$mkdir_lock" = 1 ]; then rmdir "$LOCK_DIR" 2>/dev/null || true; fi
}
trap cleanup EXIT

echo "backup: database $(printf '%s' "$DB_URL" | redact) (dump format: $DUMP_FORMAT)"
if [ "$DUMP_FORMAT" = pg_dump ]; then
  if ! "${PGDUMP[@]}" --no-owner --no-privileges --clean --if-exists -w "$DB_URL" \
    2>"$STAGE/pg_dump.err" | gzip -9 >"$STAGE/db.sql.gz"; then
    redact <"$STAGE/pg_dump.err" >&2
    die "pg_dump failed (see above)"
  fi
  # If pg_dump exits successfully but the output was truncated (disk full), this closing line won't be there.
  if ! gzip -dc "$STAGE/db.sql.gz" | tail -5 | grep -q 'PostgreSQL database dump complete'; then
    die "database dump looks truncated; refusing to keep it"
  fi
else
  if ! MLDOJO_DATABASE_URL="$DB_URL" "$API_BIN" db dump \
    2>"$STAGE/pg_dump.err" | gzip -9 >"$STAGE/db.sql.gz"; then
    redact <"$STAGE/pg_dump.err" >&2
    die "mldojo-api db dump failed (see above)"
  fi
  # The logical backup ends with the COPY terminator of the last table; a truncated dump won't have it.
  # head closes the pipe early, so gzip gets a SIGPIPE -- keep these two lines out of pipefail's reach.
  set +o pipefail
  first_line=$(gzip -dc "$STAGE/db.sql.gz" | head -1)
  last_line=$(gzip -dc "$STAGE/db.sql.gz" | tail -1)
  set -o pipefail
  case "$first_line" in
    '-- mldojo-dump'*) ;;
    *) die "database dump is missing its header; refusing to keep it" ;;
  esac
  [ "$last_line" = '\.' ] || die "database dump looks truncated; refusing to keep it"
fi

echo "backup: data_dir $DATA_DIR"
# cache/ is the content-addressed artifact cache and tmp/ holds unfinished uploads; both are
# rebuilt on demand by api/internal/logstore (logstore.New creates the directories on every
# startup), so backing them up only makes the archive bigger and slower with no upside after restore.
set +e
tar -czf "$STAGE/data.tar.gz" -C "$DATA_DIR" --exclude=./cache --exclude=./tmp .
rc=$?
set -e
[ "$rc" -le 1 ] || die "tar failed (exit $rc) while archiving $DATA_DIR"
[ "$rc" -eq 0 ] || echo "backup: note: files changed while archiving (active runs); the archive is still readable" >&2

# Master key: the secrets table's contents are age-encrypted, and without this key they still
# can't be decrypted after a restore. But the whole point of a backup is to be copied elsewhere,
# and bundling the master key in would mean "having the backup = having every secret (SSH
# private keys, bucket credentials, ...)". So it's excluded by default; turn on
# MLDOJO_BACKUP_INCLUDE_KEY=1 explicitly when you need it, and make sure wherever the backup is
# stored deserves that level of trust.
KEYFILE=$(expand_home "${MLDOJO_MASTER_KEY_FILE:-$MLDOJO_DIR/master.key}")
members=(MANIFEST db.sql.gz data.tar.gz)
key_state=excluded
if [ "${MLDOJO_BACKUP_INCLUDE_KEY:-0}" = "1" ]; then
  if [ -f "$KEYFILE" ]; then
    cp "$KEYFILE" "$STAGE/master.key"
    chmod 600 "$STAGE/master.key"
    members+=(master.key)
    key_state=included
  else
    key_state=absent
  fi
fi

db_name=${DB_URL##*/}
db_name=${db_name%%\?*}
cat >"$STAGE/MANIFEST" <<MANIFEST
mldojo-backup 1
created_at $TS
host $(hostname)
database $db_name
data_dir $DATA_DIR
master_key $key_state
dump_format $DUMP_FORMAT
dump_tool $(if [ "$DUMP_FORMAT" = pg_dump ]; then "${PGDUMP[@]}" --version 2>/dev/null | head -1; else "$API_BIN" version 2>/dev/null | head -1; fi)
MANIFEST

(cd "$STAGE" && sha256 "${members[@]}" >SHA256SUMS)
tar -cf "$BUNDLE.partial" -C "$STAGE" SHA256SUMS "${members[@]}"
mv "$BUNDLE.partial" "$BUNDLE"
chmod 600 "$BUNDLE"
(cd "$BACKUP_DIR" && sha256 "mldojo-backup-$TS.tar" >"mldojo-backup-$TS.tar.sha256")

if [ "$KEEP" -gt 0 ]; then
  find "$BACKUP_DIR" -maxdepth 1 -type f -name 'mldojo-backup-*.tar' | sort -r | tail -n +"$((KEEP + 1))" |
    while IFS= read -r old; do
      rm -f "$old" "$old.sha256"
      echo "backup: pruned $(basename "$old")"
    done
fi

echo "backup: db $(du -h "$STAGE/db.sql.gz" | cut -f1), data $(du -h "$STAGE/data.tar.gz" | cut -f1)"
echo "backup: wrote $BUNDLE ($(du -h "$BUNDLE" | cut -f1))"
case "$key_state" in
  excluded)
    echo "backup: the master key is NOT in this bundle (set MLDOJO_BACKUP_INCLUDE_KEY=1 to add it)." >&2
    echo "backup: keep $KEYFILE, or the OS keychain, safe separately -- restored secrets need it." >&2
    ;;
  absent)
    echo "backup: warning: MLDOJO_BACKUP_INCLUDE_KEY=1 but there is no $KEYFILE." >&2
    echo "backup: warning: it is in the OS keychain; without it restored secrets cannot be decrypted." >&2
    ;;
  included)
    echo "backup: this bundle CONTAINS the master key -- treat it as a secret." >&2
    ;;
esac
