#!/bin/sh
# oxyp installer: one paste, zero thinking.
#   curl -fsSL https://raw.githubusercontent.com/lirrensi/oxyp/main/install.sh | sh
# Piped flags: curl ... | sh -s -- --go --version v0.3.0
set -eu

REPO="lirrensi/oxyp"
PKG="github.com/lirrensi/oxyp/cmd/oxyp"
VERSION="${OXYP_VERSION:-latest}"
USE_GO="${OXYP_USE_GO:-0}"
BINDIR="${OXYP_BINDIR:-}"

log() { printf '%s\n' "$*"; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

while [ $# -gt 0 ]; do
  case "$1" in
    --go) USE_GO=1; shift ;;
    --version) VERSION="${2:?--version needs a tag}"; shift 2 ;;
    --version=*) VERSION="${1#--version=}"; shift ;;
    --dir) BINDIR="${2:?--dir needs a path}"; shift 2 ;;
    --dir=*) BINDIR="${1#--dir=}"; shift ;;
    -h|--help)
      echo "usage: install.sh [--go] [--version TAG] [--dir DIR]"
      echo "  --go           build from source via go install (needs Go 1.21+)"
      echo "  --version TAG  pin release tag (default: latest)"
      echo "  --dir DIR      install dir (default: pick an on-PATH dir)"
      exit 0 ;;
    *) die "unknown flag: $1 (see --help)" ;;
  esac
done

on_path() { case ":$PATH:" in *":$1:"*) return 0;; *) return 1;; esac; }

# 1. install dir: prefer ~/.local/bin when the shell already sees it,
#    else first writable on-PATH dir, else ~/.local/bin + rc fix below.
if [ -z "$BINDIR" ]; then
  if on_path "$HOME/.local/bin"; then
    BINDIR="$HOME/.local/bin"
  elif on_path "$HOME/go/bin"; then
    BINDIR="$HOME/go/bin"
  elif [ -w /usr/local/bin ] && on_path /usr/local/bin; then
    BINDIR=/usr/local/bin
  else
    BINDIR="$HOME/.local/bin"
  fi
fi
mkdir -p "$BINDIR" || die "cannot create $BINDIR"

# a stale symlink (old install advice) must not swallow the new binary
if [ -L "$BINDIR/oxyp" ]; then rm -f "$BINDIR/oxyp"; fi

# 2. fetch or build
if [ "$USE_GO" = "1" ]; then
  command -v go >/dev/null 2>&1 || die "go not found — drop --go or install Go 1.21+"
  log "building oxyp@$VERSION via go install -> $BINDIR ..."
  GOBIN="$BINDIR" go install "$PKG@$VERSION" || die "go install failed"
else
  os="$(uname -s | tr '[:upper:]' '[:lower:]')"
  arch="$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')"
  case "$os/$arch" in
    linux/amd64|linux/arm64|darwin/amd64|darwin/arm64) ;;
    *) die "no prebuilt binary for $os/$arch — retry with --go (needs Go 1.21+)" ;;
  esac
  if [ "$VERSION" = "latest" ]; then
    url="https://github.com/$REPO/releases/latest/download/oxyp-$os-$arch"
  else
    url="https://github.com/$REPO/releases/download/$VERSION/oxyp-$os-$arch"
  fi
  command -v curl >/dev/null 2>&1 || die "curl not found — install curl or use --go"
  tmp="$(mktemp /tmp/oxyp.XXXXXX)" || die "mktemp failed"
  trap 'rm -f "$tmp"' EXIT
  log "downloading oxyp $VERSION ($os/$arch) -> $BINDIR ..."
  curl -fsSL -o "$tmp" "$url" || die "download failed: $url"
  chmod +x "$tmp"
  mv -f "$tmp" "$BINDIR/oxyp" || die "cannot write to $BINDIR"
  trap - EXIT
fi

# 3. PATH: only touched when the shell can't already see the install dir
if ! on_path "$BINDIR"; then
  line="export PATH=\"$BINDIR:\$PATH\""
  added=""
  for rc in "$HOME/.bashrc" "$HOME/.zshrc"; do
    [ -f "$rc" ] || continue
    grep -qxF "$line" "$rc" 2>/dev/null && continue
    printf '\n# oxyp installer: make oxyp discoverable\n%s\n' "$line" >> "$rc" && added="$added $rc"
  done
  if [ -z "$added" ]; then
    # no bash/zsh rc — fall back to ~/.profile (created if missing)
    grep -qxF "$line" "$HOME/.profile" 2>/dev/null || \
      printf '\n# oxyp installer: make oxyp discoverable\n%s\n' "$line" >> "$HOME/.profile"
    added=" $HOME/.profile"
  fi
  log "added $BINDIR to PATH in:$added"
  log "run this once in your current shell (new shells are already fixed):"
  log "  $line"
fi

# 4. verify: absolute path always works; bare name needs PATH
"$BINDIR/oxyp" --help >/dev/null 2>&1 || die "$BINDIR/oxyp failed to run"
if command -v oxyp >/dev/null 2>&1; then
  log "done: $(command -v oxyp) is ready — try: oxyp servers"
else
  log "done: $BINDIR/oxyp installed and runs — restart your shell (or run the export above), then: oxyp servers"
fi
