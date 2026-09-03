#!/usr/bin/env bash
# 20-capture.sh — one real session, five tool classes, and the privacy posture.
#
# The three markers are the design of this phase:
#
#   PROMPT marker  must be PRESENT  — content capture is on by default, and the
#   SHELL marker   must be PRESENT     prompt, the tool input and the tool output
#   FILE marker    must be PRESENT     all egress under that one gate.
#
# SHELL and FILE used to be ABSENT assertions (INV-2 / SL3-SEC-3: "a tool command
# never egresses on an observe event"). **that decision retired that guarantee**
# tool input and output now ride ordinary tool events under `content_capture`,
# redacted and capped. The assertion is inverted rather than deleted, because
# "the marker is nowhere" and "the runtime emitted nothing at all" are the same
# observation, and only the positive form can tell them apart.
#
# that decision adds one more class under the same gate — the turn's THINKING — and
# it is checked differently on purpose: no prompt can make a model think a chosen
# phrase, so a marker assertion there would test the model's compliance rather
# than the pipeline. Presence of the `thinking` KEY is the check, and it is a skip
# rather than a failure when the session produced no block (extended thinking is a
# client setting this suite does not control).
#
# The gate's OTHER half — capture off ⇒ none of this egresses — is asserted
# server-side by 35-telemetry.sh, which already drives a capture-off session. That
# half is strict for thinking, because absence needs no cooperation from a model.
#
# The related check that thinking did NOT also ride the assistant's span lives in
# 35-telemetry.sh too, and deliberately not here: this phase asserts a dev session
# writes ZERO spans, and a non-leak assertion against zero spans proves nothing.
#
# Neither the shell command nor the file body appears in the prompt: they are
# read out of files inside the project, so their presence downstream means the
# runtime captured them from the tool call, not that the prompt mentioned them.
set -uo pipefail

TB_DIR="$(cd "$(dirname "$0")" && pwd)"
. "$TB_DIR/env.sh"
. "$TB_DIR/lib/assert.sh"
. "$TB_DIR/lib/sql.sh"
. "$TB_DIR/lib/session.sh"

export TB_MCP_CONFIG="$TB_MCP"
# Observe posture, deliberately. A gated call is allowed to carry the
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
5. Call the everything MCP server's echo tool with the message 'openbox test'.
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

# A tool call is TWO events sharing one activity_id : ActivityStarted
# then ActivityCompleted, each its own row and each independently evaluated.
# Under the old hook shape both halves were ActivityStarted with the same
# activity_id, which matched core's whole dedupe key
# (agent_id, workflow_id, run_id, activity_id, event_type) — so the completed
# half never became a row at all. This step is what proves it does now.
tb_step "tool calls are activity pairs"
# Scoped to TOOL activities. Since that decision a session also emits model-turn
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

tb_step "session-wide lifecycle pairing"
# THE assertion this suite was missing, and the one that would have caught every
# defect in the model-call capture repair. Everything above is filtered by
# $tool_pred to TOOL activity types, so the llm_completion rows sat outside the
# only guard there was -- and every in-path model call produced an
# ActivityCompleted with no ActivityStarted for the life of the feature.
#
# Unscoped, per activity_id, and it must return zero rows. Not a parity COUNT:
# that is fooled by two compensating errors, one missing Started plus one orphan
# Started netting to even. Not `total % 2` either: the total is even only when
# W + S is, so a one-prompt ended session is legitimately odd.
unpaired="$(tb_val "select count(*) from (
	select activity_id from governance_events
	where run_id='$sid' and event_type in ('ActivityStarted','ActivityCompleted')
	group by activity_id having count(*) <> 2) q;")"
if [ "${unpaired:-0}" != "0" ]; then
	tb_note "offending activity_ids: $(tb_sql "select activity_id || ' x' || count(*) from governance_events
		where run_id='$sid' and event_type in ('ActivityStarted','ActivityCompleted')
		group by activity_id having count(*) <> 2 limit 8;" | tr '\n' ' ')"
fi
assert_eq "every activity_id carries exactly two rows" 0 "$unpaired"

# The two companions. W is 2 for an ENDED session; a live one legitimately has
# WorkflowStarted and no WorkflowCompleted yet, which is why a naive check
# false-alarms on anything still running. This session has ended.
assert_eq "exactly one WorkflowStarted" 1 \
	"$(tb_count "governance_events where run_id='$sid' and event_type='WorkflowStarted'")"
assert_eq "exactly one WorkflowCompleted" 1 \
	"$(tb_count "governance_events where run_id='$sid' and event_type='WorkflowCompleted'")"
# total = W + 2A + S, so anything left over is an event type nobody decided about.
w="$(tb_count "governance_events where run_id='$sid' and event_type like 'Workflow%'")"
a="$(tb_val "select count(distinct activity_id) from governance_events where run_id='$sid' and event_type in ('ActivityStarted','ActivityCompleted');")"
sig="$(tb_count "governance_events where run_id='$sid' and event_type='SignalReceived'")"
total="$(tb_count "governance_events where run_id='$sid'")"
tb_note "W=$w A=$a S=$sig total=$total"
assert_eq "total = W + 2A + S" "$total" "$(( w + 2 * a + sig ))"

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

tb_step "zero spans, and the tension above it is now RESOLVED"
# The KNOWN TENSION this block used to carry is answered, by reading core rather
# than by running this suite: an embedded spans[] entry is parsed on the normal
# path and then DISCARDED. Persistence is gated on hook_trigger plus a
# pre-existing event row (governance_workflow.go:234, validation.go:154-174),
# which this client deliberately never sets, because that would put a model turn
# on core's approval-bypass fingerprint path. So the answer is zero spans either
# way, and the assertion that a turn span's ROW exists could never have passed.
#
# The client now sends no spans[] at all. NOT something to "fix" by re-adding
# one: a span describes work nested INSIDE an activity, and for a model call
# llm_completion IS the activity. See mapping.md §2. The cost is real and
# recorded there: no span-level Merkle leaves, no server-side semantic_type.
# The content moved to activity_input / activity_output, which do persist.
assert_eq "no spans rows for a dev session " 0 "$(tb_count "spans where session_id='$uuid'")"

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

tb_step "privacy posture (INV-2 — content gate ON)"
# Everything the runtime egressed for this session, as text. The spans query is
# kept deliberately: it returns nothing now (asserted above), and if a span ever
# reappears its contents are scanned for leaked content rather than silently
# skipped.
egress="$(tb_sql "select row_to_json(e)::text from governance_events e where run_id='$sid';")
$(tb_sql "select row_to_json(s)::text from spans s where session_id='$uuid';")"
assert_nonempty "egress captured for inspection" "$egress"
assert_contains "prompt content egressed (content_capture on)" "$egress" "$PROMPT_MARK"
# that decision: tool input and output egress under the same gate. Both markers are
# reachable two ways — as the tool's input and as what it printed — so either
# path satisfies these; the capture-off half is 35-telemetry.sh's job.
assert_contains "shell command text egressed (content_capture on)" "$egress" "$SHELL_MARK"
assert_contains "file body egressed (content_capture on)" "$egress" "$FILE_MARK"

# that decision: the turn's THINKING, on the llm_completion row's activity_output.
# Asserted as presence of the KEY rather than of a marker string, because nothing
# in a prompt can make a model think a chosen phrase — a marker assertion here
# would be a test of the model's compliance, not of the pipeline.
#
# It is a SOFT check: extended thinking has to be active for the session to
# produce any block at all, and that is a client setting this suite does not
# control. A skip naming the reason is honest; asserting it into a false failure
# is not. The capture-off half (35-telemetry.sh) is the strict one.
if [ "$(tb_count "governance_events where run_id='$sid' and activity_type='llm_completion' and output is not null and output ? 'thinking'")" -gt 0 ]; then
	assert_ge "turn thinking egressed (content_capture on)" 1 \
		"$(tb_count "governance_events where run_id='$sid' and output ? 'thinking'")"
	tb_note "thinking bytes: $(tb_val "select length(output->>'thinking') from governance_events where run_id='$sid' and output ? 'thinking' order by created_at desc limit 1;")"
else
	tb_skip "turn thinking egressed" "no thinking block in this session (extended thinking may be off — see mapping.md §7 item 22)"
fi

# ── the positive control: ANY directory is governed ──────────────────────────
# Inverted with user-wide hooks. This used to demonstrate the accepted cost of
# project scope: a real session in a never-initialized directory produced
# nothing, which made "absence of events is not evidence of absence of work" a
# measured property rather than a caveat.
#
# That cost is gone, and the claim worth measuring now is the opposite one. A
# session in a directory this suite never touched must produce rows, because
# that is what one install governing the whole machine means. The genuine
# negative moved to after `openbox uninstall`, where absence proves removal.
tb_step "a real session in a directory where init was never run"
ungoverned="$(tb_state_get ungoverned_project)"
if [ -z "$ungoverned" ] || [ ! -d "$ungoverned" ]; then
	tb_note "no control project recorded — run 10-onboard.sh; skipping the global-scope assertion"
else
	GLOBAL_MARK="global-$(date +%s)"
	sid_un="$(TB_SESSION_DIR="$ungoverned" tb_session "Say the word $GLOBAL_MARK and nothing else." "")"
	if [ -z "$sid_un" ]; then
		tb_bad "the control session produced a session id" "an id" "empty"
	else
		assert_ne "the control directory produced governance rows" 0 \
			"$(tb_count "governance_events where run_id='$sid_un'")"
		tb_ok "a directory this suite never initialized is governed — global scope is real"
	fi
fi

tb_finish
