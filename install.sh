#!/usr/bin/env bash
#
# OpenBox shift-left installer — the `curl … | bash` front door.
#
#   curl -fsSL https://raw.githubusercontent.com/OpenBox-AI/openbox-shift-left/main/install.sh | bash
#
# What it does (and ONLY this — it never touches your OpenBox account):
#   1. Checks for a Go 1.23+ toolchain and git.
#   2. Clones (or reuses) the openbox-shift-left source.
#   3. Builds the ONE unified static engine — `openbox` — from cli/cmd/openbox
#      (CGO_ENABLED=0; OD17: single no-cgo binary that is CLI + hook + sidecar +
#      git-hook all in one).
#   4. Installs it to a bin dir on your PATH (default: ~/.local/bin).
#
# It deliberately does NOT register you with OpenBox or wire Claude Code. That is
# the second, interactive step you run yourself once the binary is on PATH:
#
#   openbox dev init --provider claude-code
#
# which materializes the Claude Code plugin bundle into
# ~/.claude/plugins/openbox-observe (copying this same engine into its bin/) and
# mints + stores your agent credentials. Governance is ambient thereafter.
#
# Why build-from-source and not `go install …@latest`: the cli module uses
# relative `replace` directives to its sibling modules in this repo, which the
# module proxy path cannot resolve — so a checkout + local build is required.
#
# Tunables (all optional env vars):
#   OPENBOX_INSTALL_DIR   where to place the binary        (default: ~/.local/bin)
#   OPENBOX_REF           git branch / tag / sha to build  (default: main)
#   OPENBOX_REPO_URL      clone URL                        (default: https GitHub)
#   OPENBOX_SRC           build from THIS existing checkout instead of cloning
#   OPENBOX_SKIP_TESTS    set to 1 to skip the smoke build-verify (faster)
#
set -euo pipefail

# ----------------------------------------------------------------------------- #
# Config
# ----------------------------------------------------------------------------- #
REPO_URL="${OPENBOX_REPO_URL:-https://github.com/OpenBox-AI/openbox-shift-left.git}"
REF="${OPENBOX_REF:-main}"
INSTALL_DIR="${OPENBOX_INSTALL_DIR:-$HOME/.local/bin}"
BIN_NAME="openbox"
MIN_GO_MINOR=23   # require go 1.23+

# ----------------------------------------------------------------------------- #
# Pretty output (no color when not a tty)
# ----------------------------------------------------------------------------- #
if [ -t 1 ]; then
  BOLD=$'\033[1m'; DIM=$'\033[2m'; RED=$'\033[31m'; GRN=$'\033[32m'; YEL=$'\033[33m'; RST=$'\033[0m'
else
  BOLD=""; DIM=""; RED=""; GRN=""; YEL=""; RST=""
fi
info()  { printf '%s==>%s %s\n' "$BOLD" "$RST" "$*"; }
ok()    { printf '%s ✓%s %s\n' "$GRN" "$RST" "$*"; }
warn()  { printf '%s ⚠%s %s\n' "$YEL" "$RST" "$*" >&2; }
die()   { printf '%s ✗ %s%s\n' "$RED" "$*" "$RST" >&2; exit 1; }

# ----------------------------------------------------------------------------- #
# Preflight: toolchain
# ----------------------------------------------------------------------------- #
command -v git >/dev/null 2>&1 || die "git is required but not found on PATH."

command -v go >/dev/null 2>&1 || die \
"Go 1.${MIN_GO_MINOR}+ is required but 'go' was not found on PATH.
   Install it from https://go.dev/dl/ (or your package manager), then re-run."

# Parse `go version` → major.minor and enforce >= 1.MIN_GO_MINOR.
GO_VER="$(go env GOVERSION 2>/dev/null || go version | awk '{print $3}')"
GO_VER="${GO_VER#go}"
GO_MAJOR="${GO_VER%%.*}"
GO_REST="${GO_VER#*.}"; GO_MINOR="${GO_REST%%.*}"
if [ "${GO_MAJOR:-0}" -lt 1 ] || { [ "${GO_MAJOR:-0}" -eq 1 ] && [ "${GO_MINOR:-0}" -lt "$MIN_GO_MINOR" ]; }; then
  die "Go 1.${MIN_GO_MINOR}+ required; found ${GO_VER}. Upgrade from https://go.dev/dl/."
fi
ok "Go toolchain: ${GO_VER}"

# ----------------------------------------------------------------------------- #
# Source: reuse a provided checkout, or clone a shallow copy to a temp dir
# ----------------------------------------------------------------------------- #
CLEANUP_SRC=""
cleanup() { [ -n "$CLEANUP_SRC" ] && rm -rf "$CLEANUP_SRC" 2>/dev/null || true; }
trap cleanup EXIT

if [ -n "${OPENBOX_SRC:-}" ]; then
  SRC="$OPENBOX_SRC"
  [ -f "$SRC/cli/go.mod" ] || die "OPENBOX_SRC=$SRC does not look like an openbox-shift-left checkout (no cli/go.mod)."
  info "Building from existing checkout: $SRC"
else
  SRC="$(mktemp -d "${TMPDIR:-/tmp}/openbox-shift-left.XXXXXX")"
  CLEANUP_SRC="$SRC"
  info "Cloning ${REPO_URL} @ ${REF} …"
  if ! git clone --quiet --depth 1 --branch "$REF" "$REPO_URL" "$SRC" 2>/dev/null; then
    # --branch fails on a bare commit sha; fall back to full clone + checkout.
    git clone --quiet "$REPO_URL" "$SRC" || die \
"clone failed. If this is a private repo, either grant this machine access and
   retry, or clone it yourself and re-run with:
     OPENBOX_SRC=/path/to/openbox-shift-left bash install.sh"
    git -C "$SRC" checkout --quiet "$REF" || die "ref '$REF' not found in $REPO_URL"
  fi
  ok "Source ready"
fi

# ----------------------------------------------------------------------------- #
# Build the unified engine
# ----------------------------------------------------------------------------- #
VERSION="$(git -C "$SRC" describe --tags --always --dirty 2>/dev/null || echo "0.1.0-dev")"
OUT="$SRC/${BIN_NAME}"

info "Building ${BIN_NAME} ${VERSION} (static, no-cgo) …"
(
  cd "$SRC/cli"
  CGO_ENABLED=0 go build \
    -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o "$OUT" \
    ./cmd/openbox
)
[ -x "$OUT" ] || die "build produced no binary at $OUT"

if [ "${OPENBOX_SKIP_TESTS:-0}" != "1" ]; then
  "$OUT" version >/dev/null 2>&1 || die "built binary failed to run ('$OUT version')."
fi
ok "Built $("$OUT" version 2>/dev/null || echo "$BIN_NAME $VERSION")"

# ----------------------------------------------------------------------------- #
# Install onto PATH
# ----------------------------------------------------------------------------- #
mkdir -p "$INSTALL_DIR" || die "cannot create install dir $INSTALL_DIR"
DEST="$INSTALL_DIR/$BIN_NAME"

# Atomic-ish install: copy to a temp name in the same dir, then rename over.
TMP_DEST="$INSTALL_DIR/.${BIN_NAME}.new.$$"
if cp "$OUT" "$TMP_DEST" 2>/dev/null && chmod 0755 "$TMP_DEST" 2>/dev/null && mv -f "$TMP_DEST" "$DEST" 2>/dev/null; then
  :
else
  rm -f "$TMP_DEST" 2>/dev/null || true
  warn "No write permission to $INSTALL_DIR — retrying with sudo."
  sudo install -m 0755 "$OUT" "$DEST" || die "install to $DEST failed."
fi
ok "Installed → $DEST"

# ----------------------------------------------------------------------------- #
# PATH check + next steps
# ----------------------------------------------------------------------------- #
case ":${PATH}:" in
  *":${INSTALL_DIR}:"*) ON_PATH=1 ;;
  *)                    ON_PATH=0 ;;
esac

echo
if [ "$ON_PATH" -ne 1 ]; then
  warn "${INSTALL_DIR} is not on your PATH. Add it, e.g.:"
  printf '     %sexport PATH="%s:$PATH"%s   # add to ~/.bashrc or ~/.zshrc\n' "$DIM" "$INSTALL_DIR" "$RST"
  echo
fi

info "${BOLD}Next step — wire OpenBox into Claude Code:${RST}"
echo
printf '     export OPENBOX_BACKEND_URL=https://<your-openbox-backend>\n'
printf '     export OPENBOX_CONTROL_TOKEN=<keycloak-jwt-or-obx_key_…>   # never a flag (INV-1)\n'
printf '     %s%s dev init --provider claude-code%s\n' "$BOLD" "$([ "$ON_PATH" -eq 1 ] && echo "$BIN_NAME" || echo "$DEST")" "$RST"
echo
printf '   That materializes the Claude Code plugin into ~/.claude/plugins/openbox-observe,\n'
printf '   copies this engine into its bin/, and stores your agent credentials.\n'
printf '   Verify anytime with:  %s dev verify\n' "$([ "$ON_PATH" -eq 1 ] && echo "$BIN_NAME" || echo "$DEST")"
echo
ok "Done."
