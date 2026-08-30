#!/usr/bin/env bash
# 46-otel-lane.sh — the :otel: telemetry lane, against a real stack and a real
# desktop session.
#
# 46 rather than 35: 35-telemetry.sh already owns that slot and means something
# else entirely (it asserts the content-capture gate CLOSED). Reusing the number
# would make two unrelated phases collide in run-all.sh.
#
# ── WHAT THIS PHASE IS FOR, AND WHAT IT IS NOT ────────────────────────────────
#
# Phase 13 replays the RECORDED corpus through the shipped chain without a socket
# and asserts the outbound bytes. That covers the decode, the projection, the
# mapping, the election gate and the spool. Everything below is what replay
# cannot reach and only a live stack can answer:
#
#   * that core STORES an :otel: turn as its own row, with the span attached;
#   * that a DESKTOP, subscription-OAuth session produces model-call evidence at
#     all — the reason this lane exists, and the one thing the gateway lane
#     cannot do;
#   * that exactly ONE producer emits when more than one lane is installed.
#
# ── WHY THE SILENT CASES DOMINATE ─────────────────────────────────────────────
#
# This lane's failures are quiet by construction. An election that elects an
# absent producer records nothing while every individual line reads as healthy. A
# lane that emits BESIDE another does not collide — the activity_id namespaces are
# deliberately disjoint — so core stores both and every token count doubles with
# no error anywhere. Neither shows up unless something asserts a COUNT.
#
# DORMANT: written, never run. No stack has been reachable from this branch.
set -uo pipefail

TB_DIR="$(cd "$(dirname "$0")" && pwd)"
. "$TB_DIR/env.sh"
. "$TB_DIR/lib/assert.sh"
. "$TB_DIR/lib/sql.sh"
. "$TB_DIR/lib/session.sh"

[ -d "$TB_PROJECT/.claude" ] || tb_fatal "project not governed — run 10-onboard.sh first"
TB_AGENT="${TB_AGENT:-$(tb_state_get agent_id)}"
[ -n "$TB_AGENT" ] || tb_fatal "no agent id in test state — run 10-onboard.sh first"

OTEL_ADDR="${TB_OTEL_ADDR:-127.0.0.1:8789}"
SETTINGS="${HOME}/.claude/settings.json"

# ── 46.0  install the lane through the REAL command ───────────────────────────
# By command, never by hand-writing settings. The install ordering is part of what
# is under test: the env block is written ONLY after the receiver is proven to be
# listening, and a hand-written settings file would skip the very guarantee this
# phase exists to check.
tb_step "46.0  install the telemetry lane via \`openbox init --telemetry\`"
if ! "$OPENBOX_BIN" init --provider claude-code --telemetry --telemetry-addr "$OTEL_ADDR" \
	>"$TB_STATE/otel-init.log" 2>&1; then
	tb_bad "openbox init --telemetry failed"
	tb_note "$(tail -5 "$TB_STATE/otel-init.log")"
	tb_finish
	exit 1
fi
tb_ok "telemetry lane installed"

# The ordering guarantee on the real machine. If the env block names a receiver
# that is not up, Claude Code's exporter retries into a void and the session looks
# perfectly healthy while nothing is recorded — the exact blindness OD4 names.
if ! nc -z "${OTEL_ADDR%%:*}" "${OTEL_ADDR##*:}" 2>/dev/null; then
	tb_bad "the receiver is not listening at $OTEL_ADDR after a successful init"
	tb_note "init writes the env block only after proving the listener is up; if the"
	tb_note "var is set and the port is dead, that ordering guarantee is broken."
else
	tb_ok "receiver listening at $OTEL_ADDR"
fi

# The 13 env keys are the one thing this repository cannot verify about itself:
# every unit test asserts JSON we wrote, and the CLIENT silently ignores a key it
# does not recognize. This is the only place the real client reads them.
tb_step "46.1  the settings env block names this receiver"
if grep -q "$OTEL_ADDR" "$SETTINGS" 2>/dev/null; then
	tb_ok "settings env points at $OTEL_ADDR"
else
	tb_bad "settings env does not name $OTEL_ADDR"
	tb_note "the key names are copied verbatim from the set that produced the logger's"
	tb_note "corpus; a rename upstream is invisible to every unit test in this repo."
fi

# ── 46.2  a real session, and its turns in governance_events ──────────────────
tb_step "46.2  a real session produces :otel: turns in governance_events"
before=$(tb_count "governance_events where agent_id='$TB_AGENT' and activity_id like '%:otel:%'")
tb_session "Say the single word: telemetry." "none"
sid=$(tb_session_uuid "$(tb_state_get last_run)")
after=$(tb_wait_for "gt:$before" 60 tb_count "governance_events where agent_id='$TB_AGENT' and activity_id like '%:otel:%'")
if [ "${after:-0}" -gt "${before:-0}" ]; then
	tb_ok "stored $((after - before)) :otel: turn(s)"
else
	tb_bad "no :otel: turn reached governance_events"
	tb_note "NEEDS A STACK: the mapper is proven on the recorded corpus without one"
	tb_note "(cmd/openbox/telemetryreplay_test.go). What is unproven without this"
	tb_note "run is that core STORES the row — its dedupe key includes event_type,"
	tb_note "which says it should, but reading core is not evidence."
fi

# ── 46.3  the span survives ingest ────────────────────────────────────────────
# Core RECOMPUTES semantic_type per span and isLLMCall is the only path to
# llm_completion. Deleting the synthesized http_* attributes does not error: the
# span still stores, classifies as something else, and every model-call reader
# goes quiet.
tb_step "46.3  the turn's span classifies as llm_completion"
n=$(tb_count "spans where session_id='$sid' and semantic_type='llm_completion'")
if [ "${n:-0}" -gt 0 ]; then
	tb_ok "$n llm_completion span(s)"
else
	tb_bad "no llm_completion span for session $sid"
	tb_note "NEEDS A STACK. The synthesized http_method/http_url are the only path"
	tb_note "to this classification, and their absence is silent."
fi

# ── 46.4  EXACTLY ONE producer ────────────────────────────────────────────────
# The correctness invariant, and the only case here that cannot be inferred from
# any single lane's own logs.
tb_step "46.4  exactly one model-call producer per turn"
otel=$(tb_count "governance_events where session_id='$sid' and activity_id like '%:otel:%'")
gw=$(tb_count "governance_events where session_id='$sid' and activity_id like '%:gateway:%'")
px=$(tb_count "governance_events where session_id='$sid' and activity_id like '%:proxy:%'")
lanes=0
for c in "$otel" "$gw" "$px"; do [ "${c:-0}" -gt 0 ] && lanes=$((lanes + 1)); done
if [ "$lanes" -le 1 ]; then
	tb_ok "one producer emitted (otel=$otel gateway=$gw proxy=$px)"
else
	tb_bad "$lanes producers emitted for one session (otel=$otel gateway=$gw proxy=$px)"
	tb_note "The namespaces are disjoint on purpose, so core's dedupe CANNOT merge"
	tb_note "these — both rows store and every token count doubles with no error."
	tb_note "Check \`openbox doctor\`: the election is DERIVED from the tool's env"
	tb_note "block, so this means routing and election disagree."
fi

# ── 46.5  OD4: silence on an active session is a FINDING ──────────────────────
tb_step "46.5  an active session with a silent lane is reported, not ignored"
hooks=$(tb_count "governance_events where session_id='$sid'")
if [ "${otel:-0}" -eq 0 ] && [ "${hooks:-0}" -gt 0 ]; then
	tb_bad "the session produced $hooks event(s) and ZERO model-call turns"
	tb_note "OD4: telemetry silence on an otherwise-active session is a finding, not"
	tb_note "an absence. This lane is the governed tool reporting its own calls and"
	tb_note "is suppressible by the thing it observes."
else
	tb_ok "no unexplained silence"
fi

tb_finish
