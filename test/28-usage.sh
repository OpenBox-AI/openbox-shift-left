#!/usr/bin/env bash
# 28-usage.sh — per-turn model + token usage, and the arithmetic that makes it
# worth having.
#
# The design of this phase is that COUNTING is the assertion, not existence.
# "Usage arrived" is nearly worthless here: every interesting failure mode —
# a double-counted turn, an off-by-one cursor, a missed turn, a subagent whose
# tokens are claimed twice — passes an existence check and fails a count. The
# standing lesson is the duplicate-ActivityStarted bug on the escalation path, which shipped
# because the only assertion that would have caught it ran in a mode where the
# bug could not occur.
#
# So the load-bearing checks are:
#
#   T turns          ⇒ T llm_completion pairs, each id once Started + once Completed
#   indexes          ⇒ contiguous from 0, no gaps
#   Σ per-turn       ⇒ == the SessionEnd rollup, FIELD BY FIELD (all four counts)
#   subagent tokens  ⇒ counted exactly once, attributed to their agent
#   capture disabled ⇒ zero llm_completion rows, no model anywhere
set -uo pipefail

TB_DIR="$(cd "$(dirname "$0")" && pwd)"
. "$TB_DIR/env.sh"
. "$TB_DIR/lib/assert.sh"
. "$TB_DIR/lib/sql.sh"
. "$TB_DIR/lib/session.sh"

# Observe posture: usage capture is orthogonal to enforcement, and a gate here
# would only add noise to the counts.
export OPENBOX_ENFORCE=0
# Default posture for the capture itself — deliberately NOT setting OPENBOX_FINOPS,
# so this phase also proves that decision default: an unconfigured session captures.
unset OPENBOX_FINOPS 2>/dev/null || true
[ -d "$TB_PROJECT/.claude" ] || tb_fatal "project not governed — run 10-onboard.sh first"

run="$(date +%s)"
PROMPT_MARK="OBXUSAGE$run"
FILE_MARK="OBXUFILE$run"
SHELL_MARK="OBXUSHELL$run"

# ── the session ────────────────────────────────────────────────────────────────
# A deterministic turn count is what the pair-count assertion needs. Claude Code
# closes a turn (fires Stop) when it stops to answer, so a prompt sequence with
# T explicit "then stop and reply" steps yields T turns. If the provider ever
# batches differently the invariants below (pairing, contiguity, Σ == rollup) all
# still hold — only the absolute count would move, which is why the count is
# asserted alongside them rather than instead of them.
tb_step "seed the project (markers live in files, never in the prompt)"
printf 'echo %s\n' "$SHELL_MARK" >"$TB_PROJECT/ucmd.txt"
printf '%s\n' "$FILE_MARK" >"$TB_PROJECT/umarker.txt"
tb_ok "ucmd.txt, umarker.txt written"

tb_step "drive one real session with a subagent task"
sid="$(tb_session "Task $PROMPT_MARK. Do exactly these steps in order, nothing else:
1. Read README.md, then reply READ-DONE and stop.
2. Read ucmd.txt and run the single line it contains as a shell command, then reply RUN-DONE and stop.
3. Use the Task tool to launch one general-purpose subagent whose whole job is to read umarker.txt and report the single word it contains. Then reply SUB-DONE and stop.
Then reply DONE." "Read Bash Task")"
assert_nonempty "session id returned" "$sid"
[ -n "$sid" ] || tb_fatal "no session — $(tail -3 "$TB_STATE/last-session.err" 2>/dev/null)"
tb_note "run_id $sid"

tb_step "the session reached core"
tb_wait_for completed 30 tb_val "select status from sessions where run_id='$sid';"
uuid="$(tb_session_uuid "$sid")"
assert_nonempty "session row created" "$uuid"
assert_eq "session sealed as completed" completed "$(tb_val "select status from sessions where run_id='$sid';")"

# ── A. the pairs exist, and there are exactly as many as there are turns ───────
tb_step "A. llm_completion pairs"
lc_started="$(tb_count "governance_events where run_id='$sid' and activity_type='llm_completion' and event_type='ActivityStarted'")"
lc_completed="$(tb_count "governance_events where run_id='$sid' and activity_type='llm_completion' and event_type='ActivityCompleted'")"
tb_note "llm_completion Started $lc_started · Completed $lc_completed"
assert_ge "turn pairs reached core" 1 "$lc_started"
# Equality, not >=. A Started without its Completed means a turn spent tokens
# nobody can read; a Completed without its Started means the pair split across
# two activity ids, which would double the row count in every dashboard.
assert_eq "every turn Started has its Completed" "$lc_started" "$lc_completed"

# The pairing invariant itself, per activity_id.
lc_orphans="$(tb_val "select count(*) from governance_events c
	where c.run_id='$sid' and c.activity_type='llm_completion' and c.event_type='ActivityCompleted'
	and not exists (select 1 from governance_events s
		where s.run_id=c.run_id and s.activity_type='llm_completion'
		and s.event_type='ActivityStarted' and s.activity_id=c.activity_id);")"
assert_eq "no unpaired turn Completed" 0 "$lc_orphans"

# Exactly one of each per id — the duplicate-ActivityStarted failure mode,
# asserted as a count rather than hoped for.
lc_dupes="$(tb_val "select count(*) from (
		select activity_id, event_type, count(*) n from governance_events
		where run_id='$sid' and activity_type='llm_completion'
		group by activity_id, event_type having count(*) > 1) d;")"
assert_eq "no duplicated turn half" 0 "$lc_dupes"

# ── B. the ids are the shape the contract promises, and the indexes are dense ──
tb_step "B. activity_id shape and index contiguity"
ids="$(tb_sql "select distinct activity_id from governance_events
	where run_id='$sid' and activity_type='llm_completion' order by 1;")"
tb_note "turn ids: $(printf '%s' "$ids" | tr '\n' ' ')"
# A colon-shaped id is new for a dev event — every previous activity_id was
# cc-act-<hex>. This asserts core stored it verbatim rather than rejecting or
# normalising it.
assert_ge "turn ids carry the <session>:turn:<n> shape" 1 \
	"$(tb_count "governance_events where run_id='$sid' and activity_type='llm_completion' and activity_id like '$sid:turn:%'")"
assert_eq "no turn id collided with a tool-call id" 0 \
	"$(tb_count "governance_events where run_id='$sid' and activity_type='llm_completion' and activity_id like 'cc-act-%'")"

# Main-thread indexes must run 0..n-1 with no gaps: a gap means a window was
# read and its events never stored, which is a silently lost turn.
main_idx="$(tb_sql "select distinct (regexp_replace(activity_id, '^.*:turn:', ''))::int
	from governance_events
	where run_id='$sid' and activity_type='llm_completion'
	and activity_id like '$sid:turn:%' order by 1;" | tr '\n' ' ')"
main_n="$(printf '%s' "$main_idx" | wc -w | tr -d '[:space:]')"
expected_idx="$(seq 0 $((main_n - 1)) 2>/dev/null | tr '\n' ' ')"
assert_eq "main-thread turn indexes are contiguous from 0" "$expected_idx" "$main_idx"

# ── C. the payload is what the contract says, and nothing more ─────────────────
tb_step "C. activity_output shape"
# Every Completed half must carry the object; a missing one is a turn whose
# numbers exist locally and nowhere else.
assert_eq "every turn Completed carries activity_output" "$lc_completed" \
	"$(tb_count "governance_events where run_id='$sid' and activity_type='llm_completion' and event_type='ActivityCompleted' and output is not null")"
assert_eq "every turn Completed names its model" "$lc_completed" \
	"$(tb_count "governance_events where run_id='$sid' and activity_type='llm_completion' and event_type='ActivityCompleted' and output->>'model' is not null and output->>'model' <> ''")"
tb_note "models: $(tb_sql "select distinct output->>'model' from governance_events where run_id='$sid' and activity_type='llm_completion' and output is not null;" | tr '\n' ' ')"
# The four counts, present and non-negative. Presence is asserted per-field
# because a consumer that reads only `input_tokens` would not notice the others
# quietly vanishing.
for field in input_tokens output_tokens cache_creation_input_tokens cache_read_input_tokens; do
	assert_eq "usage.$field present on every turn" "$lc_completed" \
		"$(tb_count "governance_events where run_id='$sid' and activity_type='llm_completion' and event_type='ActivityCompleted' and (output->'usage'->>'$field') is not null")"
	assert_eq "usage.$field never negative" 0 \
		"$(tb_count "governance_events where run_id='$sid' and activity_type='llm_completion' and event_type='ActivityCompleted' and (output->'usage'->>'$field')::bigint < 0")"
done
# The Started half carries NO input: a turn's input is the prompt, which is
# content and rides the prompt_submitted signal under the content gate.
assert_eq "turn Started carries no activity_input" 0 \
	"$(tb_count "governance_events where run_id='$sid' and activity_type='llm_completion' and event_type='ActivityStarted' and input is not null")"
# Cost is derived server-side; the client must never have sent one.
assert_eq "no client-derived cost on a turn" 0 \
	"$(tb_count "governance_events where run_id='$sid' and activity_type='llm_completion' and (output ? 'cost' or metadata ? 'cost')")"

# ── D. the reconciliation — the assertion that catches double-counting ─────────
tb_step "D. Σ per-turn == the SessionEnd rollup, field by field"
# Two independent derivations of one quantity: the per-turn windows, and the
# whole-transcript rollup on WorkflowCompleted. They are only comparable because
# contract v1.1 stopped folding the cache counts into `input` — before that they
# summed different things under the same field name.
#
# The rollup covers the WHOLE transcript including sidechain lines, and the turn
# records partition into main + subagent, so the two sides are equal only if
# nothing is counted twice and nothing is dropped.
rollup="$(tb_val "select metadata->'tokens' from governance_events
	where run_id='$sid' and event_type='WorkflowCompleted' and metadata ? 'tokens' limit 1;")"
if [ -z "$rollup" ]; then
	tb_skip "Σ per-turn == rollup" "WorkflowCompleted carried no metadata.tokens"
else
	tb_note "rollup: $rollup"
	# (wire field on the rollup) : (usage field in activity_output)
	for pair in input:input_tokens output:output_tokens \
		cache_creation_input:cache_creation_input_tokens cache_read:cache_read_input_tokens; do
		rf="${pair%%:*}"
		uf="${pair#*:}"
		want="$(tb_json "$rollup" "$rf")"
		got="$(tb_val "select coalesce(sum((output->'usage'->>'$uf')::bigint),0)
			from governance_events where run_id='$sid'
			and activity_type='llm_completion' and event_type='ActivityCompleted';")"
		if [ -z "$want" ]; then
			tb_skip "Σ per-turn $uf == rollup.$rf" "rollup omitted $rf"
		else
			assert_eq "Σ per-turn $uf == rollup.$rf" "$want" "$got"
		fi
	done
fi

# ── E. the subagent — counted once, attributed ─────────────────────────────────
tb_step "E. subagent turns"
sub_rows="$(tb_count "governance_events where run_id='$sid' and activity_type='llm_completion' and activity_id like '$sid:agent:%'")"
if [ "${sub_rows:-0}" -eq 0 ]; then
	# Honest skip rather than a silent pass. Whether SubagentStop's transcript
	# window carries sidechain lines is the one thing static analysis could not
	# settle (see the measurement report); this is the phase that settles it, and
	# "no subagent records" is a real answer that must be visible in the summary.
	tb_skip "subagent turns recorded separately" "no :agent: turn ids — the session may not have spawned one, or SubagentStop's window held no sidechain usage (measurement question 2)"
	tb_note "if the session DID spawn a subagent, this is the signal that SubagentStop reads a transcript whose lines are not marked isSidechain — see plans/260811-1640-coding-agent-token-usage/reports/measure-260811-transcript-turn-surface.md"
else
	tb_note "subagent turn rows: $sub_rows"
	# Subagent ids must be partitioned from the main thread's, or the two would
	# collide on <session>:turn:<n> and core would dedupe one away.
	assert_eq "subagent turn ids are partitioned by agent" 0 \
		"$(tb_val "select count(*) from governance_events
			where run_id='$sid' and activity_type='llm_completion'
			and activity_id like '$sid:agent:%' and activity_id not like '$sid:agent:%:turn:%';")"
	assert_ge "subagent turns are attributed" 1 \
		"$(tb_count "governance_events where run_id='$sid' and activity_type='llm_completion' and metadata->>'agent_id' is not null")"
	sub_orphans="$(tb_val "select count(*) from governance_events c
		where c.run_id='$sid' and c.activity_type='llm_completion' and c.event_type='ActivityCompleted'
		and c.activity_id like '$sid:agent:%'
		and not exists (select 1 from governance_events s
			where s.run_id=c.run_id and s.activity_id=c.activity_id
			and s.event_type='ActivityStarted');")"
	assert_eq "subagent pairs are complete" 0 "$sub_orphans"
fi

# ── F. no content on the usage path ───────────────────────────────────────────
tb_step "F. INV-2 end to end — the narrowed claim"
# This is the only END-TO-END proof that INV-2 still holds after that decision replaced
# the transcript projection's structural impossibility with an allowlist. The unit
# sentinel test is necessary, not sufficient; this is the assertion a privacy
# reviewer should be pointed at.
usage_egress="$(tb_sql "select row_to_json(e)::text from governance_events e
	where run_id='$sid' and activity_type='llm_completion';")"
assert_nonempty "turn rows captured for inspection" "$usage_egress"
assert_absent "shell command text never on a turn row" "$usage_egress" "$SHELL_MARK"
assert_absent "file body never on a turn row" "$usage_egress" "$FILE_MARK"
# The prompt legitimately egresses on the prompt_submitted signal — but NOT here.
# A turn row carrying it would mean the projection started binding message text.
assert_absent "prompt text never on a turn row" "$usage_egress" "$PROMPT_MARK"
# And the raw transcript timestamp form: the projection binds it, parses it to
# compute duration_ms, and must discard the string.
assert_eq "no raw transcript timestamp on a turn row" 0 \
	"$(tb_count "governance_events where run_id='$sid' and activity_type='llm_completion' and output::text like '%timestamp%'")"

# ── G. tool-metric pollution: expected until openbox-core ships the exclusion ──
tb_step "G. tool-metric state (expected pollution, recorded not asserted-away)"
# core's ExtractToolMetric accepts BOTH activity halves with any non-empty
# activity_type (observability/errors.go:301-323), so until the core-side
# exclusion ships, llm_completion appears in the dashboards AS A TOOL, with call
# counts and latency percentiles. That is a known consequence of routing the turn
# through an activity, recorded here with its cause so nobody reads it as a
# shift-left defect.
#
# When core ships the exclusion, flip this from a recorded note to:
#   assert_eq "llm_completion excluded from tool metrics" 0 "$polluted"
polluted="$(tb_val "select count(*) from agent_metrics
	where metric_type='tool' and metric_key like '%llm_completion%';" 2>/dev/null)"
if [ -z "$polluted" ]; then
	tb_skip "tool-metric pollution recorded" "agent_metrics not readable from here"
elif [ "$polluted" -gt 0 ]; then
	tb_note "EXPECTED: $polluted tool-metric row(s) for llm_completion — core has not shipped the ExtractToolMetric exclusion yet (see plans/260811-1640-coding-agent-token-usage/reports/core-issue-activity-usage-extractor.md ask 4)"
	tb_ok "tool-metric pollution present and explained (not a shift-left defect)"
else
	tb_note "no llm_completion tool metrics — core may already exclude it; if so, convert this step to assert_eq 0"
	tb_ok "no tool-metric pollution"
fi

# ── H. the opt-out is real ────────────────────────────────────────────────────
tb_step "H. capture disabled ⇒ silent"
# A security assertion, not a feature test: it proves the documented opt-out is
# complete. A disabled flag that still leaked the model id would be worse than the
# old default, because it would contradict its own documentation.
off_mark="OBXOFF$run"
off_sid="$(OPENBOX_FINOPS=0 tb_session "Task $off_mark. Read README.md, then reply DONE." "Read")"
if [ -z "$off_sid" ]; then
	tb_skip "disabled ⇒ zero llm_completion rows" "the opt-out session did not start"
else
	tb_wait_for completed 30 tb_val "select status from sessions where run_id='$off_sid';"
	tb_note "opt-out run_id $off_sid"
	assert_eq "disabled ⇒ zero llm_completion rows" 0 \
		"$(tb_count "governance_events where run_id='$off_sid' and activity_type='llm_completion'")"
	assert_eq "disabled ⇒ no token rollup either" 0 \
		"$(tb_count "governance_events where run_id='$off_sid' and metadata ? 'tokens'")"
	# The one model mention that legitimately survives is SessionStart's own hook
	# field, which predates this feature and is not part of the usage path.
	assert_eq "disabled ⇒ no model beyond SessionStarted" 0 \
		"$(tb_count "governance_events where run_id='$off_sid' and event_type <> 'WorkflowStarted' and (metadata ? 'model' or (output is not null and output ? 'model'))")"
	# And the session still produced ordinary telemetry: the opt-out silences
	# USAGE, not governance.
	assert_ge "disabled session still captured tool calls" 1 \
		"$(tb_count "governance_events where run_id='$off_sid' and event_type='ActivityStarted'")"
fi

# ── I. the default posture is recorded as evidence ────────────────────────────
tb_step "I. posture evidence"
# What makes a default-on egress defensible after the fact: an auditor can tell
# from the event stream which sessions captured.
assert_eq "SessionStarted records finops on for the default session" true \
	"$(tb_val "select metadata->'posture'->>'finops' from governance_events where run_id='$sid' and event_type='WorkflowStarted' limit 1;")"
if [ -n "${off_sid:-}" ]; then
	assert_eq "SessionStarted records finops off for the opt-out session" false \
		"$(tb_val "select metadata->'posture'->>'finops' from governance_events where run_id='$off_sid' and event_type='WorkflowStarted' limit 1;")"
fi

tb_finish
