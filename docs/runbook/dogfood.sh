#!/usr/bin/env bash
# Dogfood the Claude Code path: drive the REAL openbox-cc-hook binary with the
# exact hook payloads Claude Code sends (event name as argv, JSON on stdin), then
# let SessionEnd flush the spool through the AIP-signed client to a capture sink.
# Observe-only, metadata-only — the same code path a live session uses.
#
# Prereqs: build the binaries (see RUNBOOK §3) and start the capture sink:
#   python3 docs/runbook/capture_server.py events.log 8787 &
# Then:  bash docs/runbook/dogfood.sh
set -euo pipefail

REPO="$(cd "$(dirname "$0")/../.." && pwd)"
BIN="${OPENBOX_BIN:-$REPO/bin}"
WORK="$(mktemp -d)"; trap 'rm -rf "$WORK"' EXIT

# Smoke-test identity (NO backend): direct-override creds. In production
# `openbox dev init` mints these into the OS secret store instead.
export OPENBOX_BASE_URL="${OPENBOX_BASE_URL:-http://127.0.0.1:8787}"
export OPENBOX_AGENT_DID="did:aip:$(python3 -c 'import uuid;print(uuid.uuid4())')"
export OPENBOX_API_KEY="obx_test_$(python3 -c 'import secrets;print(secrets.token_hex(24))')"
export OPENBOX_ED25519_SEED="$(openssl rand -base64 32)"
export OPENBOX_SPOOL_DIR="$WORK/spool"
export OPENBOX_CONFIG="$WORK/no-such-config.json"   # force env-only resolution

SID="dogfood-$(python3 -c 'import uuid;print(uuid.uuid4())')"
echo "session_id    = $SID"
echo "developer_did = $OPENBOX_AGENT_DID"
echo "base_url      = $OPENBOX_BASE_URL"
echo

hook() { printf '%s' "$2" | "$BIN/openbox-cc-hook" "$1"; }
common() { printf '"session_id":"%s","cwd":"%s","permission_mode":"default"' "$SID" "$REPO"; }

# A realistic slice: start -> prompt -> four tool pairs -> end.
hook SessionStart     "{$(common),\"hook_event_name\":\"SessionStart\",\"source\":\"startup\",\"model\":\"claude-opus-4-8\"}"
hook UserPromptSubmit "{$(common),\"hook_event_name\":\"UserPromptSubmit\"}"
hook PreToolUse       "{$(common),\"hook_event_name\":\"PreToolUse\",\"tool_name\":\"Bash\",\"tool_input\":{\"command\":\"go test ./...\"}}"
hook PostToolUse      "{$(common),\"hook_event_name\":\"PostToolUse\",\"tool_name\":\"Bash\",\"tool_input\":{\"command\":\"go test ./...\"}}"
hook PreToolUse       "{$(common),\"hook_event_name\":\"PreToolUse\",\"tool_name\":\"Read\",\"tool_input\":{\"file_path\":\"$REPO/actions/openbox-git-action/resolve.go\"}}"
hook PostToolUse      "{$(common),\"hook_event_name\":\"PostToolUse\",\"tool_name\":\"Read\",\"tool_input\":{\"file_path\":\"$REPO/actions/openbox-git-action/resolve.go\"}}"
hook PreToolUse       "{$(common),\"hook_event_name\":\"PreToolUse\",\"tool_name\":\"Edit\",\"tool_input\":{\"file_path\":\"$REPO/actions/openbox-git-action/deploy.go\"}}"
hook PostToolUse      "{$(common),\"hook_event_name\":\"PostToolUse\",\"tool_name\":\"Edit\",\"tool_input\":{\"file_path\":\"$REPO/actions/openbox-git-action/deploy.go\"}}"
hook PreToolUse       "{$(common),\"hook_event_name\":\"PreToolUse\",\"tool_name\":\"mcp__claude_ai_Datadog__search\",\"tool_input\":{}}"
hook PostToolUse      "{$(common),\"hook_event_name\":\"PostToolUse\",\"tool_name\":\"mcp__claude_ai_Datadog__search\",\"tool_input\":{}}"

# SessionEnd drains the spool through the AIP-signed client to the sink.
hook SessionEnd       "{$(common),\"hook_event_name\":\"SessionEnd\",\"reason\":\"logout\"}"
echo "done — 11 events emitted to $OPENBOX_BASE_URL (inspect the sink's log)."
