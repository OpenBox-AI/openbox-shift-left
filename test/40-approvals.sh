#!/usr/bin/env bash
# 40-approvals.sh — the E9 approval loop, scripted (scenarios A–F).
#
# Nothing here is simulated: a real org policy compiled into the real OPA
# bundle mints a real approval window, and the decisions are made through
# `openbox approve` under the approver's own credential.
#
# The approver runs as a background poller rather than a person watching a
# terminal — that is the only substitution, and it is the same REST call a
# person's click makes.
#
# One property of headless operation shapes every scenario below: `claude -p`
# does not exit while the async rewake watcher is still waiting on an UNDECIDED
# approval, and that watcher's window is 45 minutes. Interactively this is
# invisible (the developer is still sitting in the session). Here it means the
# hook's answer must be read from the audit while the session is still up, and
# every request must be decided before the session can be joined. The marker
# directory is shared, so one leftover request hangs the NEXT session too —
# hence settle() between scenarios.
set -uo pipefail

TB_DIR="$(cd "$(dirname "$0")" && pwd)"
. "$TB_DIR/env.sh"
. "$TB_DIR/lib/assert.sh"
. "$TB_DIR/lib/sql.sh"
. "$TB_DIR/lib/session.sh"
. "$TB_DIR/lib/policy.sh"

[ -d "$TB_PROJECT/.claude" ] || tb_fatal "project not governed — run 10-onboard.sh first"
TB_AGENT="$(tb_state_get agent_id)"
AGENT="$TB_AGENT"
[ -n "$AGENT" ] || tb_fatal "no agent id in state — run 10-onboard.sh first"

export OPENBOX_ENFORCE=1
AUDIT="$OPENBOX_ENFORCEMENT_FILE"
PENDING_DIR="$OPENBOX_PENDING_APPROVAL_DIR"

# No local bundle to keep permissive any more (ADR-0017): the server is the only
# decider, so core's policy is the only thing that can gate — which is what this
# phase was always really testing.

tb_audit_size() { [ -r "$AUDIT" ] && wc -c <"$AUDIT" | tr -d ' ' || echo 0; }
tb_audit_since() { tail -c "+$(($1 + 1))" "$AUDIT" 2>/dev/null; }

# ── the org policy + the queue: test/lib/policy.sh ─────────────────────────
# tb_gate_on / tb_gate_off, tb_pending_first, tb_settle and friends live there
# because 70-approver-auto.sh needs exactly the same gate.
pending_first() { tb_pending_first; }
pending_json() { tb_pending_json; }
release_pending() { tb_release_pending; }
settle() { tb_settle; }
opa_decision() { tb_opa_decision "$1"; }

# decide_when_pending answers the first request that appears, in the background,
# the way an approver watching the queue would.
decide_when_pending() { # <allow|deny> <deadline-seconds>
	(
		local i=0 id=""
		while [ "$i" -lt "$2" ]; do
			id="$(tb_pending_first)"
			if [ -n "$id" ]; then
				"$TB_BIN" approve "$1" "$id" --org "$OPENBOX_ORG_ID" >"$TB_STATE/approve-$1.out" 2>&1
				printf '%s' "$id" >"$TB_STATE/decided-id"
				exit 0
			fi
			i=$((i + 1))
			sleep 1
		done
	) &
	APPROVER_PID=$!
}

# wait_for_audit blocks until a needle appears in the audit written after an
# offset — the hook's answer, which lands long before the session ends.
wait_for_audit() { # <needle> <seconds> <offset>
	local i=0
	while [ "$i" -lt "$2" ]; do
		case "$(tb_audit_since "$3")" in *"$1"*) return 0 ;; esac
		i=$((i + 1))
		sleep 1
	done
	return 1
}

tb_step "install the gating policy"
rm -f "$TB_STATE/decided-id"
# A previous run that was interrupted can leave a request undecided, and the
# counts below are about THIS run. Start from an empty queue.
for _ in 1 2 3 4 5; do [ -n "$(pending_first)" ] || break; release_pending; done
rm -f "$PENDING_DIR"/*.json 2>/dev/null
path="$(tb_policy_apply "openbox test — approvals" "$TB_GATE_REGO")"
assert_nonempty "policy created" "$path"
if tb_wait_for_opa Bash require_approval; then
	tb_ok "OPA serves require_approval for Bash"
else
	tb_fatal "OPA never served the policy (decision: $(opa_decision Bash)) — nothing below can work"
fi
assert_eq "Read is not gated" allow "$(opa_decision Read)"

# ── A · approved inside the hold ──────────────────────────────────────────────
tb_step "A · an approver answers inside the hold"
export OPENBOX_APPROVAL_HOLD_MS=20000
before="$(tb_audit_size)"
decide_when_pending allow 30
start="$(date +%s)"
tb_session_bg "Run the shell command: echo scenario-a" "Bash"
tb_session_wait 150 || { tb_bad "the approved session ended" "an ended session" "still running"; release_pending; }
elapsed=$(($(date +%s) - start))
wait "$APPROVER_PID" 2>/dev/null
audit="$(tb_audit_since "$before")"
assert_nonempty "an approval was filed and decided" "$(tb_state_get decided-id)"
assert_contains "the hook took the approver's answer" "$audit" '"source":"approval:decided"'
assert_contains "and it was an ALLOW" "$audit" '"verdict":"ALLOW"'
tb_note "held for ${elapsed}s (hold budget 20s)"
settle

# ── B · nobody answers ────────────────────────────────────────────────────────
tb_step "B · nobody answers — deny, never a silent allow"
export OPENBOX_APPROVAL_HOLD_MS=6000
before="$(tb_audit_size)"
# Backgrounded on purpose: with the request left undecided, the async rewake
# watcher keeps `claude -p` alive for its whole 45-minute window. The hook's
# answer lands in the audit long before that, and that is what B is about.
tb_session_bg "Run the shell command: echo scenario-b" "Bash"
if wait_for_audit '"source":"evaluate"' 120 "$before"; then
	audit="$(tb_audit_since "$before")"
	assert_contains "the call was denied" "$audit" '"applied_decision":"deny"'
	assert_contains "the deny came from the escalation, not a local prompt" "$audit" '"source":"evaluate"'
	assert_contains "the audit carries a resolvable approval reference" "$audit" '"approval_ref"'
else
	tb_bad "the escalation denied within the hold" "an evaluate deny" "nothing in the audit after 120s"
fi
pending_b="$(pending_first)"
assert_nonempty "the request is still pending for a human" "$pending_b"

# ── C · a late approval ───────────────────────────────────────────────────────
tb_step "C · the late approval is claimed exactly once"
# B's request is still pending and its marker is still on disk. Deciding it now
# IS the rewake path — and the session ending is the proof the watcher saw it.
assert_ge "a pending-approval marker was left" 1 "$(find "$PENDING_DIR" -type f 2>/dev/null | wc -l)"
c_before="$(tb_audit_size)"
"$TB_BIN" approve allow "$pending_b" --org "$OPENBOX_ORG_ID" >"$TB_STATE/approve-late.out" 2>&1
assert_eq "the late approval was accepted" 0 "$?"
assert_eq "decided in the database" 0 "$(tb_val "select count(*) from governance_events where id='$pending_b' and decided_at is null;")"
if tb_session_wait 120; then
	tb_ok "the watcher woke on the decision and released the session"
else
	tb_bad "the watcher woke on the decision" "the session to end" "still waiting after 120s"
fi
assert_eq "the marker was claimed exactly once" 0 "$(find "$PENDING_DIR" -type f 2>/dev/null | wc -l)"

# The model was told, and it retried — inside the SAME session, which is the
# scope an operation identity is compared in (client/operation.go: the hash
# distinguishes operations "within one session"). The retry must re-enter the
# same governance event and read its now-decided verdict rather than filing a
# second request. A retry from a NEW session legitimately mints its own
# approval, so that is deliberately not asserted here.
after="$(tb_audit_since "$c_before")"
assert_contains "the retry was allowed" "$after" '"verdict":"ALLOW"'
assert_contains "on the same approval, not a new one" "$after" "$pending_b"
assert_eq "nothing was left pending" 0 "$(tb_val "select count(*) from governance_events where agent_id='$AGENT' and approval_expired_at is not null and decided_at is null;")"
settle

# ── D · rejected ──────────────────────────────────────────────────────────────
tb_step "D · a rejection blocks on the decision"
before="$(tb_audit_size)"
decide_when_pending deny 40
tb_session_bg "Run the shell command: echo scenario-d" "Bash"
tb_session_wait 150 || { tb_bad "the rejected session ended" "an ended session" "still running"; release_pending; }
wait "$APPROVER_PID" 2>/dev/null
audit="$(tb_audit_since "$before")"
assert_contains "the approver's refusal was applied" "$audit" '"source":"approval:decided"'
assert_contains "and it blocked" "$audit" '"applied_decision":"deny"'
settle

# ── E · the cost when nothing needs approving ─────────────────────────────────
tb_step "E · an ungated class costs nothing"
before="$(tb_audit_size)"
start="$(date +%s)"
sid="$(tb_session "Read README.md and reply with its first line." "Read")"
elapsed=$(($(date +%s) - start))
audit="$(tb_audit_since "$before")"
assert_absent "no approval was filed" "$audit" '"approval_ref"'
assert_eq "no approval window was minted" 0 "$(tb_val "select count(*) from governance_events where run_id='$sid' and approval_expired_at is not null;")"
tb_note "ungated session took ${elapsed}s — the 30s ceiling is a ceiling, not a delay"

# ── F · an MCP call carries what it is asking to do ───────────────────────────
tb_step "F · an MCP escalation is decidable (OD-E9-7)"
export TB_MCP_CONFIG="$TB_MCP"
export OPENBOX_APPROVAL_HOLD_MS=8000
before="$(tb_audit_size)"
tb_session_bg "Call the everything MCP server's echo tool with the message 'scenario-f'." "mcp__everything__echo"
wait_for_audit '"approval_ref"' 150 "$before" || tb_note "no MCP escalation recorded within 150s"
mcp_pending="$(pending_first)"
if [ -n "$mcp_pending" ]; then
	row="$(pending_json)"
	assert_contains "the queue shows the MCP tool" "$row" "mcp__everything__echo"
	input="$(tb_val "select input::text from governance_events where id='$mcp_pending';")"
	assert_contains "and what it was asked to do (arguments)" "$input" "scenario-f"
	"$TB_BIN" approve deny "$mcp_pending" --org "$OPENBOX_ORG_ID" >/dev/null 2>&1
else
	tb_bad "an MCP call filed an approval" "a pending request" "none"
fi
tb_session_wait 90 || { release_pending; tb_session_wait 30; }
settle

# ── G · an escalated call is still ONE activity ───────────────────────────────
# The gap this closes: 20-capture asserts started == completed, but it runs in
# observe mode, where nothing escalates. On the enforce path a gated PreToolUse
# reaches core twice — the inline evaluation POSTs the event synchronously and the
# observe copy is spooled and flushed — carrying ONE event_id both times. Core
# does not dedupe developer events on that id, so for a while every escalated
# call stored two ActivityStarted rows and two Merkle leaves, and no assertion
# anywhere looked. 20-capture's orphan check runs one way only (a completed half
# without a started one), so a DUPLICATED started half passed it unseen.
#
# Scoped to this file because this is where real escalations happen (the sessions
# above produced evaluate verdicts and filed approvals).
tb_step "G · no activity_id is stored more than once per half"
dupes="$(tb_val "select count(*) from (
	select activity_id, event_type from governance_events
	where activity_id is not null and event_type like 'Activity%'
	group by activity_id, event_type having count(*) > 1) d;")"
assert_eq "no duplicated activity half" 0 "$dupes"
if [ "${dupes:-0}" != "0" ]; then
	tb_note "duplicated: $(tb_sql "select activity_id||' '||event_type||' x'||count(*)
		from governance_events where activity_id is not null and event_type like 'Activity%'
		group by activity_id, event_type having count(*) > 1 limit 5;" | tr '\n' ' ')"
fi

# Since ADR-0014 a session carries TWO kinds of activity: tool calls (activity_type
# = the tool name) and model turns (activity_type = llm_completion). The check
# above is deliberately global, so it still covers both — but a single aggregate
# number cannot say WHICH kind duplicated, and the two have entirely different
# causes: a tool-call duplicate means the escalation and the observe copy both
# stored a row, while a turn duplicate means the transcript cursor advanced
# without its events being spooled. Splitting the count keeps the diagnosis in the
# failure message rather than in someone's later investigation.
for kind in tool turn; do
	case "$kind" in
	tool) pred="activity_type <> 'llm_completion'" ;;
	turn) pred="activity_type = 'llm_completion'" ;;
	esac
	kind_dupes="$(tb_val "select count(*) from (
		select activity_id, event_type from governance_events
		where activity_id is not null and event_type like 'Activity%' and $pred
		group by activity_id, event_type having count(*) > 1) d;")"
	assert_eq "no duplicated $kind activity half" 0 "$kind_dupes"
done
tb_note "activity kinds seen: $(tb_sql "select coalesce(activity_type,'(null)')||' x'||count(*)
	from governance_events where event_type like 'Activity%' group by 1 order by 1;" | tr '\n' ' ')"

# ── restore ───────────────────────────────────────────────────────────────────
tb_step "leave the agent ungated for the later phases"
# Deactivating alone leaves the compiled bundle in place, so publish an
# allow-only version: that is what forces OPA to rebuild.
if tb_gate_off; then
	tb_ok "OPA serves allow again"
else
	tb_bad "OPA serves allow again" allow "$(opa_decision Bash)"
fi
unset OPENBOX_APPROVAL_HOLD_MS TB_MCP_CONFIG

tb_finish
