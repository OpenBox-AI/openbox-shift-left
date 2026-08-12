#!/usr/bin/env bash
# testbed/env.sh — the one place that knows where the local stack is.
#
#   . testbed/env.sh          # source it: every phase script does this first
#   ./testbed/env.sh mint     # run it: mint the read credential (P1), once
#   ./testbed/env.sh drop     # deactivate that credential again
#
# Two things make this harness safe to run on a working machine:
#
#   Every path openbox writes is pinned into testbed/.state — the credential file
#   and dev.json via OPENBOX_HOME, and each runtime-state file via its own
#   override (see below; XDG_CONFIG_HOME alone does NOT isolate them on macOS).
#   Nothing touches the developer's real ~/.openbox or OS config dir. Enforcement
#   is now ON by default (ADR-0016), which makes this load-bearing rather than
#   tidy: a testbed posture leaking into the real config would govern every
#   Claude Code session on the box.
#
#   Hooks are installed into one scratch project only, which since ADR-0016 is
#   simply `init`'s default scope — the phase runs it from inside $TB_PROJECT.
#   Sessions started anywhere else stay ungoverned, and 10-onboard.sh asserts
#   that rather than assuming it.
#
# Secrets are never written here. `mint` stores the control token in
# testbed/.state/control-token (git-ignored, 0600) and sourcing picks it up.

TB_REPO="$(cd "$(dirname "${BASH_SOURCE[0]:-$0}")/.." && pwd)"
export TB_REPO
export TB_DIR="$TB_REPO/testbed"
export TB_STATE="${TB_STATE:-$TB_DIR/.state}"
mkdir -p "$TB_STATE"

# The stack (local-stack/docker-compose.local.yml).
export OPENBOX_BACKEND_URL="${OPENBOX_BACKEND_URL:-http://localhost:3000}"
export OPENBOX_BASE_URL="${OPENBOX_BASE_URL:-http://localhost:8086}"
export TB_OPA_URL="${TB_OPA_URL:-http://localhost:8181}"
export TB_FE_URL="${TB_FE_URL:-http://localhost:3233}"
export TB_PG="${TB_PG:-openbox-local-postgres-1}"
export TB_PG_DB="${TB_PG_DB:-openbox}"
export OPENBOX_ORG_ID="${OPENBOX_ORG_ID:-openbox.ai}"
export OPENBOX_ORG="${OPENBOX_ORG:-$OPENBOX_ORG_ID}"

# Isolation (see the header). Runtime state — spool, bundle, audit logs — still
# resolves through XDG_CONFIG_HOME; configuration and credentials resolve through
# OPENBOX_HOME (ADR-0015).
export XDG_CONFIG_HOME="${XDG_CONFIG_HOME_OVERRIDE:-$TB_STATE/config}"
mkdir -p "$XDG_CONFIG_HOME"

# XDG_CONFIG_HOME ALONE IS NOT ENOUGH ON macOS. Go's os.UserConfigDir() returns
# $HOME/Library/Application Support on darwin and does not consult
# XDG_CONFIG_HOME at all, so on a Mac the runtime-state paths derived from it —
# spool, bundle, enforcement log, advisories, findings cursor, pending approvals,
# stale markers, the session registry — resolved to the developer's REAL config
# directory while this file claimed they were isolated. A stray hook run during
# development is enough to write there, which is how this was found.
#
# Every one of those paths has an explicit override, so they are all pinned here
# rather than trusted to derive correctly. Cheaper than reasoning about
# os.UserConfigDir() per platform, and it fails visibly if a new state file
# appears without an override.
export OPENBOX_SPOOL_DIR="${OPENBOX_SPOOL_DIR:-$TB_STATE/state/spool}"
export OPENBOX_SIDECAR_BUNDLE="${OPENBOX_SIDECAR_BUNDLE:-$TB_STATE/state/policy-bundle.json}"
export OPENBOX_ENFORCEMENT_FILE="${OPENBOX_ENFORCEMENT_FILE:-$TB_STATE/state/enforcements.jsonl}"
export OPENBOX_ADVISORY_FILE="${OPENBOX_ADVISORY_FILE:-$TB_STATE/state/advisories.jsonl}"
export OPENBOX_FINDINGS_CURSOR="${OPENBOX_FINDINGS_CURSOR:-$TB_STATE/state/findings.cursor}"
export OPENBOX_PENDING_APPROVAL_DIR="${OPENBOX_PENDING_APPROVAL_DIR:-$TB_STATE/state/pending-approvals}"
export OPENBOX_STALE_DIR="${OPENBOX_STALE_DIR:-$TB_STATE/state/stale}"
mkdir -p "$TB_STATE/state"

# ~/.openbox, relocated: dev.json, approver.json and the .env credential file.
# It must be ABSOLUTE — devconfig rejects a relative OPENBOX_HOME, because a
# hook's working directory is whatever project the tool happens to be in.
export OPENBOX_HOME="${OPENBOX_HOME_OVERRIDE:-$TB_STATE/openbox-home}"
mkdir -p "$OPENBOX_HOME"
chmod 700 "$OPENBOX_HOME" 2>/dev/null || true

# Credentials are a plaintext 0600 file inside OPENBOX_HOME (ADR-0015). There is
# no keyring to unlock and no backend to select any more, which is what makes a
# headless run possible without a prompt — the old harness had to opt into a file
# backend explicitly to get here.
export TB_ENV_FILE="$OPENBOX_HOME/.env"

# The governed scratch project, the binary under test, and the driver's model.
export TB_PROJECT="${TB_PROJECT:-/tmp/openbox-testbed-project}"
export TB_BIN="${TB_BIN:-$TB_STATE/openbox}"
export TB_MODEL="${TB_MODEL:-sonnet}"
export TB_KEY_NAME="${TB_KEY_NAME:-openbox-testbed}"
export TB_AGENT_NAME="${TB_AGENT_NAME:-openbox-testbed-claude-code}"
# The MCP fixture. A phase that wants it exports TB_MCP_CONFIG="$TB_MCP";
# sessions elsewhere pay nothing for it.
export TB_MCP="${TB_MCP:-$TB_DIR/mcp/everything.json}"

# The control token: minted by `./testbed/env.sh mint`, or supplied by the
# operator. Never a flag and never committed (INV-1).
if [ -z "${OPENBOX_CONTROL_TOKEN:-}" ] && [ -r "$TB_STATE/control-token" ]; then
	OPENBOX_CONTROL_TOKEN="$(cat "$TB_STATE/control-token")"
	export OPENBOX_CONTROL_TOKEN
fi
[ -r "$TB_DIR/env.local.sh" ] && . "$TB_DIR/env.local.sh"

# State handed between phases: the agent id 10 registered, the session 20 ran,
# the commit 50 made. One value per file, so a half-finished run leaves
# something readable behind.
tb_state_set() { printf '%s' "$2" >"$TB_STATE/$1"; }
tb_state_get() { cat "$TB_STATE/$1" 2>/dev/null || true; }

# tb_psql runs one statement as the stack's postgres superuser. Used only by
# mint/drop here; assertions go through lib/sql.sh.
tb_psql() { docker exec -i "$TB_PG" psql -U postgres -d "$TB_PG_DB" -q -t -A -c "$1"; }

# tb_mint_key creates the org API key the harness asserts through: read-only,
# plus manage:agent_session so the approver half of 40-approvals can decide.
tb_mint_key() {
	local token hash
	token="obx_key_$(openssl rand -hex 24)"
	hash="$(printf %s "$token" | sha256sum | cut -d' ' -f1)"
	tb_psql "INSERT INTO api_keys (organization_id,name,key_hash,key_prefix,permissions,is_active,created_at,updated_at)
	 VALUES ('$OPENBOX_ORG_ID','$TB_KEY_NAME','$hash',left('$token',12),
	 ARRAY['create:agent','read:agent','read:agent_session','read:agent_log',
	       'create:agent_policy','read:agent_policy','manage:agent_session'],true,now(),now());" >/dev/null
	umask 077
	printf '%s' "$token" >"$TB_STATE/control-token"
	echo "minted $TB_KEY_NAME → $TB_STATE/control-token"
}

tb_drop_key() {
	tb_psql "update api_keys set is_active=false where name='$TB_KEY_NAME';" >/dev/null
	rm -f "$TB_STATE/control-token"
	echo "deactivated $TB_KEY_NAME"
}

# Executed rather than sourced: the mint/drop entrypoint.
case "${BASH_SOURCE[0]:-}" in
"$0")
	set -euo pipefail
	case "${1:-}" in
	mint) tb_mint_key ;;
	drop) tb_drop_key ;;
	*) echo "usage: ./testbed/env.sh <mint|drop>   (to use the settings: . testbed/env.sh)" >&2; exit 2 ;;
	esac
	;;
esac
