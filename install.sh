#!/bin/sh
# Install the `scout` command from a GitHub release.
#   curl -fsSL https://raw.githubusercontent.com/sshuvoo/scout-claw/main/install.sh | sh
# Env: SCOUT_VERSION (default: latest), SCOUT_INSTALL_DIR (default: ~/.local/bin)
set -eu

REPO="sshuvoo/scout-claw"
DIR="${SCOUT_INSTALL_DIR:-$HOME/.local/bin}"

os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$os" in linux|darwin) ;; *) echo "unsupported OS: $os (use 'go install' or build from source)" >&2; exit 1;; esac
case "$(uname -m)" in
  x86_64|amd64) arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) echo "unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac

if [ -n "${SCOUT_VERSION:-}" ]; then
  tag="$SCOUT_VERSION"
else
  tag=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -n1)
fi
[ -n "$tag" ] || { echo "could not determine latest release" >&2; exit 1; }

ver="${tag#v}"
name="scoutclaw_${ver}_${os}_${arch}.tar.gz"
base="https://github.com/$REPO/releases/download/$tag"
tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT

echo "downloading $name ($tag)"
curl -fsSL -o "$tmp/$name" "$base/$name"
curl -fsSL -o "$tmp/checksums.txt" "$base/checksums.txt"
want=$(grep " $name\$" "$tmp/checksums.txt" | cut -d' ' -f1)
if command -v sha256sum >/dev/null 2>&1; then got=$(sha256sum "$tmp/$name" | cut -d' ' -f1); else got=$(shasum -a 256 "$tmp/$name" | cut -d' ' -f1); fi
[ -n "$want" ] && [ "$want" = "$got" ] || { echo "checksum mismatch, aborting" >&2; exit 1; }

tar -xzf "$tmp/$name" -C "$tmp" scout
mkdir -p "$DIR"
install -m 0755 "$tmp/scout" "$DIR/scout"
echo "installed $DIR/scout"
case ":$PATH:" in *":$DIR:"*) ;; *) echo "add to your shell profile:  export PATH=\"$DIR:\$PATH\"";; esac
echo "next: set OPENAI_API_KEY (see .env.example), then run: scout doctor"
