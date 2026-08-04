#!/usr/bin/env bash
# 10-onboard.sh — onboard for real: register a developer agent against the local
# backend and govern exactly one scratch project.
#
# This is the product's own front door, not a fixture: the same `init` a
# developer runs, against the same backend, writing the same config. What the
# harness changes is only WHERE it writes (XDG_CONFIG_HOME, see env.sh) and how
# far it reaches (--local-hooks, one project).
set -uo pipefail

TB_DIR="$(cd "$(dirname "$0")" && pwd)"
. "$TB_DIR/env.sh"
. "$TB_DIR/lib/assert.sh"
. "$TB_DIR/lib/sql.sh"

CONFIG="$XDG_CONFIG_HOME/openbox/dev.json"
HOOKS="$TB_PROJECT/.claude/settings.local.json"

tb_step "build the binary under test"
go build -o "$TB_BIN" "$TB_REPO/cli/cmd/openbox" || tb_fatal "go build failed"
tb_ok "built $TB_BIN"

tb_step "a fresh governed project"
# The identity survives a re-run by default: an agent's API key and signing key
# are shown exactly once, so wiping the credential store strands the agent in
# the org. TB_FRESH=1 opts into a clean registration (and a new agent).
rm -rf "$TB_PROJECT"
[ "${TB_FRESH:-0}" = "1" ] && rm -rf "$XDG_CONFIG_HOME/openbox"
mkdir -p "$TB_PROJECT"
git -C "$TB_PROJECT" init -q -b main || tb_fatal "git init failed"
git -C "$TB_PROJECT" config user.name "OpenBox Testbed"
git -C "$TB_PROJECT" config user.email "testbed@openbox.ai"
git -C "$TB_PROJECT" config commit.gpgsign false
printf '# OpenBox testbed project\n\nA scratch project whose Claude Code sessions are governed.\n' >"$TB_PROJECT/README.md"
git -C "$TB_PROJECT" add README.md && git -C "$TB_PROJECT" commit -qm "chore: init testbed project"
tb_ok "project at $TB_PROJECT"

tb_step "openbox init"
[ -n "${OPENBOX_CONTROL_TOKEN:-}" ] || tb_fatal "no control token — run ./testbed/env.sh mint"

# One stable agent per machine, so repeated runs do not sprawl the org's seats.
tb_init() { # [extra flags…]
	"$TB_BIN" init \
		--provider claude-code \
		--agent-name "$TB_AGENT_NAME" \
		--enforce \
		--install-git-hook \
		--local-hooks "$TB_PROJECT" \
		--backend-url "$OPENBOX_BACKEND_URL" \
		--base-url "$OPENBOX_BASE_URL" \
		--secret-backend file "$@" >"$TB_STATE/init.out" 2>&1
}

if tb_init; then
	tb_ok "init succeeded"
elif grep -q "already exists in this org" "$TB_STATE/init.out"; then
	# The org holds an agent of this name whose one-time keys we no longer have.
	# Registering a distinctly-named one is the only recovery the product offers.
	tb_note "$TB_AGENT_NAME exists remotely with no local credentials — registering a new one (--force)"
	if tb_init --force; then tb_ok "init succeeded (--force)"; else tb_bad "init succeeded" 0 "$(tail -2 "$TB_STATE/init.out")"; fi
else
	tb_bad "init succeeded" 0 "$(tail -2 "$TB_STATE/init.out")"
fi
[ -r "$CONFIG" ] || tb_fatal "no dev.json at $CONFIG — $(tail -3 "$TB_STATE/init.out")"

cfg="$(cat "$CONFIG")"
agent_id="$(tb_json "$cfg" agent_id)"
did="$(tb_json "$cfg" developer_did)"
tb_state_set agent_id "$agent_id"
tb_state_set did "$did"

tb_step "the config it wrote"
assert_contains "config is testbed-scoped, not ~/.config" "$CONFIG" "$TB_STATE"
assert_nonempty "agent_id persisted" "$agent_id"
assert_nonempty "developer_did persisted" "$did"
assert_eq "enforce persisted (ADR-0006: no runtime env)" true "$(tb_json "$cfg" enforce)"
assert_eq "backend_url persisted" "$OPENBOX_BACKEND_URL" "$(tb_json "$cfg" backend_url)"
# The data-plane URL a self-hosted install must carry: without it the hook and
# `dev verify` sign their requests at the SaaS core and get a 401 that reads as a
# broken install.
assert_eq "base_url persisted (self-hosted core)" "$OPENBOX_BASE_URL" "$(tb_json "$cfg" base_url)"
assert_eq "git-hook stamping enabled" true "$(tb_json "$cfg" install_git_hook)"

tb_step "the agent it registered"
assert_eq "agent row is a developer agent" developer "$(tb_val "select agent_type from agents where id='$agent_id';")"
assert_eq "AIP signing required on every event" t "$(tb_val "select signing_required from agents where id='$agent_id';")"
assert_eq "agent belongs to the harness org" "$OPENBOX_ORG_ID" "$(tb_val "select organization_id from agents where id='$agent_id';")"

tb_step "hooks, scoped to one project"
[ -r "$HOOKS" ] || tb_fatal "no $HOOKS — --local-hooks did not write"
hooks="$(cat "$HOOKS")"
for ev in SessionStart UserPromptSubmit PreToolUse PostToolUse SessionEnd; do
	assert_contains "$ev wired" "$hooks" "hook claude-code $ev"
done
assert_contains "PreToolUse holds to 30s (E9-S4)" "$hooks" '"timeout": 30'
assert_contains "PreToolUse reads as a reason, not a freeze" "$hooks" "OpenBox governance"
assert_contains "rewake watcher rides alongside the gate" "$hooks" "rewake claude-code"
assert_contains "rewake is async, so it cannot gate" "$hooks" '"asyncRewake": true'

# The plugin bundle is materialised into ~/.claude/plugins but activation is a
# separate, deliberate step. If it were enabled here, every session on this
# machine would be governed by the testbed's config — the exact accident
# a global enforce posture causes.
enabled="$(cat "$HOME/.claude/settings.json" 2>/dev/null || echo '{}')"
assert_absent "plugin not globally enabled — scope holds" "$enabled" "openbox-observe"

tb_step "verify + doctor"
"$TB_BIN" dev verify >"$TB_STATE/verify.out" 2>&1
assert_eq "dev verify succeeded" 0 "$?"
assert_file_contains "verify names the DID it authenticated as" "$TB_STATE/verify.out" "$did"

"$TB_BIN" doctor >"$TB_STATE/doctor.out" 2>&1
doctor="$(cat "$TB_STATE/doctor.out")"
assert_contains "doctor reports enforce" "$doctor" "enforce"
assert_contains "doctor reports require_verified_bundle (G8)" "$doctor" "require_verified_bundle"
assert_contains "doctor reports provenance, not just values" "$doctor" "from "

tb_finish
