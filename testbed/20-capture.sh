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
assert_ge "tool activity stored" 4 "$(tb_count "governance_events where run_id='$sid' and event_type='ActivityStarted'")"

tb_step "spans, by class"
types="$(tb_sql "select distinct span_type from spans where session_id='$uuid' order by 1;" | tr '\n' ' ')"
tb_note "span types: $types"
assert_contains "file read captured" "$types" "file_"
assert_contains "shell command captured" "$types" "shell_command"
assert_ge "file write captured" 1 "$(tb_count "spans where session_id='$uuid' and span_type='file_write'")"
# The MCP assertion is why the everything server exists here: before this, no
# MCP span had ever reached the local stack, so the mapper's mcp_server /
# mcp_tool extraction and core's classification of it were untested end to end.
mcp_spans="$(tb_count "spans where session_id='$uuid' and (name ilike '%mcp%' or attributes::text ilike '%mcp%')")"
assert_ge "MCP call captured" 1 "$mcp_spans"
tb_note "MCP span types: $(tb_sql "select distinct span_type from spans where session_id='$uuid' and (name ilike '%mcp%' or attributes::text ilike '%mcp%');" | tr '\n' ' ')"

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
spool="$XDG_CONFIG_HOME/openbox/cc-spool"
assert_eq "no events left spooled for this session" 0 "$(find "$spool" -name "*$sid*" 2>/dev/null | wc -l)"

tb_step "privacy posture (INV-2 / SL3-SEC-3)"
# Everything the runtime egressed for this session, as text.
egress="$(tb_sql "select row_to_json(e)::text from governance_events e where run_id='$sid';")
$(tb_sql "select row_to_json(s)::text from spans s where session_id='$uuid';")"
assert_nonempty "egress captured for inspection" "$egress"
assert_contains "prompt content egressed (content_capture on)" "$egress" "$PROMPT_MARK"
assert_absent "shell command text never egressed" "$egress" "$SHELL_MARK"
assert_absent "file body never egressed" "$egress" "$FILE_MARK"

tb_finish
