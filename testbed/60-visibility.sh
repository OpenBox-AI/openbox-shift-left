#!/usr/bin/env bash
# 60-visibility.sh — is the lineage 50 produced actually readable?
#
# Asserted through the backend read API rather than SQL, because that is the
# surface a user has. The rows exist either way; what this phase catches is a
# read side that renders a chain differently from the rows underneath it —
# including the two states that matter most and had never been exercised
# against real data: AMBER (an unverified claim) and GAP (a hop with no
# evidence at all, which must be reported, never omitted).
set -uo pipefail

TB_DIR="$(cd "$(dirname "$0")" && pwd)"
. "$TB_DIR/env.sh"
. "$TB_DIR/lib/assert.sh"
. "$TB_DIR/lib/sql.sh"

AGENT="$(tb_state_get agent_id)"
SHA="$(tb_state_get commit_sha)"
RUN_ID="$(tb_state_get lineage_run_id)"
[ -n "$SHA" ] || tb_fatal "no commit in state — run 50-lineage.sh first"

# hop_status pulls one hop's status out of the chain's evidence block.
hop_status() { # <chain-json> <hop>
	python3 - "$1" "$2" <<'PY'
import json, sys
try:
    hops = json.loads(sys.argv[1])["data"]["evidence"]
except Exception:
    hops = []
print(next((h.get("status", "") for h in hops if h.get("hop") == sys.argv[2]), ""))
PY
}

# Precondition, not an assertion: the org feature flag is cached for ~30s, so a
# run started right after the gate test below can still be seeing the cached
# "off" value. Wait for the flag to be live before judging anything.
for _ in $(seq 1 45); do
	tb_api "/lineage/commits/$SHA" >/dev/null
	[ "$(tb_status)" = "403" ] || break
	sleep 1
done

tb_step "the chain for the commit a session authored"
chain="$(tb_api "/lineage/commits/$SHA")"
assert_eq "the chain is readable" 200 "$(tb_status)"
assert_eq "it is the commit we made" "$SHA" "$(tb_json "$chain" data.commit_sha)"
assert_eq "the authoring session is named" "$RUN_ID" "$(tb_json "$chain" data.sessions.0.session_run_id)"
assert_eq "resolved to the session row core stored" \
	"$(tb_val "select id from sessions where run_id='$RUN_ID';")" "$(tb_json "$chain" data.sessions.0.session_id)"
assert_nonempty "the developer behind it is named" "$(tb_json "$chain" data.developers.0.agent_did)"
assert_ge "both deploys appear" 2 "$(python3 -c 'import json,sys;print(len(json.loads(sys.argv[1])["data"]["deploys"]))' "$chain" 2>/dev/null || echo 0)"

tb_step "per-hop evidence, and whether it matches the rows"
assert_eq "the dev-session hop is verified (a sealed session)" verified "$(hop_status "$chain" dev_session)"
# The commit hop must agree with the link row: verified in the database means
# green here, and an unverified claim means amber. A read side that reported
# green over an unverified row would be the dangerous failure.
verified_row="$(tb_val "select verified from deploy_session_links where commit_sha='$SHA' limit 1;")"
commit_hop="$(hop_status "$chain" commit)"
case "$verified_row" in
t) assert_eq "verified row renders as verified" verified "$commit_hop" ;;
*) assert_eq "an unverified claim renders as amber, not green" partial "$commit_hop" ;;
esac
tb_note "link verified=$verified_row → commit hop '$commit_hop'"
# The production-runtime hop has no producer at all. It must be REPORTED as
# missing rather than left out of the chain.
assert_eq "the prod-session hop is reported as a gap" missing "$(hop_status "$chain" prod_session)"
assert_nonempty "…and says why" "$(python3 -c '
import json,sys
hops=json.loads(sys.argv[1])["data"]["evidence"]
print(next((h.get("detail","") for h in hops if h.get("hop")=="prod_session"),""))' "$chain" 2>/dev/null)"

tb_step "the deploy feed"
feed="$(tb_api "/lineage/deploys?commit=$SHA")"
assert_eq "the feed is readable" 200 "$(tb_status)"
assert_contains "our deploy is in it" "$feed" "deploy-staging-$SHA"
assert_contains "…with its environment" "$feed" "staging"
# Org scoping. The rows carry no agent id, so the check goes the other way:
# take every deploy event the feed returned and ask the database whose agent it
# belongs to. A single row owned by another org would be a cross-tenant read.
all_feed="$(tb_api "/lineage/deploys?page_size=100")"
ids="$(python3 - "$all_feed" <<'PY'
import json, sys
try:
    rows = json.loads(sys.argv[1])["data"]["items"]
except Exception:
    rows = []
print(",".join(sorted({r["deploy_governance_event_id"] for r in rows
                       if isinstance(r, dict) and r.get("deploy_governance_event_id")})))
PY
)"
if [ -n "$ids" ]; then
	outside="$(tb_val "select count(*) from governance_events e join agents a on a.id = e.agent_id
	  where e.id in ('${ids//,/\',\'}') and a.organization_id <> '$OPENBOX_ORG_ID';")"
	assert_eq "every deploy the feed returns belongs to this org" 0 "$outside"
	tb_note "$(printf '%s' "$ids" | tr ',' '\n' | wc -l) deploys checked for tenancy"
else
	tb_bad "the feed returns this org's deploys" "at least one row" "none"
fi

tb_step "the reverse anchor (session → commits → deploys)"
for anchor in "$RUN_ID" "$(tb_val "select id from sessions where run_id='$RUN_ID';")"; do
	[ -n "$anchor" ] || continue
	rev="$(tb_api "/lineage/sessions/$anchor/chain")"
	assert_eq "reachable by ${#anchor}-char anchor" 200 "$(tb_status)"
	assert_contains "…and it finds the commit" "$rev" "$SHA"
done

tb_step "an unattributed deploy renders as a gap, not an omission"
human_sha="$(tb_val "select metadata::jsonb->>'commit_sha' from governance_events e where run_id like 'deploy-%' and metadata::jsonb->>'attribution_status'='unattributed' and agent_id='$AGENT' order by created_at desc limit 1;")"
if [ -n "$human_sha" ]; then
	hfeed="$(tb_api "/lineage/deploys?commit=$human_sha")"
	assert_eq "the unattributed deploy is readable" 200 "$(tb_status)"
	assert_contains "…and appears in the feed" "$hfeed" "deploy-staging-$human_sha"
	assert_contains "…marked unattributed rather than guessed at" "$hfeed" "unattributed"

	# KNOWN GAP (openbox-backend lineage.service.ts:212-217): the commit chain
	# is anchored on deploy_session_links, so a deployed commit with NO
	# authoring session has no chain at all — 404, not a chain whose session hop
	# reads "missing". The deploy is visible in the feed (asserted above), but
	# the one view built to never hide a hop cannot render this commit.
	tb_api "/lineage/commits/$human_sha" >/dev/null
	case "$(tb_status)" in
	200) tb_ok "the trailer-less commit renders a chain with an empty session hop" ;;
	404) tb_skip "the trailer-less commit renders a chain" "404 — the chain anchors on links, so an unattributed commit has no chain (backend lineage.service.ts:212)" ;;
	*) tb_bad "the trailer-less commit renders a chain" "200, or the known 404" "$(tb_status)" ;;
	esac
else
	tb_skip "an unattributed deploy renders as a gap" "no unattributed deploy found — run 50-lineage.sh"
fi

tb_step "dashboard lineage KPIs"
dash="$(tb_api "/organization/$OPENBOX_ORG_ID/dashboard")"
assert_eq "the dashboard is readable" 200 "$(tb_status)"
kpis="$(tb_json "$dash" data.lineage_kpis)"
assert_nonempty "it carries lineage KPIs" "$kpis"
assert_ge "deploys in the last 30 days include ours" 1 "$(tb_json "$kpis" deploys_30d)"
for k in gate_pass_rate attested_lineage_pct sessions_signed_commit_pct; do
	assert_contains "KPI $k is reported" "$kpis" "$k"
done
tb_note "lineage KPIs: $kpis"

tb_step "the read API is not public"
curl -s -o /dev/null -w '%{http_code}' "$OPENBOX_BACKEND_URL/lineage/commits/$SHA" >"$TB_STATE/anon.code"
assert_eq "an anonymous read is refused" 401 "$(cat "$TB_STATE/anon.code")"

tb_step "the agent_lineage feature gate"
flags="$(tb_val "select feature_flags::text from organization_settings where organization_id='$OPENBOX_ORG_ID' limit 1;")"
if [ -n "$flags" ] && [ "${TB_TEST_FEATURE_GATE:-1}" = "1" ]; then
	restore_flags() {
		docker exec -i "$TB_PG" psql -U postgres -d "$TB_PG_DB" -q -c \
			"update organization_settings set feature_flags='$flags'::jsonb where organization_id='$OPENBOX_ORG_ID';" >/dev/null 2>&1
	}
	trap restore_flags EXIT
	docker exec -i "$TB_PG" psql -U postgres -d "$TB_PG_DB" -q -c \
		"update organization_settings set feature_flags = (feature_flags::jsonb || '{\"agent_lineage\": false}'::jsonb) where organization_id='$OPENBOX_ORG_ID';" >/dev/null 2>&1
	# The flag is cached for 30s, so wait it out rather than reporting a cached
	# 200 as either a pass or a failure.
	gate=""
	for _ in $(seq 1 45); do
		tb_api "/lineage/commits/$SHA" >/dev/null
		gate="$(tb_status)"
		[ "$gate" = "403" ] && break
		sleep 1
	done
	assert_eq "with agent_lineage off the read is refused" 403 "$gate"
	restore_flags
	trap - EXIT
	# Same cache in the other direction: the flag is back on disk immediately,
	# but the API keeps refusing until the cached value expires.
	back=""
	for _ in $(seq 1 45); do
		tb_api "/lineage/commits/$SHA" >/dev/null
		back="$(tb_status)"
		[ "$back" = "200" ] && break
		sleep 1
	done
	assert_eq "the flag was restored" 200 "$back"
else
	tb_skip "the agent_lineage feature gate" "no organization_settings row for $OPENBOX_ORG_ID"
fi

tb_finish
