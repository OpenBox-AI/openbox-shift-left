#!/usr/bin/env bash
# Git-side dogfood: SL-5 commit-trailer stamping + SL-6 server-side deploy
# resolution, in an ISOLATED throwaway repo (never touches a real repo).
set -euo pipefail
REPO="$(cd "$(dirname "$0")/../.." && pwd)"
BIN="${OPENBOX_BIN:-$REPO/bin}"

R="$(mktemp -d)"; trap 'rm -rf "$R"' EXIT
cd "$R"
git init -q -b main
git config user.name Dev; git config user.email dev@example.com; git config commit.gpgsign false

# Install the SL-5 prepare-commit-msg hook into THIS throwaway repo.
"$BIN/openbox-git-hook" install "$R/.git/hooks" >/dev/null
echo "installed SL-5 hook into throwaway repo $R"
export OPENBOX_GIT_HOOK="$BIN/openbox-git-hook"

# A base (human, no session) then two agent commits under different sessions.
# OPENBOX_SESSION is the explicit override; in a live session the CC adapter's
# worktree-scoped registry supplies the id to the git subprocess instead.
git commit -q --allow-empty -m "chore: init repo"
BASE=$(git rev-parse HEAD)
OPENBOX_SESSION="sess-alice" git commit -q --allow-empty -m "feat: add deploy resolver"
OPENBOX_SESSION="sess-bob"   git commit -q --allow-empty -m "fix: bound message reads"
HEAD_SHA=$(git rev-parse HEAD)

echo
echo "=== the two agent commits (note the auto-stamped OpenBox-Session trailers) ==="
git log "$BASE".."$HEAD_SHA" --format='  %h  %s   ->   trailer:%(trailers:key=OpenBox-Session,valueonly,separator=+)'

echo
echo "=== a human commit gets NO trailer (SL-6 will mark it unattributed) ==="
git commit -q --allow-empty -m "docs: tweak README"   # no OPENBOX_SESSION in scope
git log -1 --format='  %h  %s   ->   trailer:[%(trailers:key=OpenBox-Session,valueonly)]'

echo
echo "=== SL-6: resolve the push range $BASE..$HEAD_SHA (multi-session fan-in) + emit Deploy ==="
export OPENBOX_BASE_URL="${OPENBOX_BASE_URL:-http://127.0.0.1:8787}"
export OPENBOX_DID="did:aip:$(python3 -c 'import uuid;print(uuid.uuid4())')"
export OPENBOX_API_KEY="obx_test_$(python3 -c 'import secrets;print(secrets.token_hex(24))')"
export OPENBOX_SEED="$(openssl rand -base64 32)"
"$BIN/openbox-git-action" --dir "$R" --sha "$HEAD_SHA" --base "$BASE" --repo "openbox-ai/dogfood" --environment production
