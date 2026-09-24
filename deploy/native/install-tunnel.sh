#!/usr/bin/env bash
# Publish this MLDojo instance through a public box with an SSH reverse tunnel
# (user access only): the API host dials out and binds
# 127.0.0.1:<REMOTE_PORT> on the public box, where nginx terminates TLS for a
# domain and proxies into the tunnel. Agents keep connecting to the API
# directly on the LAN; nothing is exposed on the API host itself.
#
#   REMOTE_HOST=root@1.2.3.4 REMOTE_PORT=18765 deploy/native/install-tunnel.sh
#
# Creates a dedicated key (printed, add it to the public box's authorized_keys)
# and a systemd --user service that keeps the tunnel up.
set -euo pipefail
REMOTE_HOST=${REMOTE_HOST:?set REMOTE_HOST=user@host}
REMOTE_PORT=${REMOTE_PORT:-18765}
LOCAL_PORT=${LOCAL_PORT:-8765}
KEY=${KEY:-$HOME/.ssh/id_ed25519_mldojo_tunnel}
UNIT="$HOME/.config/systemd/user/mldojo-tunnel.service"

if [ ! -f "$KEY" ]; then
  ssh-keygen -t ed25519 -N "" -C "mldojo-tunnel@$(hostname)" -f "$KEY" >/dev/null
  echo "created $KEY"
fi
mkdir -p "$(dirname "$UNIT")"
cat > "$UNIT" <<UNITEOF
[Unit]
Description=MLDojo reverse tunnel to $REMOTE_HOST ($REMOTE_PORT -> local $LOCAL_PORT)
After=network-online.target mldojo-api.service
Wants=network-online.target

[Service]
# IdentityAgent=none: a gpg-agent on SSH_AUTH_SOCK would offer its own keys first.
ExecStart=/usr/bin/ssh -N -T -i $KEY \\
  -o IdentitiesOnly=yes -o IdentityAgent=none -o BatchMode=yes \\
  -o ExitOnForwardFailure=yes -o ServerAliveInterval=30 -o ServerAliveCountMax=3 \\
  -o StrictHostKeyChecking=accept-new \\
  -R 127.0.0.1:$REMOTE_PORT:127.0.0.1:$LOCAL_PORT $REMOTE_HOST
Restart=always
RestartSec=5

[Install]
WantedBy=default.target
UNITEOF
systemctl --user daemon-reload
echo "wrote $UNIT"
echo
echo "Add this line to $REMOTE_HOST's ~/.ssh/authorized_keys (restricted to this one port):"
echo "restrict,port-forwarding,permitlisten=\"127.0.0.1:$REMOTE_PORT\",command=\"/usr/bin/false\" $(cat "$KEY.pub")"
echo
echo "then: systemctl --user enable --now mldojo-tunnel && systemctl --user status mldojo-tunnel"
