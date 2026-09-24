#!/usr/bin/env bash
# Install the daily backup as systemd --user units (roadmap R3; no root).
# Run from a checkout, after install-postgres.sh and install.sh:
#   deploy/native/install-backup.sh
#   MLDOJO_BACKUP_KEEP=30 MLDOJO_BACKUP_ONCALENDAR=weekly deploy/native/install-backup.sh
#
# Creates mldojo-backup.service (oneshot, runs deploy/native/backup.sh from
# this checkout) + mldojo-backup.timer, enables the timer, and runs one
# backup right away so a broken setup fails here and not at 03:00.
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
MLDOJO_DIR=${MLDOJO_HOME:-$HOME/.mldojo}
BACKUP_DIR=${MLDOJO_BACKUP_DIR:-$MLDOJO_DIR/backups}
KEEP=${MLDOJO_BACKUP_KEEP:-7}
ONCALENDAR=${MLDOJO_BACKUP_ONCALENDAR:-daily}
DELAY=${MLDOJO_BACKUP_DELAY:-1h}
UNITS="$HOME/.config/systemd/user"

[ -x "$ROOT/deploy/native/backup.sh" ] || { echo "missing $ROOT/deploy/native/backup.sh" >&2; exit 1; }
command -v systemctl >/dev/null 2>&1 || { echo "no systemctl: run deploy/native/backup.sh from cron instead" >&2; exit 1; }
mkdir -p "$UNITS"

# ExecStart points at the checkout rather than a copy: a git pull picks up fixes automatically.
# Re-run this script if the checkout path ever moves.
cat > "$UNITS/mldojo-backup.service" <<UNIT
[Unit]
Description=MLDojo backup (PostgreSQL + data_dir)
After=mldojo-postgres.service
Wants=mldojo-postgres.service

[Service]
Type=oneshot
ExecStart=$ROOT/deploy/native/backup.sh
Environment=MLDOJO_BACKUP_DIR=$BACKUP_DIR
Environment=MLDOJO_BACKUP_KEEP=$KEEP${MLDOJO_HOME:+
Environment=MLDOJO_HOME=$MLDOJO_HOME}
UNIT

cat > "$UNITS/mldojo-backup.timer" <<UNIT
[Unit]
Description=MLDojo backup timer ($ONCALENDAR)

[Timer]
OnCalendar=$ONCALENDAR
RandomizedDelaySec=$DELAY
Persistent=true

[Install]
WantedBy=timers.target
UNIT

systemctl --user daemon-reload
systemctl --user enable --now mldojo-backup.timer
echo "wrote $UNITS/mldojo-backup.service and .timer"
systemctl --user list-timers --no-pager mldojo-backup.timer | head -3

if [ "$(loginctl show-user "$(id -un)" -p Linger --value 2>/dev/null)" != yes ]; then
  echo "note: lingering is off; the timer stops when you log out everywhere (loginctl enable-linger $(id -un))" >&2
fi

echo "running one backup now to verify the setup..."
if systemctl --user start mldojo-backup.service; then
  journalctl --user -u mldojo-backup.service -n 10 --no-pager 2>/dev/null || true
  echo "ok: backups land in $BACKUP_DIR (keeping $KEEP)"
else
  journalctl --user -u mldojo-backup.service -n 30 --no-pager 2>/dev/null || true
  echo "the first backup failed; the timer is installed but will keep failing until this is fixed" >&2
  exit 1
fi
