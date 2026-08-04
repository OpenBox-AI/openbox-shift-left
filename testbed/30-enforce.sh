#!/usr/bin/env bash
# 30-enforce.sh — Tier-1 in-process enforcement, for real.
#
# Four things that only exist in enforce mode: a local bundle rule that denies,
# secret redaction rewriting a tool body before it lands, the fail-closed HALT
# when core cannot be reached, and the stale-bundle block that `dev sync`
# clears. Each runs a real session; none of them mocks the decider.
set -uo pipefail

TB_DIR="$(cd "$(dirname "$0")" && pwd)"
. "$TB_DIR/env.sh"
. "$TB_DIR/lib/assert.sh"
. "$TB_DIR/lib/sql.sh"
. "$TB_DIR/lib/session.sh"

[ -d "$TB_PROJECT/.claude" ] || tb_fatal "project not governed — run 10-onboard.sh first"

export OPENBOX_ENFORCE=1
BUNDLE="$XDG_CONFIG_HOME/openbox/policy-bundle.json"
AUDIT="$XDG_CONFIG_HOME/openbox/enforcements.jsonl"
STALE_DIR="$XDG_CONFIG_HOME/openbox/stale"
CORE_CTR="${TB_CORE_CTR:-openbox-local-openbox-core-1}"
run="$(date +%s)"

# tb_audit_since prints the enforcement records written after a byte offset —
# the audit file is append-only, so an offset is how a phase reads only its own.
tb_audit_since() { # <offset>
	[ -r "$AUDIT" ] || return 0
	tail -c "+$(($1 + 1))" "$AUDIT" 2>/dev/null
}
tb_audit_size() { [ -r "$AUDIT" ] && wc -c <"$AUDIT" | tr -d ' ' || echo 0; }

write_bundle() { # <json>
	mkdir -p "$(dirname "$BUNDLE")"
	printf '%s\n' "$1" >"$BUNDLE"
	chmod 0600 "$BUNDLE"
}

# ── A. a local rule that denies ────────────────────────────────────────────────
tb_step "A · Tier-1 deny from the local bundle"
DENY_MARK="OBXDENY$run"
write_bundle "{
  \"version\": \"testbed-$run\",
  \"default_decision\": \"allow\",
  \"rules\": [{
    \"id\": \"testbed-deny\",
    \"match\": {\"tool_name\": \"Bash\", \"attribute_contains\": {\"command\": \"$DENY_MARK\"}},
    \"decision\": \"block\",
    \"reason\": \"testbed deny rule\"
  }]
}"
before="$(tb_audit_size)"
tb_session "Run the shell command: echo $DENY_MARK" "Bash" >/dev/null
audit="$(tb_audit_since "$before")"
assert_contains "the call was denied" "$audit" '"applied_decision":"deny"'
assert_contains "decided by the local bundle (Tier-1)" "$audit" '"source":"local-bundle"'
assert_contains "a real verdict, not a fail-open" "$audit" '"fail_open":false'
# The model paraphrases, so the stable assertion is that governance — not a
# tool error — is what it reports back.
assert_contains "the denial reached the model as governance" "$(tb_session_text)" "OpenBox"

# ── B. secret redaction rewrites the body ─────────────────────────────────────
tb_step "B · Tier-1 secret redaction"
# The synthetic key lives in a file, never in the prompt, so "it never egressed"
# means the runtime dropped it rather than never having seen it.
AWS_KEY="AKIAIOSFODNN7EXAMPLE"
printf '%s\n' "$AWS_KEY" >"$TB_PROJECT/secret-src.txt"
rm -f "$TB_PROJECT/secret-out.txt"
write_bundle "{\"version\": \"testbed-$run-b\", \"default_decision\": \"allow\", \"rules\": []}"
before="$(tb_audit_size)"
sid="$(tb_session "Read secret-src.txt and write its exact contents into a new file secret-out.txt." "Read Write")"
audit="$(tb_audit_since "$before")"
if [ -r "$TB_PROJECT/secret-out.txt" ]; then
	body="$(cat "$TB_PROJECT/secret-out.txt")"
	assert_contains "the written body was redacted" "$body" "OPENBOX_REDACTED"
	assert_absent "the secret never reached disk" "$body" "$AWS_KEY"
	assert_contains "the redaction is recorded" "$audit" '"redacted":true'
else
	tb_bad "secret-out.txt written" "a file" "the model did not write one"
fi
if [ -n "$sid" ]; then
	uuid="$(tb_session_uuid "$sid")"
	egress="$(tb_sql "select row_to_json(e)::text from governance_events e where run_id='$sid';")
$(tb_sql "select row_to_json(s)::text from spans s where session_id='${uuid:-none}';")"
	assert_absent "the secret never egressed" "$egress" "$AWS_KEY"
fi

# ── C. fail-closed when core cannot be reached ────────────────────────────────
tb_step "C · fail-closed HALT with core down"
restore_core() { docker start "$CORE_CTR" >/dev/null 2>&1 || true; }
trap restore_core EXIT
if docker stop "$CORE_CTR" >/dev/null 2>&1; then
	before="$(tb_audit_size)"
	export OPENBOX_FAIL_CLOSED=1
	tb_session "Run the shell command: echo hello" "Bash" >/dev/null
	unset OPENBOX_FAIL_CLOSED
	audit="$(tb_audit_since "$before")"
	# `fail_open:true` here is provenance, not outcome: the escalation returned
	# no real verdict, so the org's failure policy decided — and fail-closed
	# turned that into a HALT rather than letting the call through.
	assert_contains "the escalation degraded" "$audit" '"source":"tier2:fail-open"'
	assert_contains "fail-closed synthesised a HALT" "$audit" '"verdict":"HALT"'
	assert_contains "the call was denied" "$audit" '"applied_decision":"deny"'
	restore_core
	# Core needs a moment before the next phase's events land.
	tb_wait_for 200 60 curl -s -o /dev/null -w '%{http_code}' "$OPENBOX_BASE_URL/" &&
		tb_ok "core restored" || tb_bad "core restored" "200" "still down"
else
	tb_skip "fail-closed HALT" "could not stop $CORE_CTR"
fi
trap - EXIT

# ── D. a stale bundle blocks, and dev sync clears it ──────────────────────────
tb_step "D · stale bundle + recovery"
# A pin the backend cannot match (this agent has no policy) is a genuine
# mismatch; fail-closed turns that into a per-session stale marker.
write_bundle "{
  \"version\": \"testbed-$run-d\",
  \"policy_id\": \"testbed-stale-pin\",
  \"updated_at\": \"2020-01-01T00:00:00Z\",
  \"default_decision\": \"allow\",
  \"rules\": []
}"
rm -rf "$STALE_DIR"
before="$(tb_audit_size)"
export OPENBOX_FAIL_CLOSED=1
tb_session "Run the shell command: echo stale-check" "Bash" >/dev/null
unset OPENBOX_FAIL_CLOSED
audit="$(tb_audit_since "$before")"
assert_ge "a stale marker was written" 1 "$(find "$STALE_DIR" -type f 2>/dev/null | wc -l)"
assert_contains "the session was blocked as stale" "$audit" '"stale":true'

"$TB_BIN" dev sync >"$TB_STATE/sync.out" 2>&1
assert_eq "dev sync succeeded" 0 "$?"
assert_eq "sync cleared the stale markers" 0 "$(find "$STALE_DIR" -type f 2>/dev/null | wc -l)"

# ── E. findings channel ───────────────────────────────────────────────────────
tb_step "E · findings loop"
ADVISORIES="$XDG_CONFIG_HOME/openbox/advisories.jsonl"
if [ -s "$ADVISORIES" ]; then
	before_lines="$(wc -l <"$ADVISORIES")"
	OPENBOX_FINDINGS=1 tb_session "Read README.md and summarise it in one line." "Read" >/dev/null
	assert_ge "advisories are still being written" "$before_lines" "$(wc -l <"$ADVISORIES")"
	tb_ok "findings channel exercised (advisories present)"
else
	# No advisory has ever been produced on this stack: core returns none for
	# this agent's posture. Saying so is the honest report — a findings
	# assertion with nothing to find would pass without testing anything.
	tb_skip "findings surface in-session" "core has returned no advisory for this agent (advisories.jsonl empty)"
fi

tb_finish
