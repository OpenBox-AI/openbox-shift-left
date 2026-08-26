#!/usr/bin/env bash
# 45-gateway.sh — the LOCAL gateway, against a real stack and a real session.
#
# 45 rather than 40: 40-approvals.sh already owns that slot.
#
# Everything before this phase is unit- and conformance-verified only, and this
# repo's own rule is that reading is not evidence. These cases drive real headless
# sessions through a real gateway daemon to a real core and assert what ARRIVED.
#
# ── WHY THE SILENT CASES DOMINATE THIS FILE ───────────────────────────────────
#
# The gateway's dangerous failures do not throw. A stripped `anthropic-beta` makes
# a capability quietly unavailable; a reordered `system` array poisons the prompt
# cache with no error; a failed `/v1/models` discovery falls back silently; a
# buffered relay looks like a slow provider. Every one of those needs an explicit
# assertion, because nothing else will ever complain.
#
# ── WHAT CANNOT RUN YET, AND WHY THAT IS STATED HERE ──────────────────────────
#
# Case D (a policy refusal that does not trigger retry) depends on probe A naming
# a refusal shape. The two constants in gateway/refuse.go are PROVISIONAL. D runs
# and reports, but a failure there is a probe-A finding rather than a regression —
# and if no shape qualifies, phase 06 descopes to observe-only and D is deleted
# rather than fixed. The script says so at the point of failure instead of leaving
# whoever runs it to guess.
set -uo pipefail

TB_DIR="$(cd "$(dirname "$0")" && pwd)"
. "$TB_DIR/env.sh"
. "$TB_DIR/lib/assert.sh"
. "$TB_DIR/lib/sql.sh"
. "$TB_DIR/lib/session.sh"

[ -d "$TB_PROJECT/.claude" ] || tb_fatal "project not governed — run 10-onboard.sh first"

TB_AGENT="${TB_AGENT:-$(tb_state_get agent_id)}"
[ -n "$TB_AGENT" ] || tb_fatal "no agent id in testbed state — run 10-onboard.sh first"

GW_ADDR="${TB_GATEWAY_ADDR:-127.0.0.1:8788}"
GW_HOME="${HOME}"
SETTINGS="$GW_HOME/.claude/settings.json"

# ── setup ─────────────────────────────────────────────────────────────────────
# Install through the REAL command, not by hand-writing files. The install path is
# part of what is under test: its ordering guarantee (never point the tool at a
# gateway that is not up) only means something if this phase exercises it.
tb_step "45.0  install the gateway through \`openbox init --gateway\`"
if ! "$OPENBOX_BIN" init --provider claude-code --gateway --gateway-addr "$GW_ADDR" >"$TB_STATE/gw-init.log" 2>&1; then
	tb_bad "openbox init --gateway failed"
	tb_note "$(tail -5 "$TB_STATE/gw-init.log")"
	tb_finish
	exit 1
fi
tb_ok "gateway installed and started"

# The ordering guarantee, checked on the real machine: the env var exists ONLY
# because the daemon came up. If the daemon is down but the var is set, init broke
# its own invariant and every model call below would fail for the wrong reason.
if ! nc -z "${GW_ADDR%%:*}" "${GW_ADDR##*:}" 2>/dev/null; then
	tb_bad "gateway is not listening at $GW_ADDR after a successful init"
	tb_note "init must not write ANTHROPIC_BASE_URL unless the daemon is proven up"
	tb_finish
	exit 1
fi
grep -q 'ANTHROPIC_BASE_URL' "$SETTINGS" || tb_fatal "init did not write ANTHROPIC_BASE_URL to $SETTINGS"
tb_ok "daemon up AND config written — install ordering held"

run="$(date +%s)"

# ── A. the gateway sees a real model call ─────────────────────────────────────
tb_step "45.A  a real session's model call arrives as a gateway span"
uuid_a="$(tb_session "say exactly: gateway-a-$run")"
[ -n "$uuid_a" ] || tb_fatal "session A produced no run id"

spans="$(tb_count "spans where session_id='$uuid_a' and name like '%llm%'")"
if [ "${spans:-0}" -gt 0 ]; then
	tb_ok "gateway spans stored for the session ($spans)"
else
	tb_bad "no gateway spans for session $uuid_a — the relay ran but nothing reached core"
fi

# Requirement 4: ONE session row receives BOTH producers. This is the join that
# makes the two vantage points one record rather than two half-records.
acts="$(tb_count "governance_events where run_id='$uuid_a' and event_type like 'Activity%'")"
if [ "${spans:-0}" -gt 0 ] && [ "${acts:-0}" -gt 0 ]; then
	tb_ok "session join proven: hook activities ($acts) AND gateway spans ($spans) on one session"
else
	tb_bad "session join NOT proven: activities=$acts spans=$spans on $uuid_a"
fi

# And the two producers must not have collided on activity_id, or core's dedupe
# absorbed one as a duplicate of the other and half the evidence is silently gone.
# ALIAS the subquery, and use the STRICT reader. PostgreSQL requires an alias for
# a subquery in FROM ("subquery in FROM must have an alias"), so this query
# ERRORED — and tb_val discards stderr while the caller coerces "" to 0, which
# made the collision assertion report PASS unconditionally. That is precisely the
# failure mode 45.B's header describes and tb_val_strict was added for, fifteen
# lines further down the same file.
if ! dupes="$(tb_val_strict "select count(*) from (select activity_id from governance_events where run_id='$uuid_a' and activity_id like '%:gateway:%' intersect select activity_id from governance_events where run_id='$uuid_a' and activity_id like '%:turn:%') as shared")"; then
	tb_bad "activity_id collision query FAILED — this is an inconclusive assertion, not a pass"
elif [ "$dupes" = "0" ]; then
	tb_ok "gateway and hook activity ids are disjoint"
else
	tb_bad "activity_id COLLISION between producers ($dupes) — core's dedupe will have dropped evidence"
fi

# ── B. credential never egresses; its fingerprint does ───────────────────────
tb_step "45.B  the raw provider credential is in zero stored bytes; the fingerprint is present"
# STRICT, and the column names matter. The `spans` table has NO request_headers /
# request_body / credential_fingerprint columns — only the JSON blobs `data`,
# `attributes` and `metadata`. An earlier version of this case queried the
# non-existent columns, and because tb_sql discards stderr and callers coerce ""
# to 0, it would have printed a GREEN TICK for "no credential leaked" the first
# time anyone ran it. A broken query must never be indistinguishable from a clean
# result on the one assertion that matters most.
if ! leaked="$(tb_count_strict "spans where session_id='$uuid_a' and (data::text ilike '%sk-ant-%' or data::text ilike '%bearer sk-%' or attributes::text ilike '%sk-ant-%')")"; then
	tb_bad "credential-leak query FAILED — this is an inconclusive assertion, not a pass"
elif [ "$leaked" = "0" ]; then
	tb_ok "no raw credential in stored span data"
else
	tb_bad "RAW CREDENTIAL IN STORED ROWS ($leaked) — treat as an incident, not a bug"
fi

# The fingerprint's route into core is attributes["openbox.credential_fingerprint"]:
# core's SpanData has no credential_fingerprint field at all (verified against
# openbox-core), so the top-level key is dropped on ingest and `attributes` is the
# only copy that survives to be matched on.
if ! fp="$(tb_count_strict "spans where session_id='$uuid_a' and attributes::text like '%openbox.credential_fingerprint%'")"; then
	tb_bad "fingerprint query FAILED — inconclusive"
elif [ "$fp" -gt 0 ]; then
	tb_ok "credential fingerprint present in span attributes ($fp)"
else
	tb_bad "no credential fingerprint in attributes — account binding has nothing to match on"
fi

# ── C. the silent failures ───────────────────────────────────────────────────
# None of these throw. That is exactly why they are asserted.
tb_step "45.C  silent-failure assertions: beta passthrough, system-array identity, discovery"

beta="$(tb_val "select count(*) from spans where session_id='$uuid_a' and data::text ilike '%anthropic-beta%'")"
if [ "${beta:-0}" -gt 0 ]; then
	tb_ok "anthropic-beta survived the relay (a stripped value makes a capability quietly unavailable)"
else
	tb_skip "anthropic-beta survived the relay" "no anthropic-beta on this session's calls — inconclusive, not a pass"
fi

# The system array must still be an ARRAY with the attribution block first. A
# reordered or stringified system block poisons the prompt-cache key with no error.
sysfirst="$(tb_val "select count(*) from spans where session_id='$uuid_a' and data::text like '%\"system\":[%'")"
if [ "${sysfirst:-0}" -gt 0 ]; then
	tb_ok "system block relayed as a positional array"
else
	tb_bad "system block is not an array in stored request bodies — prompt cache and attribution both break silently"
fi

# Discovery: a redirect on /v1/models makes the model picker silently lose the
# gateway's models. Assert the gateway answers directly.
code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 3 "http://$GW_ADDR/v1/models" -H 'anthropic-version: 2023-06-01')"
case "$code" in
30*) tb_bad "/v1/models REDIRECTED ($code) — Claude Code treats any redirect as discovery failure, silently" ;;
"")  tb_bad "/v1/models did not answer within 3s" ;;
*)   tb_ok "/v1/models answered directly ($code)" ;;
esac

# ── D. refusal — PROBE-A DEPENDENT ───────────────────────────────────────────
tb_step "45.D  a policy refusal stops the call and the session does not retry around it"
tb_note "PROVISIONAL: the refusal shape (gateway/refuse.go) is unprobed. A failure here"
tb_note "is a probe-A finding, not necessarily a regression. If no shape qualifies,"
tb_note "phase 06 descopes to observe-only and this case is DELETED, not fixed."
if [ "${TB_GATEWAY_REFUSAL:-0}" != "1" ]; then
	tb_skip "policy refusal stops the call" "set TB_GATEWAY_REFUSAL=1 with a deny policy published to run case D"
else
	before="$(tb_count "governance_events where agent_id='$TB_AGENT'")"
	uuid_d="$(tb_session "say exactly: gateway-d-$run")"
	after="$(tb_count "governance_events where agent_id='$TB_AGENT'")"
	# A retry loop shows up as many near-identical calls for one prompt.
	calls="$(tb_count "spans where session_id='$uuid_d' and name like '%llm%'")"
	if [ "${calls:-0}" -le 2 ]; then
		tb_ok "refused without a retry storm ($calls model call(s) for one prompt)"
	else
		tb_bad "$calls model calls for ONE prompt — the refusal shape reads as transient and is being retried around"
	fi
	tb_note "events before=$before after=$after"
fi

# ── E. bypass visibility — the detection tier's whole point ───────────────────
tb_step "45.E  bypass is visible in stored data, and doctor names it"
"$OPENBOX_BIN" init --provider claude-code --remove-gateway >"$TB_STATE/gw-remove.log" 2>&1 ||
	tb_note "remove-gateway reported: $(tail -2 "$TB_STATE/gw-remove.log")"

if grep -q 'ANTHROPIC_BASE_URL' "$SETTINGS" 2>/dev/null; then
	tb_bad "ANTHROPIC_BASE_URL survived --remove-gateway"
else
	tb_ok "uninstall removed the owned key"
fi

uuid_e="$(tb_session "say exactly: gateway-e-$run")"
turns_e="$(tb_count "governance_events where run_id='$uuid_e' and event_type like 'Activity%'")"
spans_e="$(tb_count "spans where session_id='$uuid_e' and name like '%llm%'")"
if [ "${turns_e:-0}" -gt 0 ] && [ "${spans_e:-0}" = "0" ]; then
	tb_ok "BYPASS IS QUERYABLE: session has activity ($turns_e) and zero gateway spans"
else
	tb_bad "bypass not detectable as designed: activities=$turns_e spans=$spans_e"
fi

if "$OPENBOX_BIN" doctor 2>&1 | grep -qi 'bypass'; then
	tb_ok "doctor reports bypass exposure"
else
	tb_bad "doctor does not mention bypass — the detection tier is silent about itself"
fi

# ── F. fail-closed-by-accident ───────────────────────────────────────────────
# Config present, daemon stopped: model calls must FAIL rather than escape. This
# is the direction that makes a dead gateway safe instead of a hole.
tb_step "45.F  config present + daemon stopped ⇒ zero model calls succeed"
if [ "${TB_GATEWAY_DEADSTOP:-0}" != "1" ]; then
	tb_skip "dead daemon blocks model calls" "set TB_GATEWAY_DEADSTOP=1 to run it (it makes this machine's model calls fail)"
else
	"$OPENBOX_BIN" init --provider claude-code --gateway --gateway-addr "$GW_ADDR" >/dev/null 2>&1
	pkill -f "openbox gateway" 2>/dev/null
	sleep 1
	uuid_f="$(tb_session "say exactly: gateway-f-$run" || true)"
	# A session that never started is the EXPECTED outcome here, and it must not be
	# checked by querying a uuid column for the literal 'none': that is a type
	# error, and the "" it returns is coerced to 0 — the same value as the pass.
	if [ -z "$uuid_f" ]; then
		tb_ok "a dead gateway blocked the session outright (no run id was produced)"
		spans_f=0
	elif ! spans_f="$(tb_count_strict "spans where session_id='$uuid_f' and name like '%llm%'")"; then
		tb_bad "dead-gateway span query FAILED — inconclusive"
		spans_f=-1
	fi
	if [ "${spans_f:-0}" = "0" ]; then
		tb_ok "a dead gateway blocked the calls rather than letting them escape"
	else
		tb_bad "$spans_f model calls succeeded with the gateway dead — the fail direction is OPEN"
	fi
fi

# ── G. Track A independence (requirement 7) ──────────────────────────────────
# Phases 01-02 must hold with NO gateway at all, or Track A's "ships alone"
# property was never true.
tb_step "45.G  Track A holds with no gateway present"
# `output`, not `activity_output`. The latter is the WIRE field name; the COLUMN
# core stores it in is `output`, which is what every other query in this suite
# uses. The mismatch made this query error, tb_val swallow it, and case G report a
# FALSE FAILURE — "Track A capture stopped working" — on a perfectly good run.
# Strict, so a future column rename fails loudly instead of inverting the verdict.
if ! content="$(tb_val_strict "select count(*) from governance_events where run_id='$uuid_e' and output is not null and (output ? 'thinking' or output ? 'output')")"; then
	tb_bad "Track A content query FAILED — inconclusive, not a failure of Track A"
	content=0
fi
if [ "${content:-0}" -gt 0 ]; then
	tb_ok "tool/turn content still captured with the gateway removed ($content)"
else
	tb_bad "Track A capture stopped working without a gateway — the two tracks are coupled"
fi

tb_finish
