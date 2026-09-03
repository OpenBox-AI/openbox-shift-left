#!/usr/bin/env bash
# 00-preflight.sh — refuse to run the suite against a stack that cannot answer.
#
# The failure this exists for: OPA bound to its own container-localhost. Core
# then gets connection-refused on every evaluation, the behaviour path masks it
# as a fallback ALLOW and the policy path fail-closes to BLOCK — a stack that
# looks healthy and enforces nothing. So the probe
# here asks OPA for a *decision*, not for a heartbeat.
set -uo pipefail

TB_DIR="$(cd "$(dirname "$0")" && pwd)"
. "$TB_DIR/env.sh"
. "$TB_DIR/lib/assert.sh"
. "$TB_DIR/lib/sql.sh"

tb_step "tooling"
for tool in docker python3 curl git go claude npx; do
	if command -v "$tool" >/dev/null 2>&1; then tb_ok "$tool present"; else tb_bad "$tool present" "on PATH" "missing"; fi
done

tb_step "containers"
roster="$(docker ps --format '{{.Names}}' 2>/dev/null)"
for name in postgres openbox-core openbox-backend opa governance-worker attestation-worker openbox-fe; do
	assert_contains "$name running" "$roster" "$name"
done

tb_step "the stack answers"
assert_eq "core reachable" 200 "$(curl -s -o /dev/null -w '%{http_code}' "$OPENBOX_BASE_URL/" 2>/dev/null)"
assert_eq "backend healthy" 200 "$(curl -s -o /dev/null -w '%{http_code}' "$OPENBOX_BACKEND_URL/health" 2>/dev/null)"
assert_eq "dashboard serving" 200 "$(curl -s -o /dev/null -w '%{http_code}' "$TB_FE_URL/" 2>/dev/null)"
assert_eq "database reachable" 2 "$(tb_val 'select 1+1;')"

tb_step "OPA decides (not merely up)"
opa="$(curl -s -X POST -H 'content-type: application/json' -d '{"input":{}}' "$TB_OPA_URL/v1/data" 2>/dev/null)"
assert_nonempty "OPA returned a decision id" "$(tb_json "$opa" decision_id)"
assert_nonempty "OPA is serving an org bundle" "$(tb_json "$opa" result.org)"

tb_step "harness credential (P1)"
if [ -z "${OPENBOX_CONTROL_TOKEN:-}" ]; then
	tb_bad "control token available" "a token" "unset — run: ./test/env.sh mint"
else
	tb_api "/organization/$OPENBOX_ORG_ID/approvals?status=pending" >/dev/null
	assert_eq "credential can read the org" 200 "$(tb_status)"
fi

# The suite installs real daemons under fixed launchd/systemd names and writes
# the real user-scope settings file. env.sh pins OPENBOX_HOME, the spool and
# nine other coordinates, but it does NOT pin $HOME — project scope used to be
# the isolation, and the hooks now land user-wide. So a host that already
# carries a real install cannot be told apart from the test one afterwards, and
# `openbox uninstall` would take the developer's own with it.
#
# This is a fail-closed safety control, not a convenience. The labels are fixed
# constants, so they collide regardless of $HOME. Resolve the real settings
# path from the login home rather than $HOME: the harness has already changed
# it by the time this runs.
tb_step "the host carries no real OpenBox install"
tb_login_home="$(eval echo "~$(id -un)")"
tb_real_units=""
case "$(uname -s)" in
Darwin) tb_real_units="$(launchctl list 2>/dev/null | grep 'ai\.openbox' || true)" ;;
Linux) tb_real_units="$(systemctl --user list-units 'openbox*' --no-legend 2>/dev/null || true)" ;;
esac
if [ -n "$tb_real_units" ]; then
	tb_fatal "this machine already runs an OpenBox lane daemon:
$tb_real_units
  The lane labels are fixed (ai.openbox.gateway/.telemetry/.transport), so the suite
  would replace YOUR units and the teardown would remove them. Run \`openbox uninstall\`
  first, then re-run this suite."
fi
tb_real_hooks=0
if [ -f "$tb_login_home/.claude/settings.json" ]; then
	tb_real_hooks="$(grep -c 'hook claude-code' "$tb_login_home/.claude/settings.json" 2>/dev/null)"
	tb_real_hooks="${tb_real_hooks:-0}"
fi
if [ "$tb_real_hooks" -gt 0 ]; then
	tb_fatal "$tb_login_home/.claude/settings.json already registers $tb_real_hooks OpenBox hook(s).
  The suite writes that same file, so it would repoint your own governance at a test
  build and the teardown would remove it. Run \`openbox uninstall\` first, then re-run."
fi
tb_ok "no real install to collide with"

tb_finish
