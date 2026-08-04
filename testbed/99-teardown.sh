#!/usr/bin/env bash
# 99-teardown.sh — give the box back.
#
# The database rows stay: they are the fixture the next run's assertions compare
# against, and they are the only real lineage this stack has. What goes away is
# everything that could affect someone else's work — the gating policy, the
# harness credential, the scratch project, and any request left undecided (which
# would otherwise keep a rewake watcher alive for 45 minutes).
set -uo pipefail

TB_DIR="$(cd "$(dirname "$0")" && pwd)"
. "$TB_DIR/env.sh"
. "$TB_DIR/lib/assert.sh"
. "$TB_DIR/lib/sql.sh"

AGENT="$(tb_state_get agent_id)"

tb_step "release anything still pending"
pending="$(tb_val "select count(*) from governance_events where approval_expired_at is not null and decided_at is null and agent_id='${AGENT:-none}';")"
if [ "${pending:-0}" -gt 0 ] && [ -n "${OPENBOX_CONTROL_TOKEN:-}" ]; then
	for id in $(tb_sql "select id from governance_events where approval_expired_at is not null and decided_at is null and agent_id='$AGENT';"); do
		"$TB_BIN" approve deny "$id" --org "$OPENBOX_ORG_ID" >/dev/null 2>&1
	done
fi
assert_eq "no request is left undecided" 0 "$(tb_val "select count(*) from governance_events where approval_expired_at is not null and decided_at is null and agent_id='${AGENT:-none}';")"
rm -f "$XDG_CONFIG_HOME/openbox/pending-approvals/"*.json 2>/dev/null
# A watcher from the last session may still be inside its grace window; that is
# not a leak, so give it a moment before calling it one.
#
# Matched by process NAME, not command line: `pgrep -f` would also match this
# script's own shell, whose command line contains the pattern — it reported a
# phantom watcher until it was matched this way.
# pgrep -c prints 0 AND exits non-zero when nothing matches, so the count is
# taken from stdout alone — an `|| echo 0` fallback would print it twice.
watchers() {
	local n
	n="$(pgrep -xc openbox 2>/dev/null)"
	echo "${n:-0}"
}
for _ in $(seq 1 30); do
	[ "$(watchers)" = "0" ] && break
	sleep 1
done
assert_eq "no openbox process is left running" 0 "$(watchers)"

tb_step "deactivate the testbed policy"
if [ -n "$AGENT" ]; then
	docker exec -i "$TB_PG" psql -U postgres -d "$TB_PG_DB" -q -c \
		"update policies set is_active=false, is_current_version=false where agent_id='$AGENT';" >/dev/null 2>&1
	assert_eq "no current policy remains for the testbed agent" 0 "$(tb_count "policies where agent_id='$AGENT' and is_current_version=true")"
	tb_note "OPA keeps serving the last compiled bundle until the backend rebuilds it; nothing else on this stack uses this agent"
else
	tb_skip "deactivate the testbed policy" "no agent id in state"
fi

tb_step "remove the scratch project"
rm -rf "$TB_PROJECT"
assert_eq "the governed project is gone" 0 "$([ -d "$TB_PROJECT" ] && echo 1 || echo 0)"

tb_step "deactivate the harness credential"
tb_drop_key
assert_eq "the key is inactive" 0 "$(tb_count "api_keys where name='$TB_KEY_NAME' and is_active=true")"

tb_note "kept: $TB_STATE (config, audit trail, session output) and every database row"
tb_finish
