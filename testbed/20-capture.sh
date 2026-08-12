#!/usr/bin/env bash
# 20-capture.sh — one real session, five tool classes, and the privacy posture.
#
# The three markers are the design of this phase:
#
#   PROMPT marker  must be PRESENT  — content capture is on by default, and the
#                                     prompt is the one piece of content that
#                                     legitimately egresses.
#   SHELL marker   must be ABSENT   — a tool command never egresses on an observe
#   FILE marker    must be ABSENT     event (INV-2 / SL3-SEC-3).
#
# Neither the shell command nor the file body appears in the prompt: they are
# read out of files inside the project, so their absence downstream means the
# runtime dropped them, not that they were never mentioned.
set -uo pipefail

TB_DIR="$(cd "$(dirname "$0")" && pwd)"
. "$TB_DIR/env.sh"
. "$TB_DIR/lib/assert.sh"
. "$TB_DIR/lib/sql.sh"
. "$TB_DIR/lib/session.sh"

export TB_MCP_CONFIG="$TB_MCP"
# Observe posture, deliberately. A Tier-2 escalation is allowed to carry the
# command so an approver can decide (OD-E9-7, client.Content.ToolInput) — that
# copy is 40-approvals' business. What must hold HERE is the observe rule:
# ordinary telemetry never gains a command or a file body.
export OPENBOX_ENFORCE=0
[ -d "$TB_PROJECT/.claude" ] || tb_fatal "project not governed — run 10-onboard.sh first"

run="$(date +%s)"
PROMPT_MARK="OBXPROMPT$run"
SHELL_MARK="OBXSHELL$run"
FILE_MARK="OBXFILE$run"

tb_step "seed the project (markers live in files, never in the prompt)"
printf 'echo %s\n' "$SHELL_MARK" >"$TB_PROJECT/cmd.txt"
printf '%s\n' "$FILE_MARK" >"$TB_PROJECT/marker.txt"
printf 'PLACEHOLDER\n' >"$TB_PROJECT/notes.txt"
tb_ok "cmd.txt, marker.txt, notes.txt written"

tb_step "drive one real session"
sid="$(tb_session "Task $PROMPT_MARK. Do exactly these five steps in order, nothing else:
1. Read README.md.
2. Grep this project for the word governance.
3. Read cmd.txt and run the single line it contains as a shell command.
4. Read marker.txt and edit notes.txt, replacing PLACEHOLDER with what marker.txt contains.
5. Call the everything MCP server's echo tool with the message 'openbox testbed'.
Then reply DONE." "Read Grep Bash Edit mcp__everything__echo")"
assert_nonempty "session id returned" "$sid"
[ -n "$sid" ] || tb_fatal "no session — $(tail -3 "$TB_STATE/last-session.err" 2>/dev/null)"
tb_state_set run_id "$sid"
tb_note "run_id $sid"

tb_step "the session reached core"
tb_wait_for completed 30 tb_val "select status from sessions where run_id='$sid';"
uuid="$(tb_session_uuid "$sid")"
assert_nonempty "session row created" "$uuid"
tb_state_set session_uuid "$uuid"
assert_eq "session sealed as completed" completed "$(tb_val "select status from sessions where run_id='$sid';")"
assert_eq "WorkflowStarted stored" 1 "$(tb_count "governance_events where run_id='$sid' and event_type='WorkflowStarted'")"
assert_eq "WorkflowCompleted stored" 1 "$(tb_count "governance_events where run_id='$sid' and event_type='WorkflowCompleted'")"

# A tool call is TWO events sharing one activity_id (ADR-0013): ActivityStarted
# then ActivityCompleted, each its own row and each independently evaluated.
# Under the old hook shape both halves were ActivityStarted with the same
# activity_id, which matched core's whole dedupe key
# (agent_id, workflow_id, run_id, activity_id, event_type) — so the completed
# half never became a row at all. This step is what proves it does now.
tb_step "tool calls are activity pairs"
# Scoped to TOOL activities. Since ADR-0014 a session also emits model-turn
# activities (activity_type = llm_completion), which ride the same two wire types
# — so an unscoped count here would silently include turns and let "4 tool calls
# captured" pass on two tool calls plus two turns. Turn pairing is asserted in
# 28-usage.sh; this phase is about tool calls.
tool_pred="activity_type is distinct from 'llm_completion'"
started="$(tb_count "governance_events where run_id='$sid' and event_type='ActivityStarted' and $tool_pred")"
completed="$(tb_count "governance_events where run_id='$sid' and event_type='ActivityCompleted' and $tool_pred")"
tb_note "tool ActivityStarted $started · ActivityCompleted $completed"
tb_note "turn activities in this session: $(tb_count "governance_events where run_id='$sid' and activity_type='llm_completion'")"
assert_ge "tool calls captured" 4 "$started"
# Equality, not >=: every started half must have its completed half. The scripted
# session is deterministic enough to assert this, and a mismatch is exactly the
# failure mode worth catching (a dropped result, or a merged row).
assert_eq "every started half has a completed half" "$started" "$completed"

# The pairing invariant itself: no ActivityCompleted may exist without an
# ActivityStarted carrying the same activity_id. This is what puts the two rows
# on one dashboard row and what makes ONE approval cover both.
orphans="$(tb_val "select count(*) from governance_events c
	where c.run_id='$sid' and c.event_type='ActivityCompleted'
	and not exists (select 1 from governance_events s
		where s.run_id=c.run_id and s.event_type='ActivityStarted'
		and s.activity_id=c.activity_id);")"
# Unscoped on purpose: an unpaired completed half is wrong for EITHER activity
# kind, and this is the cheapest place to notice it.
assert_eq "no unpaired ActivityCompleted" 0 "$orphans"

assert_nonempty "activity_type is the tool name" \
	"$(tb_val "select activity_type from governance_events where run_id='$sid' and event_type='ActivityStarted' and $tool_pred and activity_type is not null limit 1;")"
tb_note "activity types: $(tb_sql "select distinct activity_type from governance_events where run_id='$sid' and event_type like 'Activity%' order by 1;" | tr '\n' ' ')"

# duration_ms is client-computed now — with no span there is nothing server-side
# to derive it from, and the dashboard reads event.duration_ms directly. The
# client OMITS it rather than sending zero when the cross-process start-time
# stash misses, so assert at least one real duration rather than requiring all.
assert_ge "a completed row carries a real duration" 1 \
	"$(tb_count "governance_events where run_id='$sid' and event_type='ActivityCompleted' and $tool_pred and duration_ms > 0")"
tb_note "durations: $(tb_sql "select coalesce(duration_ms::text,'(absent)') from governance_events where run_id='$sid' and event_type='ActivityCompleted' order by created_at limit 6;" | tr '\n' ' ')"

# activity_output carries structural counts only — never tool output text
# (INV-2). Its presence is what core runs Guardrails stage 1 over.
assert_ge "a completed row carries activity_output" 1 \
	"$(tb_count "governance_events where run_id='$sid' and event_type='ActivityCompleted' and $tool_pred and output is not null")"

tb_step "tool classes reached core"
# These used to be asserted as span_type values, which core computed from the
# span. With no span there is no server-side semantic_type, so the classes are
# now asserted where they actually live: activity_type (the tool name) and
# activity_input.kind / the file+mcp locators the client puts there.
kinds="$(tb_sql "select distinct input->>'kind' from governance_events where run_id='$sid' and event_type='ActivityStarted' and $tool_pred and input is not null order by 1;" | tr '\n' ' ')"
tb_note "activity_input kinds: $kinds"
assert_contains "file tool captured" "$kinds" "file"
assert_contains "shell tool captured" "$kinds" "shell"
assert_ge "a file locator reached core" 1 \
	"$(tb_count "governance_events where run_id='$sid' and event_type='ActivityStarted' and input->>'file_path' is not null")"
# The MCP assertion is why the everything server exists in this suite: before it,
# no MCP call had ever reached the local stack, so the mapper's mcp_server /
# mcp_tool extraction was untested end to end. It used to be checked against the
# span's mcp family fields; activity_input is their only home now.
assert_ge "MCP call captured with its server+tool" 1 \
	"$(tb_count "governance_events where run_id='$sid' and event_type='ActivityStarted' and input->>'mcp_server' is not null and input->>'mcp_tool' is not null")"

tb_step "zero spans — the accepted trade-off, asserted on purpose"
# NOT a bug and NOT something to "fix" by re-adding a span. A hook process has no
# in-process OpenTelemetry, so the spans shift-left used to send were fabricated
# by hand to satisfy a wire shape. ADR-0013 retired them. The cost is real and is
# recorded there: no span-level Merkle leaves and no server-side semantic_type
# for developer sessions. If this assertion ever fails, the span layer grew a
# caller again — read the ADR before changing it.
assert_eq "no spans rows for a dev session (ADR-0013)" 0 "$(tb_count "spans where session_id='$uuid'")"

tb_step "merkle — event leaves, no span leaves"
assert_ge "event leaves written" 2 \
	"$(tb_count "session_merkle_leaves where session_id='$uuid' and governance_event_id is not null")"
assert_eq "no span leaves" 0 "$(tb_count "session_merkle_leaves where session_id='$uuid' and span_id is not null")"
# Both halves of a tool call are attested, not just the start.
assert_ge "completed halves are attested too" 1 \
	"$(tb_val "select count(*) from session_merkle_leaves l
		join governance_events e on e.id = l.governance_event_id
		where l.session_id='$uuid' and e.event_type='ActivityCompleted';")"

tb_step "evaluation fan-out"
# Policy and guardrails only produce a row when the org has attached one to
# THIS agent. A freshly registered agent has neither, so the honest report is a
# skip naming the reason — 40-approvals attaches a policy and asserts the
# positive case there. AGE runs for every event and is asserted unconditionally.
events="(select id from governance_events where run_id='$sid')"
agent="$(tb_state_get agent_id)"
if [ "$(tb_count "policies where agent_id='$agent' and is_active=true")" -gt 0 ]; then
	assert_ge "policy evaluated" 1 "$(tb_count "policy_evaluations where governance_event_id in $events")"
else
	tb_skip "policy evaluated" "no policy attached to this agent yet (40-approvals creates one)"
fi
if [ "$(tb_count "guardrails where agent_id='$agent' and is_active=true")" -gt 0 ]; then
	assert_ge "guardrails evaluated" 1 "$(tb_count "guardrails_evaluations where governance_event_id in $events")"
else
	tb_skip "guardrails evaluated" "no guardrails attached to this agent"
fi
assert_ge "AGE evaluated" 1 "$(tb_count "age_evaluations where governance_event_id in $events")"

tb_step "spool drained at SessionEnd"
spool="$OPENBOX_SPOOL_DIR"
assert_eq "no events left spooled for this session" 0 "$(find "$spool" -name "*$sid*" 2>/dev/null | wc -l)"

tb_step "privacy posture (INV-2 / SL3-SEC-3)"
# Everything the runtime egressed for this session, as text. The spans query is
# kept deliberately: it returns nothing now (asserted above), and if a span ever
# reappears its contents are scanned for leaked content rather than silently
# skipped.
egress="$(tb_sql "select row_to_json(e)::text from governance_events e where run_id='$sid';")
$(tb_sql "select row_to_json(s)::text from spans s where session_id='$uuid';")"
assert_nonempty "egress captured for inspection" "$egress"
assert_contains "prompt content egressed (content_capture on)" "$egress" "$PROMPT_MARK"
assert_absent "shell command text never egressed" "$egress" "$SHELL_MARK"
assert_absent "file body never egressed" "$egress" "$FILE_MARK"

# ── the negative: an ungoverned directory produces NOTHING ───────────────────
# ADR-0016's accepted cost, demonstrated end to end rather than asserted. This is
# the assertion that makes "absence of events is not evidence of absence of work"
# a measured property of the product instead of a caveat in a document.
tb_step "a real session in a directory where init was not run"
ungoverned="$(tb_state_get ungoverned_project)"
if [ -z "$ungoverned" ] || [ ! -d "$ungoverned" ]; then
	tb_note "no ungoverned twin recorded — run 10-onboard.sh; skipping the negative scope assertion"
else
	before="$(tb_count "governance_events")"
	UNGOVERNED_MARK="ungoverned-$(date +%s)"
	sid_un="$(TB_SESSION_DIR="$ungoverned" tb_session "Say the word $UNGOVERNED_MARK and nothing else." "")"
	after="$(tb_count "governance_events")"
	assert_eq "no governance events from an ungoverned directory" "$before" "$after"
	if [ -n "$sid_un" ]; then
		assert_eq "no session row for it either" 0 "$(tb_count "governance_events where run_id='$sid_un'")"
	fi
	# And nothing about that session reached OpenBox at all, prompt included. The
	# whole row is scanned rather than one column, matching how the capture
	# assertions above inspect egress.
	assert_eq "its prompt never egressed" 0 \
		"$(tb_val "select count(*) from governance_events e where row_to_json(e)::text like '%$UNGOVERNED_MARK%';")"
	tb_ok "the ungoverned twin produced no rows — the scope gap is real and bounded"
fi

tb_finish
