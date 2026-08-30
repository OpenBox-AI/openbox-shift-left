#!/usr/bin/env bash
#
# OpenBox shift-left installer — the `curl … | bash` front door.
#
#   curl -fsSL https://raw.githubusercontent.com/OpenBox-AI/openbox-shift-left/main/install.sh | bash
#
# What it does (and ONLY this — it never touches your OpenBox account):
#   1. Detects your OS + CPU (linux/darwin, amd64/arm64).
#   2. Downloads the matching PREBUILT `openbox` engine from GitHub Releases and
#      verifies its sha256 against the release checksums.txt. No Go toolchain, no
#      compile — this is the fast path and needs nothing but curl + tar.
#   3. Installs it to a bin dir on your PATH (default: ~/.local/bin).
#
# If no prebuilt asset matches your platform (or you set OPENBOX_FROM_SOURCE=1), it
# FALLS BACK to building the one unified static engine from source — which then
# requires a Go 1.27+ toolchain + git. Same OD17 binary either way: one no-cgo
# `openbox` that is CLI + hook + sidecar + git-hook.
#
# It deliberately does NOT register you with OpenBox or wire Claude Code. That is
# the second step you run yourself once the binary is on PATH:
#
#   export OPENBOX_CONTROL_TOKEN=<keycloak-jwt-or-obx_key_…>   # never a flag (INV-1)
#   openbox init --provider claude-code --backend-url https://<your-openbox-backend> [--base-url https://<your-openbox-core>] [--enforce]
#
# which registers your agent, materializes the Claude Code plugin into
# ~/.claude/plugins/openbox-observe (copying this same engine into its bin/), stores
# your credentials, and pulls your org policy. Governance is AMBIENT thereafter —
# no daemon to run and no runtime env to set (enforcement evaluates in-process).
#
# Tunables (all optional env vars):
#   OPENBOX_INSTALL_DIR    where to place the binary        (default: ~/.local/bin)
#   OPENBOX_VERSION        release tag to install           (default: latest)
#   OPENBOX_FROM_SOURCE=1  skip the prebuilt download; build from source
#   OPENBOX_REF            git branch/tag/sha to build      (source fallback; default: main)
#   OPENBOX_REPO_URL       clone URL                        (source fallback)
#   OPENBOX_SRC            build from THIS existing checkout instead of downloading
#   OPENBOX_SKIP_TESTS=1   skip the post-install smoke check
#
set -euo pipefail

# ----------------------------------------------------------------------------- #
# Config
# ----------------------------------------------------------------------------- #
GH_OWNER="OpenBox-AI"
GH_REPO="openbox-shift-left"
REPO_URL="${OPENBOX_REPO_URL:-https://github.com/${GH_OWNER}/${GH_REPO}.git}"
REF="${OPENBOX_REF:-main}"
INSTALL_DIR="${OPENBOX_INSTALL_DIR:-$HOME/.local/bin}"
BIN_NAME="openbox"
MIN_GO_MINOR=27   # require go 1.27+ for the SOURCE fallback only

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

# A scratch dir cleaned up on exit, shared by both install paths.
WORK="$(mktemp -d "${TMPDIR:-/tmp}/openbox-install.XXXXXX")"
CLEANUP_SRC=""
cleanup() {
  rm -rf "$WORK" 2>/dev/null || true
  [ -n "$CLEANUP_SRC" ] && rm -rf "$CLEANUP_SRC" 2>/dev/null || true
}
trap cleanup EXIT

# ----------------------------------------------------------------------------- #
# Platform detection → GoReleaser's {{.Os}}_{{.Arch}} tokens
# ----------------------------------------------------------------------------- #
detect_platform() {
  local os arch
  os="$(uname -s | tr '[:upper:]' '[:lower:]')"
  case "$os" in
    linux)  OS=linux ;;
    darwin) OS=darwin ;;
    *)      OS="" ;;   # unsupported → forces the source fallback
  esac
  arch="$(uname -m)"
  case "$arch" in
    x86_64|amd64)  ARCH=amd64 ;;
    aarch64|arm64) ARCH=arm64 ;;
    *)             ARCH="" ;;  # unsupported → forces the source fallback
  esac
}

sha256_of() { # $1=file → prints hex digest
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | awk '{print $1}'
  else
    return 1
  fi
}

# resolve_tag: echo the release tag to install. OPENBOX_VERSION wins; otherwise
# follow the /releases/latest redirect and read the tag off the final URL (no jq,
# no API token needed).
resolve_tag() {
  if [ -n "${OPENBOX_VERSION:-}" ]; then
    printf '%s' "$OPENBOX_VERSION"
    return 0
  fi
  local eff
  eff="$(curl -fsSLI -o /dev/null -w '%{url_effective}' \
    "https://github.com/${GH_OWNER}/${GH_REPO}/releases/latest" 2>/dev/null)" || return 1
  case "$eff" in
    */releases/tag/*) printf '%s' "${eff##*/releases/tag/}" ;;
    *)                return 1 ;;   # no published release yet
  esac
}

# ----------------------------------------------------------------------------- #
# Fast path: download a prebuilt release asset + verify checksum
# ----------------------------------------------------------------------------- #
install_prebuilt() {
  command -v curl >/dev/null 2>&1 || { warn "curl not found — cannot download a prebuilt binary."; return 1; }
  command -v tar  >/dev/null 2>&1 || { warn "tar not found — cannot unpack a prebuilt binary.";  return 1; }

  detect_platform
  if [ -z "$OS" ] || [ -z "$ARCH" ]; then
    warn "no prebuilt binary for $(uname -s)/$(uname -m) — will build from source."
    return 1
  fi

  local tag ver base asset url tarball sums
  tag="$(resolve_tag)" || { warn "no published release found — will build from source."; return 1; }
  ver="${tag#v}"  # GoReleaser {{.Version}} drops the leading v
  asset="openbox_${ver}_${OS}_${ARCH}.tar.gz"
  base="https://github.com/${GH_OWNER}/${GH_REPO}/releases/download/${tag}"
  url="${base}/${asset}"
  tarball="${WORK}/${asset}"

  info "Downloading ${BIN_NAME} ${tag} for ${OS}/${ARCH} …"
  if ! curl -fsSL -o "$tarball" "$url"; then
    warn "prebuilt asset not found: ${url} — will build from source."
    return 1
  fi

  # Verify sha256 against the release checksums.txt (best-effort but preferred).
  sums="${WORK}/checksums.txt"
  if curl -fsSL -o "$sums" "${base}/checksums.txt" 2>/dev/null; then
    local want got
    want="$(grep " ${asset}\$" "$sums" 2>/dev/null | awk '{print $1}' | head -n1 || true)"
    got="$(sha256_of "$tarball" || true)"
    if [ -n "$want" ] && [ -n "$got" ]; then
      [ "$want" = "$got" ] || die "checksum mismatch for ${asset} (expected ${want}, got ${got}) — refusing to install."
      ok "Checksum verified (sha256)"
    else
      warn "could not verify checksum (missing entry or no sha256 tool) — proceeding on the TLS-authenticated download."
    fi
  else
    warn "no checksums.txt in the release — proceeding on the TLS-authenticated download."
  fi

  tar -xzf "$tarball" -C "$WORK" || { warn "failed to unpack ${asset} — will build from source."; return 1; }
  [ -x "${WORK}/${BIN_NAME}" ] || { warn "archive did not contain an executable ${BIN_NAME} — will build from source."; return 1; }

  BUILT="${WORK}/${BIN_NAME}"
  return 0
}

# ----------------------------------------------------------------------------- #
# Fallback path: build the unified engine from source (needs go 1.27+ and git)
# ----------------------------------------------------------------------------- #
build_from_source() {
  command -v git >/dev/null 2>&1 || die "git is required for the source build but was not found on PATH."
  command -v go  >/dev/null 2>&1 || die \
"Go 1.${MIN_GO_MINOR}+ is required for the source build but 'go' was not found on PATH.
   Either install Go from https://go.dev/dl/ and re-run, or install a tagged release
   (which ships a prebuilt binary) with:  OPENBOX_VERSION=vX.Y.Z bash install.sh"

  # Parse `go version` → major.minor and enforce >= 1.MIN_GO_MINOR.
  local GO_VER GO_MAJOR GO_REST GO_MINOR
  GO_VER="$(go env GOVERSION 2>/dev/null || go version | awk '{print $3}')"
  GO_VER="${GO_VER#go}"
  GO_MAJOR="${GO_VER%%.*}"
  GO_REST="${GO_VER#*.}"; GO_MINOR="${GO_REST%%.*}"
  if [ "${GO_MAJOR:-0}" -lt 1 ] || { [ "${GO_MAJOR:-0}" -eq 1 ] && [ "${GO_MINOR:-0}" -lt "$MIN_GO_MINOR" ]; }; then
    die "Go 1.${MIN_GO_MINOR}+ required; found ${GO_VER}. Upgrade from https://go.dev/dl/."
  fi
  ok "Go toolchain: ${GO_VER}"

  local SRC
  if [ -n "${OPENBOX_SRC:-}" ]; then
    SRC="$OPENBOX_SRC"
    [ -f "$SRC/go.mod" ] || die "OPENBOX_SRC=$SRC does not look like an openbox-shift-left checkout (no go.mod)."
    info "Building from existing checkout: $SRC"
  else
    SRC="$(mktemp -d "${TMPDIR:-/tmp}/openbox-shift-left.XXXXXX")"
    CLEANUP_SRC="$SRC"
    info "Cloning ${REPO_URL} @ ${REF} …"
    if ! git clone --quiet --depth 1 --branch "$REF" "$REPO_URL" "$SRC" 2>/dev/null; then
      git clone --quiet "$REPO_URL" "$SRC" || die \
"clone failed. If this is a private repo, either grant this machine access and
   retry, or clone it yourself and re-run with:
     OPENBOX_SRC=/path/to/openbox-shift-left bash install.sh"
      git -C "$SRC" checkout --quiet "$REF" || die "ref '$REF' not found in $REPO_URL"
    fi
    ok "Source ready"
  fi

  local VERSION OUT
  VERSION="$(git -C "$SRC" describe --tags --always --dirty 2>/dev/null || echo "0.1.0-dev")"
  OUT="$SRC/${BIN_NAME}"
  info "Building ${BIN_NAME} ${VERSION} (static, no-cgo) …"
  (
    cd "$SRC"
    CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o "$OUT" ./cmd/openbox
  )
  [ -x "$OUT" ] || die "build produced no binary at $OUT"
  BUILT="$OUT"
}

# ----------------------------------------------------------------------------- #
# Obtain the binary: prebuilt fast path, else source fallback
# ----------------------------------------------------------------------------- #
BUILT=""
if [ -n "${OPENBOX_SRC:-}" ] || [ "${OPENBOX_FROM_SOURCE:-0}" = "1" ]; then
  build_from_source
else
  install_prebuilt || build_from_source
fi
[ -n "$BUILT" ] && [ -x "$BUILT" ] || die "internal error: no binary produced."

if [ "${OPENBOX_SKIP_TESTS:-0}" != "1" ]; then
  "$BUILT" version >/dev/null 2>&1 || die "the ${BIN_NAME} binary failed to run ('$BUILT version')."
fi
ok "Ready: $("$BUILT" version 2>/dev/null || echo "$BIN_NAME")"

# ----------------------------------------------------------------------------- #
# Install onto PATH
# ----------------------------------------------------------------------------- #
mkdir -p "$INSTALL_DIR" || die "cannot create install dir $INSTALL_DIR"
DEST="$INSTALL_DIR/$BIN_NAME"

# Atomic-ish install: copy to a temp name in the same dir, then rename over.
TMP_DEST="$INSTALL_DIR/.${BIN_NAME}.new.$$"
if cp "$BUILT" "$TMP_DEST" 2>/dev/null && chmod 0755 "$TMP_DEST" 2>/dev/null && mv -f "$TMP_DEST" "$DEST" 2>/dev/null; then
  :
else
  rm -f "$TMP_DEST" 2>/dev/null || true
  warn "No write permission to $INSTALL_DIR — retrying with sudo."
  sudo install -m 0755 "$BUILT" "$DEST" || die "install to $DEST failed."
fi
ok "Installed → $DEST"

# ----------------------------------------------------------------------------- #
# PATH check + next steps
# ----------------------------------------------------------------------------- #
case ":${PATH}:" in
  *":${INSTALL_DIR}:"*) ON_PATH=1 ;;
  *)                    ON_PATH=0 ;;
esac
CMD="$([ "$ON_PATH" -eq 1 ] && echo "$BIN_NAME" || echo "$DEST")"

echo
if [ "$ON_PATH" -ne 1 ]; then
  warn "${INSTALL_DIR} is not on your PATH. Add it, e.g.:"
  printf '     %sexport PATH="%s:$PATH"%s   # add to ~/.bashrc or ~/.zshrc\n' "$DIM" "$INSTALL_DIR" "$RST"
  echo
fi

info "${BOLD}Next — wire OpenBox into Claude Code (one command):${RST}"
echo
printf '     export OPENBOX_CONTROL_TOKEN=<keycloak-jwt-or-obx_key_…>   # never a flag (INV-1)\n'
printf '     %s%s init --provider claude-code --backend-url https://<your-openbox-backend> [--enforce]%s\n' "$BOLD" "$CMD" "$RST"
printf '     %sSelf-hosted core? add%s --base-url https://<your-openbox-core> %s— without it the install points at the SaaS core.%s\n' "$DIM" "$RST" "$DIM" "$RST"
echo
printf '   That registers your agent, materializes the Claude Code plugin into\n'
printf '   ~/.claude/plugins/openbox-observe, stores your credentials, and pulls your policy.\n'
printf '   Governance is then AMBIENT — no daemon to run, no runtime env to set.\n'
printf '   Verify anytime with:  %s dev verify\n' "$CMD"
printf '\n   Full walkthrough (credentials, self-hosted, troubleshooting):\n'
printf '     %shttps://github.com/OpenBox-AI/openbox-shift-left/blob/main/docs/getting-started.md%s\n' "$DIM" "$RST"
echo
ok "Done."
