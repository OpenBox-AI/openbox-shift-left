#!/usr/bin/env bash
# test/lib/policy.sh — author the org policy that makes approvals happen.
#
# The SERVER's verdict is what mints an approval window; a local bundle saying
# require_approval only causes the escalation. So a phase that needs approvals
# has to put a real policy in the backend and wait for OPA to serve it — and
# check that it did, because "the policy exists" and "OPA is enforcing it" are
# different facts, ~20s apart.
#
# Shared by 40-approvals.sh and 70-approver-auto.sh. Requires TB_AGENT (the
# agent whose policy this is) to be set by the caller.

# The rule core's OPA client expects: a `result` document carrying decision +
# reason. activity_type is the tool name as shift-left sends it.
TB_GATE_REGO='default result := {"decision": "allow", "reason": "no rule matched"}

result := {"decision": "require_approval", "reason": "shell execution requires human approval"} if {
	input.event_type == "ActivityStarted"
	input.activity_type == "Bash"
}

result := {"decision": "require_approval", "reason": "MCP tool call requires human approval"} if {
	input.event_type == "ActivityStarted"
	startswith(input.activity_type, "mcp__")
}'

TB_ALLOW_REGO='default result := {"decision": "allow", "reason": "test: no gate"}'

# tb_policy_apply publishes a new current policy version for TB_AGENT and prints
# its OPA path.
tb_policy_apply() { # <name> <rego>
	python3 - "$OPENBOX_BACKEND_URL" "$TB_AGENT" "$OPENBOX_CONTROL_TOKEN" "$1" "$2" <<'PY'
import json, sys, urllib.request, urllib.error
backend, agent, token, name, rego = sys.argv[1:6]
body = json.dumps({"name": name, "description": "openbox test policy", "rego_code": rego}).encode()
req = urllib.request.Request(f"{backend}/agent/{agent}/policies", data=body,
    headers={"Content-Type": "application/json", "X-API-Key": token}, method="POST")
try:
    p = json.load(urllib.request.urlopen(req)).get("data", {})
    print((p.get("config") or {}).get("path", ""))
except urllib.error.HTTPError as e:
    print("", file=sys.stdout); print(f"HTTP {e.code}: {e.read().decode()[:200]}", file=sys.stderr)
PY
}

# tb_opa_decision asks OPA what the agent's CURRENT policy says about a tool —
# the only way to know the compiled bundle actually reached it.
tb_opa_decision() { # <tool>
	local path
	path="$(tb_val "select config->>'path' from policies where agent_id='$TB_AGENT' and is_current_version=true limit 1;")"
	[ -n "$path" ] || {
		echo "NOPOLICY"
		return
	}
	curl -s -X POST "$TB_OPA_URL/v1/data/$path" -H 'Content-Type: application/json' \
		-d "{\"input\":{\"event_type\":\"ActivityStarted\",\"activity_type\":\"$1\"}}" |
		python3 -c 'import sys,json;r=json.load(sys.stdin).get("result",{}).get("result");print(r["decision"] if r else "UNDEFINED")' 2>/dev/null
}

tb_wait_for_opa() { # <tool> <want>
	local i=0
	while [ "$i" -lt 60 ]; do
		[ "$(tb_opa_decision "$1")" = "$2" ] && return 0
		i=$((i + 1))
		sleep 1
	done
	return 1
}

# tb_gate_on / tb_gate_off bracket a phase that needs approvals. Deactivating a
# policy leaves the compiled bundle in place, so going back to ungated means
# publishing an allow-only version — that is what forces OPA to rebuild.
tb_gate_on() { # <phase-name>
	tb_policy_apply "openbox test — $1" "$TB_GATE_REGO" >/dev/null
	tb_wait_for_opa Bash require_approval
}

tb_gate_off() {
	tb_policy_apply "openbox test — ungated" "$TB_ALLOW_REGO" >/dev/null
	tb_wait_for_opa Bash allow
}

# tb_pending_json / tb_pending_first read the org's queue through the API the
# approver and the dashboard both use.
tb_pending_json() { tb_api "/organization/$OPENBOX_ORG_ID/approvals?status=pending"; }

tb_pending_first() { # → id of the oldest pending request for TB_AGENT, if any
	python3 - "$(tb_pending_json)" "$TB_AGENT" <<'PY'
import json, sys
try:
    rows = json.loads(sys.argv[1])["data"]["approvals"]["data"]
except Exception:
    rows = []
rows = [r for r in rows if r.get("agent_id") == sys.argv[2]]
rows.sort(key=lambda r: r.get("created_at") or "")
print(rows[0]["id"] if rows else "")
PY
}

# tb_release_pending is the safety valve: an undecided request keeps a rewake
# watcher — and so a headless session — alive for 45 minutes.
tb_release_pending() {
	local id
	id="$(tb_pending_first)"
	[ -n "$id" ] && "$TB_BIN" approve deny "$id" --org "$OPENBOX_ORG_ID" >/dev/null 2>&1
}

# tb_settle clears the queue between scenarios. Not tidiness: the marker
# directory is shared, so ONE leftover request hangs the NEXT session.
tb_settle() {
	local i=0
	while [ -n "$(tb_pending_first)" ] && [ "$i" -lt 5 ]; do
		tb_release_pending
		i=$((i + 1))
	done
	i=0
	while [ -n "$(find "${TB_PENDING_DIR:-$XDG_CONFIG_HOME/openbox/pending-approvals}" -type f 2>/dev/null | head -1)" ] && [ "$i" -lt 15 ]; do
		sleep 1
		i=$((i + 1))
	done
	rm -f "${TB_PENDING_DIR:-$XDG_CONFIG_HOME/openbox/pending-approvals}"/*.json 2>/dev/null
}
