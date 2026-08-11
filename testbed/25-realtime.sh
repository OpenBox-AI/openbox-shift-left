#!/usr/bin/env bash
# 25-realtime.sh — telemetry reaches core WHILE the session is still running.
#
# Before the realtime trigger (hookflow.RealtimeTrigger, default on), delivery
# was batch-at-end: nothing for a session existed in core until SessionEnd
# drained the spool. The proof here is ordering, not latency: the session's
# WorkflowStarted and tool activity are queryable in core while the driver
# process is still alive — then the join shows completeness survived (exactly
# one WorkflowStarted/WorkflowCompleted, so overlapping realtime + teardown
# drains never double-counted; server-side Idempotency-Key dedupe, E8-S7).
set -uo pipefail

TB_DIR="$(cd "$(dirname "$0")" && pwd)"
. "$TB_DIR/env.sh"
. "$TB_DIR/lib/assert.sh"
. "$TB_DIR/lib/sql.sh"
. "$TB_DIR/lib/session.sh"

# Observe posture; realtime is the DEFAULT (no OPENBOX_REALTIME set) — the
# default-on posture is exactly what this phase verifies.
export OPENBOX_ENFORCE=0
[ -d "$TB_PROJECT/.claude" ] || tb_fatal "project not governed — run 10-onboard.sh first"

AGENT="$(tb_state_get agent_id)"
[ -n "$AGENT" ] || tb_fatal "no agent_id in state — run 10-onboard.sh first"
prev_sid="$(tb_state_get run_id)"

tb_step "background a real session that works, then idles"
# The sleep is the observation window: while the session sits in it, the
# earlier hooks' events must already be in core if delivery is real-time.
tb_session_bg "Do exactly these steps in order, nothing else:
1. Read README.md.
2. Run this exact shell command: sleep 25.
3. Reply DONE." "Read Bash"
tb_note "driver pid $TB_SESSION_PID"

# The newest WorkflowStarted for this agent, minted in the last 3 minutes.
newest_started() {
	tb_val "select run_id from governance_events where agent_id='$AGENT' and event_type='WorkflowStarted' and created_at > now() - interval '3 minutes' order by created_at desc limit 1;"
}

tb_step "the session is visible in core while the driver is still running"
sid=""
i=0
while [ "$i" -lt 45 ]; do
	kill -0 "$TB_SESSION_PID" 2>/dev/null || break
	cand="$(newest_started)"
	if [ -n "$cand" ] && [ "$cand" != "$prev_sid" ]; then
		sid="$cand"
		break
	fi
	i=$((i + 1))
	sleep 1
done
assert_nonempty "WorkflowStarted stored mid-session" "$sid"
[ -n "$sid" ] || {
	tb_session_wait 120
	tb_fatal "no mid-session delivery — realtime flush regressed to batch-at-end? $(tail -3 "$TB_STATE/last-session.err" 2>/dev/null)"
}
if kill -0 "$TB_SESSION_PID" 2>/dev/null; then
	tb_ok "driver still alive at first sighting — delivery was mid-session, not teardown"
else
	tb_bad "driver still alive at first sighting" "running driver" "already exited (cannot distinguish realtime from batch)"
fi
status_now="$(tb_val "select status from sessions where run_id='$sid';")"
assert_ne "session not sealed at first sighting" completed "$status_now"
tb_note "status at first sighting: ${status_now:-<none>}"

# Tool activity streams too: the Read/Bash events land while the session idles
# in its sleep, not just the turn-start burst.
#
# Counts BOTH activity types. A tool call is an ActivityStarted plus an
# ActivityCompleted (ADR-0013), and the completed half is often what arrives on
# the debounced flush — counting only starts would under-report the progress
# signal this phase exists to observe.
mid_activity() { tb_count "governance_events where run_id='$sid' and event_type like 'Activity%'"; }
i=0
while [ "$i" -lt 30 ] && [ "$(mid_activity)" -lt 1 ]; do
	kill -0 "$TB_SESSION_PID" 2>/dev/null || break
	i=$((i + 1))
	sleep 1
done
if kill -0 "$TB_SESSION_PID" 2>/dev/null; then
	assert_ge "tool activity stored mid-session" 1 "$(mid_activity)"
else
	tb_skip "tool activity stored mid-session" "session finished before the poll caught it"
fi

tb_step "join the session — completeness intact, nothing double-counted"
tb_session_wait 120 || tb_note "driver outlived its deadline and was killed"
tb_wait_for completed 30 tb_val "select status from sessions where run_id='$sid';"
assert_eq "session sealed as completed" completed "$(tb_val "select status from sessions where run_id='$sid';")"
assert_eq "exactly one WorkflowStarted (realtime + teardown drains never double-send)" 1 "$(tb_count "governance_events where run_id='$sid' and event_type='WorkflowStarted'")"
assert_eq "exactly one WorkflowCompleted" 1 "$(tb_count "governance_events where run_id='$sid' and event_type='WorkflowCompleted'")"
assert_ge "tool activity stored" 1 "$(tb_count "governance_events where run_id='$sid' and event_type='ActivityStarted'")"
assert_ge "completed halves stored too" 1 "$(tb_count "governance_events where run_id='$sid' and event_type='ActivityCompleted'")"
# The realtime path must not duplicate a half: a mid-session flush and the
# teardown drain both touching one tool call would show up as more completed
# rows than started ones.
assert_eq "no half double-sent across realtime + teardown drains" \
	"$(tb_count "governance_events where run_id='$sid' and event_type='ActivityStarted'")" \
	"$(tb_count "governance_events where run_id='$sid' and event_type='ActivityCompleted'")"

tb_finish
