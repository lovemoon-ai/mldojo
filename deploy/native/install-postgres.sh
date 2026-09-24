#!/usr/bin/env bash
# Install PostgreSQL 16 without root or docker and run it as a systemd --user
# service. Binaries come from Maven Central (zonky embedded-postgres builds).
#   PG_VERSION=16.15.0 PGPORT=55432 deploy/native/install-postgres.sh
# Prints the database URL at the end (also saved to ~/.mldojo/postgres.url).
set -euo pipefail
PG_VERSION=${PG_VERSION:-16.15.0}
PREFIX=${PREFIX:-$HOME/opt/pgsql-${PG_VERSION%%.*}}
PGDATA=${PGDATA:-$HOME/.mldojo/pgdata}
PGPORT=${PGPORT:-55432}
MAVEN=${MAVEN:-https://repo1.maven.org/maven2}

case "$(uname -m)" in
  x86_64) artifact=embedded-postgres-binaries-linux-amd64 ;;
  aarch64|arm64) artifact=embedded-postgres-binaries-linux-arm64v8 ;;
  *) echo "unsupported arch $(uname -m)" >&2; exit 1 ;;
esac

if [ ! -x "$PREFIX/bin/postgres" ]; then
  tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
  url="$MAVEN/io/zonky/test/postgres/$artifact/$PG_VERSION/$artifact-$PG_VERSION.jar"
  echo "downloading $url"
  curl -fsSL --retry 3 -o "$tmp/pg.jar" "$url"
  (cd "$tmp" && python3 -c 'import zipfile,sys; zipfile.ZipFile("pg.jar").extractall(".")')
  txz=$(find "$tmp" -name '*.txz' | head -1)
  mkdir -p "$PREFIX"
  tar -xJf "$txz" -C "$PREFIX"
fi
"$PREFIX/bin/postgres" --version

mkdir -p "$(dirname "$PGDATA")"
if [ ! -f "$PGDATA/PG_VERSION" ]; then
  pw=$(head -c 24 /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | head -c 24)
  pwfile=$(mktemp); echo "$pw" > "$pwfile"
  "$PREFIX/bin/initdb" -D "$PGDATA" -U mldojo --pwfile="$pwfile" --auth-local=trust --auth-host=scram-sha-256 -E UTF8 >/dev/null
  rm -f "$pwfile"
  echo "CREATE DATABASE mldojo OWNER mldojo;" | "$PREFIX/bin/postgres" --single -D "$PGDATA" postgres >/dev/null
  umask 077
  echo "postgres://mldojo:$pw@127.0.0.1:$PGPORT/mldojo?sslmode=disable" > "$HOME/.mldojo/postgres.url"
fi

mkdir -p "$HOME/.config/systemd/user"
cat > "$HOME/.config/systemd/user/mldojo-postgres.service" <<UNIT
[Unit]
Description=MLDojo PostgreSQL ($PGPORT)

[Service]
ExecStart=$PREFIX/bin/postgres -D $PGDATA -p $PGPORT -c listen_addresses=127.0.0.1 -c unix_socket_directories=/tmp
Restart=always
RestartSec=3

[Install]
WantedBy=default.target
UNIT
systemctl --user daemon-reload
systemctl --user enable --now mldojo-postgres.service
sleep 2
systemctl --user --no-pager status mldojo-postgres.service | head -5
echo "database url: $(cat "$HOME/.mldojo/postgres.url" | sed 's#://mldojo:[^@]*@#://mldojo:***@#')"
