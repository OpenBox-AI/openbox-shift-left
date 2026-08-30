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

tb_finish
