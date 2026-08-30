#!/usr/bin/env bash
# 35-telemetry.sh — tool outcome, failure/lifecycle signals, and the assistant
# turn span (ADR-0018).
#
# ═══ STATUS: DORMANT — WRITTEN, NEVER RUN ════════════════════════════════════
# No local stack has been reachable since these assertions were written. They are
# merged deliberately rather than held back, so the next person with a stack gets
# coverage instead of a TODO — but nothing here has ever executed, and a first run
# should expect to fix mechanics (SQL column names above all) before it finds a
# real defect. Do not cite this file as evidence until it has run.
# ═════════════════════════════════════════════════════════════════════════════
#
# The single list of what a live run must confirm is MAPPING.md §7 items 15-21.
# This file is the executable form of that list and deliberately does not restate
# it — if the two disagree, MAPPING is the contract.
#
# What makes this its own phase rather than assertions bolted onto 20-capture or
# 28-usage: it needs a session that FAILS a tool call and one that spawns a
# subagent. Neither existing phase drives either, and bending them into it would
# make their own counts noisier for no gain.
#
# The load-bearing checks, in the order their failures matter:
#
#   status on the completed row  ⇒ Tool Health can compute anything at all
#   a failed call stored failed  ⇒ SUCCESS% means something
#   ONE span, llm_completion     ⇒ Goal Alignment has text to score
#   capture off ⇒ no span rows   ⇒ the gate is real server-side, not just on the wire
#   signal_args NULL on the new signals ⇒ the alignment goal is not being overwritten
set -uo pipefail

TB_DIR="$(cd "$(dirname "$0")" && pwd)"
. "$TB_DIR/env.sh"
. "$TB_DIR/lib/assert.sh"
. "$TB_DIR/lib/sql.sh"
. "$TB_DIR/lib/session.sh"

# Observe posture: the outcome field is orthogonal to enforcement, and a gate
# here would only add escalation rows to counts that are about shape.
export OPENBOX_ENFORCE=0
# Deliberately NOT setting OPENBOX_FINOPS or OPENBOX_CONTENT_CAPTURE, so this
# phase also proves the defaults: an unconfigured session captures both.
unset OPENBOX_FINOPS OPENBOX_CONTENT_CAPTURE 2>/dev/null || true
[ -d "$TB_PROJECT/.claude" ] || tb_fatal "project not governed — run 10-onboard.sh first"

# ── P0: the preconditions that decide success before any client code matters ──
# Two of these live outside this repo and both produce "the feature is broken" as
# their symptom, so they are checked FIRST and reported as skips rather than
# failures. A phase that reports a red alignment assertion because Redis is down
# has told the reader something false.
tb_step "preconditions"
# TB_LLAMAFIREWALL_HOST is supplied by whoever runs the test; it mirrors the
# stack's own LlamaFirewallHost setting and exists only so this phase can tell
# "alignment is broken" from "alignment was never going to run".
if [ -z "${TB_LLAMAFIREWALL_HOST:-}" ]; then
	tb_note "TB_LLAMAFIREWALL_HOST unset — performTraceCheck returns nil (llama_firewall.go:31-34)"
	tb_note "  the alignment assertions below will SKIP, not fail: both widgets stay empty with a perfect client"
	ALIGNMENT_READY=0
else
	ALIGNMENT_READY=1
fi
# A reused agent carries tool.<name>.failed accumulated from before status
# shipped, so SUCCESS% shows partial recovery rather than 100% and reads as a
# broken fix. The metric assertions below are therefore written as "success_calls
# > 0", not "== total".
tb_note "agent id in use: $(tb_state_get agent_id) — a REUSED agent shows partial recovery, not 100%"

run="$(date +%s)"
FAIL_MARK="OBXFAIL$run"

# ── a session that succeeds, and one that fails ──────────────────────────────
tb_step "a tool call that succeeds reports status completed"
sid="$(tb_session "Read README.md and say the word $FAIL_MARK." "Read")"
uuid="$(tb_session_uuid "$sid")"
tb_wait_for completed 30 tb_val "select status from sessions where run_id='$sid';"

completed_rows="$(tb_count "governance_events where run_id='$sid' and event_type='ActivityCompleted' and activity_type<>'llm_completion'")"
assert_ge "at least one tool ActivityCompleted" 1 "$completed_rows"
# THE assertion this phase exists for. The client sends `status`; core copies it
# into workflow_status (storage_event.go:417). If this is empty the field never
# arrived, and Tool Health stays at 0.0% no matter what the dashboard does.
assert_eq "every completed tool row carries a status" 0 \
	"$(tb_count "governance_events where run_id='$sid' and event_type='ActivityCompleted' and activity_type<>'llm_completion' and workflow_status is null")"
assert_ge "at least one row reports completed" 1 \
	"$(tb_count "governance_events where run_id='$sid' and event_type='ActivityCompleted' and workflow_status='completed'")"

tb_step "a tool call that FAILS reports status failed"
# A command that exits non-zero. PostToolUse does not fire for it; PostToolUseFailure
# does — which is the whole basis for deriving the outcome structurally.
sid_f="$(tb_session "Run exactly this bash command once and then stop: exit 3" "Bash")"
tb_wait_for completed 30 tb_val "select status from sessions where run_id='$sid_f';"
failed_rows="$(tb_count "governance_events where run_id='$sid_f' and workflow_status='failed'")"
if [ "$failed_rows" -eq 0 ]; then
	tb_bad "no failed tool row — either the model did not run the failing command, or PostToolUseFailure is not wired"
else
	tb_ok "a failed tool call stored as failed ($failed_rows row(s))"
	# A failure with no duration drops out of every latency percentile, and the
	# stash has to pair across two DIFFERENT hooks to produce one.
	assert_eq "the failed row carries a duration" 0 \
		"$(tb_count "governance_events where run_id='$sid_f' and workflow_status='failed' and duration_ms is null")"
fi
# The tool's own error text is free-form and deliberately unbound (ADR-0019).
assert_eq "no free-text tool error egressed" 0 \
	"$(tb_val "select count(*) from governance_events e where e.run_id='$sid_f' and row_to_json(e)::text like '%exit code 3%';")"

# ── the turn span ────────────────────────────────────────────────────────────
tb_step "one span per captured model turn, carrying the assistant text"
turns="$(tb_count "governance_events where run_id='$sid' and activity_type='llm_completion' and event_type='ActivityCompleted'")"
spans="$(tb_count "spans where session_id='$uuid'")"
assert_ge "at least one turn" 1 "$turns"
# Exactly one span per turn. More means the ids are not deduping; fewer means
# capture or the hook field is missing.
assert_eq "one span per completed turn" "$turns" "$spans"
assert_eq "every span classified as llm_completion" "$spans" \
	"$(tb_count "spans where session_id='$uuid' and span_type='llm_completion'")"
# If the row exists but the type is wrong, the synthesized http.* attributes
# stopped satisfying isLLMCall and alignment is silently dead — the failure mode
# with no error anywhere.
assert_eq "no span classified as something else" 0 \
	"$(tb_count "spans where session_id='$uuid' and span_type<>'llm_completion'")"
# The assistant actually said the mark, so its presence proves the text made the
# whole trip rather than an empty span having been stored.
assert_ge "the assistant text reached the span body" 1 \
	"$(tb_val "select count(*) from spans s where s.session_id='$uuid' and s.response_body like '%$FAIL_MARK%';")"

# ADR-0019 P3: thinking must NOT also be in that span. This is the assertion for
# the failure mode with no error anywhere — core reads this body as the
# assistant's REPLY, so chain-of-thought here would score every later turn's drift
# against the model's reasoning instead of its answer, and nothing would log.
#
# Asserted HERE and not in 20-capture.sh because this is the phase where a span is
# expected to EXIST; a non-leak check against zero spans proves nothing.
#
# Cross-field rather than marker-based: no prompt can make a model think a chosen
# phrase, so the needle is the stored thinking itself. It stays entirely inside
# SQL — the model's own text is never interpolated through the shell.
if [ "$(tb_count "governance_events where run_id='$sid' and output ? 'thinking'")" -gt 0 ]; then
	assert_eq "thinking did not ride the assistant span" 0 		"$(tb_val "select count(*) from spans s
			where s.session_id='$uuid'
			  and position(
			        left((select output->>'thinking' from governance_events
			              where run_id='$sid' and output ? 'thinking'
			              order by created_at desc limit 1), 60)
			        in coalesce(s.response_body,'')) > 0;")"
else
	tb_skip "thinking did not ride the assistant span" "no thinking block in this session (extended thinking may be off)"
fi

# ── alignment ────────────────────────────────────────────────────────────────
tb_step "goal alignment is fed"
if [ "$ALIGNMENT_READY" -eq 0 ]; then
	tb_skip "alignment evaluated" "LlamaFirewall not configured — a client-side pass cannot be shown from here"
else
	events="(select id from governance_events where run_id='$sid')"
	# span_id IS NULL distinguishes a trace-level evaluation from a per-span one.
	# The honest criterion is that an evaluation HAPPENED: a drift verdict is not
	# producible on demand and asserting one would make this phase flaky by design.
	assert_ge "an AGE evaluation exists for the session" 1 \
		"$(tb_count "age_evaluations where governance_event_id in $events")"
fi

# ── the lifecycle signals ────────────────────────────────────────────────────
tb_step "failure and lifecycle signals"
# A session that spawns a subagent. If the model declines to delegate, this is a
# skip rather than a failure — the assertion is about the wiring, and we cannot
# compel a tool choice.
sid_s="$(tb_session "Use the Task tool to launch one subagent that reads README.md, then stop." "Task,Read")"
tb_wait_for completed 60 tb_val "select status from sessions where run_id='$sid_s';"
subagents="$(tb_count "governance_events where run_id='$sid_s' and signal_name='subagent_started'")"
if [ "$subagents" -eq 0 ]; then
	tb_skip "subagent_started" "the model did not spawn a subagent this run"
else
	tb_ok "subagent_started recorded ($subagents)"
	assert_ge "the subagent's kind is attributed" 1 \
		"$(tb_val "select count(*) from governance_events where run_id='$sid_s' and signal_name='subagent_started' and metadata->>'agent_type' is not null;")"
fi

# THE signal assertion that is not about presence. Core reads any SignalReceived
# with non-empty signal_args as a NEW USER GOAL and overwrites the alignment
# session's goal with it (age.go:112-137) — so a non-null signal_args on one of
# these three means telemetry is destroying the thing alignment scores against.
for s in subagent_started permission_denied api_error; do
	assert_eq "$s carries no signal_args" 0 \
		"$(tb_val "select count(*) from governance_events where signal_name='$s' and signal_args is not null and signal_args::text <> 'null';")"
done
# prompt_submitted is the one that MUST have them — it is what creates the goal.
assert_ge "prompt_submitted still carries its args" 1 \
	"$(tb_val "select count(*) from governance_events where run_id='$sid' and signal_name='prompt_submitted' and signal_args is not null;")"

# permission_denied and api_error are not producible on demand: the first needs an
# auto-mode classifier denial, the second a provider-side API error. Their wiring
# is schema-verified only (see plans/reports/probe-260813-2329-…). Recorded as a
# gap rather than asserted into a false pass.
tb_note "permission_denied / api_error not driven: neither is producible on demand (probe report §Q2)"

# ── the negative: capture off ⇒ no span rows at all ──────────────────────────
# The strongest single check in the phase. One run validates the whole gate
# design, server-side rather than on the wire.
tb_step "content capture off ⇒ no span rows, but status survives"
sid_off="$(OPENBOX_CONTENT_CAPTURE=0 tb_session "Read README.md and say done." "Read")"
uuid_off="$(tb_session_uuid "$sid_off")"
tb_wait_for completed 30 tb_val "select status from sessions where run_id='$sid_off';"
assert_eq "no span rows with capture off" 0 "$(tb_count "spans where session_id='$uuid_off'")"
# …and the outcome field is NOT gated, so Tool Health does not depend on a
# privacy setting.
assert_ge "status still recorded with capture off" 1 \
	"$(tb_count "governance_events where run_id='$sid_off' and workflow_status='completed'")"

# …and neither does the tool content ADR-0019 P1 added. This is the closing half
# of the gate 20-capture.sh proves open: the same classes, the same session shape,
# the opposite posture. Without it, "capture off" would be verified for spans only
# while four newer content classes went unchecked end to end.
tb_step "content capture off ⇒ no tool input or output on any row"
# Asserted as JSONB key tests, NOT as a substring scan of row_to_json. `input` and
# `output` are real columns that row_to_json renders on EVERY row (as null when
# empty), so `assert_absent '"output":'` would fail on a perfectly correct run —
# it would be testing the column list, not the content. The nested keys below are
# the ones that appear only when Content survived the gate.
assert_eq "no tool command/arguments/body on any row with capture off" 0 \
	"$(tb_count "governance_events where run_id='$sid_off' and input is not null and (input ? 'command' or input ? 'arguments' or input ? 'content')")"
assert_eq "no tool output text on any row with capture off" 0 \
	"$(tb_count "governance_events where run_id='$sid_off' and output is not null and output ? 'output'")"
assert_eq "no refusal free text on any row with capture off" 0 \
	"$(tb_count "governance_events where run_id='$sid_off' and metadata is not null and (metadata ? 'denial_reason' or metadata ? 'error_details')")"
# ADR-0019 P3: thinking is the newest content class and the only one sourced from
# the TRANSCRIPT rather than a hook field, so a gate that holds for every
# hook-sourced field above proves nothing about it. Same JSONB key test, same
# reason: `output` is a column that renders on every row.
assert_eq "no thinking on any row with capture off" 0 \
	"$(tb_count "governance_events where run_id='$sid_off' and output is not null and output ? 'thinking'")"
# The structural axes must survive, or "no content" would be indistinguishable
# from "no events" — the same reason 20-capture.sh asserts the gate positively.
assert_ge "structural tool identity survives capture off" 1 \
	"$(tb_count "governance_events where run_id='$sid_off' and metadata ? 'tool_name'")"
# The turn's NUMBERS are on the same object thinking would have used, and they are
# gated by finops, not by content. If they vanished with capture off, the check
# above would pass for a client that simply stopped emitting turns.
assert_ge "turn usage survives capture off" 1 \
	"$(tb_count "governance_events where run_id='$sid_off' and activity_type='llm_completion' and output is not null and output ? 'usage'")"

tb_finish
