#!/bin/sh
# MLDojo CLI installer:  curl -fsSL {{.Base}}/install.sh | sh
# Installs to $MLDOJO_BIN_DIR (default ~/.local/bin). No credentials needed.
set -eu
BASE="{{.Base}}"
DIR="${MLDOJO_BIN_DIR:-$HOME/.local/bin}"
os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$(uname -m)" in
  x86_64 | amd64) arch=amd64 ;;
  aarch64 | arm64) arch=arm64 ;;
  *) echo "unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac
case "$os" in linux | darwin) ;; *) echo "unsupported OS: $os" >&2; exit 1 ;; esac
mkdir -p "$DIR"
echo "downloading mldojo ($os/$arch) from $BASE ..."
curl -fL --progress-bar "$BASE/dl/mldojo-$os-$arch" -o "$DIR/mldojo.tmp"
chmod +x "$DIR/mldojo.tmp" && mv "$DIR/mldojo.tmp" "$DIR/mldojo"
echo "installed $DIR/mldojo ($("$DIR/mldojo" --version))"
case ":$PATH:" in *":$DIR:"*) ;; *) echo "note: $DIR is not on PATH; add: export PATH=\"$DIR:\$PATH\"" ;; esac
echo
echo "next: {{.Login}}   # approve the code in a browser (Conductor login)"
echo "docs: $BASE/SKILL.md (open in a browser, or see mldojo --help)"
