#!/usr/bin/env bash
# 50-lineage.sh — commit → session → deploy, produced and then asserted.
#
# The producer half of Mechanism A, end to end and for real: a commit made
# INSIDE a governed session (so the trailer is stamped by the session that
# authored it), its signed attestation note, and two Deploy events emitted by
# the real openbox-git-action against the real core.
#
# The negatives matter as much as the happy path. Before this ran, the local
# stack held exactly two lineage rows from one commit, so every "gap" and
# "amber" state the read side renders was untested against real data.
set -uo pipefail

TB_DIR="$(cd "$(dirname "$0")" && pwd)"
. "$TB_DIR/env.sh"
. "$TB_DIR/lib/assert.sh"
. "$TB_DIR/lib/sql.sh"
. "$TB_DIR/lib/session.sh"

[ -d "$TB_PROJECT/.claude" ] || tb_fatal "project not governed — run 10-onboard.sh first"
AGENT="$(tb_state_get agent_id)"
ACTION="$TB_STATE/openbox-git-action"
REPO_SLUG="${TB_REPO_SLUG:-openboxai/openbox-testbed}"

tb_step "build the deploy action"
go build -o "$ACTION" "$TB_REPO/cmd/openbox-git-action" || tb_fatal "go build failed"
tb_ok "built $ACTION"

# The action authenticates as the developer agent itself — same DID, same key —
# because a deploy that is not this agent's is not this agent's lineage. The
# credentials come out of the harness's own credential file rather than being
# re-minted, so the Deploy event is signed by exactly the identity whose sessions
# it references.
#
# Two names, deliberately: the SECRETS come from ~/.openbox/.env and the DID from
# dev.json, because ADR-0015 keeps them in separate files. Reading the DID from
# beside the secrets is the two-store bug that split removed.
eval "$(python3 - "$TB_ENV_FILE" "$OPENBOX_HOME/dev.json" <<'PY'
import json, shlex, sys

def parse_env(path):
    out = {}
    try:
        for line in open(path):
            line = line.strip().removeprefix("export ")
            if not line or line.startswith("#") or "=" not in line:
                continue
            k, v = line.split("=", 1)
            out[k.strip()] = v.strip().strip("'\"")
    except OSError:
        pass
    return out

env = parse_env(sys.argv[1])
try:
    did = json.load(open(sys.argv[2])).get("developer_did", "")
except Exception:
    did = ""
print(f"export OPENBOX_DID={shlex.quote(did)}")
print(f"export OPENBOX_API_KEY={shlex.quote(env.get('OPENBOX_API_KEY', ''))}")
# Under the name the platform documents; the action still reads the old aliases,
# but the harness exercises the current one.
print(f"export OPENBOX_AGENT_PRIVATE_KEY={shlex.quote(env.get('OPENBOX_AGENT_PRIVATE_KEY', ''))}")
PY
)"
[ -n "${OPENBOX_DID:-}" ] || tb_fatal "no developer DID in $OPENBOX_HOME/dev.json — run 10-onboard.sh"
[ -n "${OPENBOX_API_KEY:-}" ] || tb_fatal "no credentials in $TB_ENV_FILE — run 10-onboard.sh"
export OPENBOX_AGENT_ID="$AGENT"

# ── the authoring session ─────────────────────────────────────────────────────
tb_step "a commit made inside a governed session"
run="$(date +%s)"
printf 'change %s\n' "$run" >>"$TB_PROJECT/README.md"
base="$(git -C "$TB_PROJECT" rev-parse HEAD)"
sid="$(tb_session "Stage README.md and commit it with the message 'feat: testbed change $run'. Use git through the Bash tool." "Bash")"
assert_nonempty "session id returned" "$sid"
sha="$(git -C "$TB_PROJECT" rev-parse HEAD)"
assert_ne "the session made a commit" "$base" "$sha"
[ "$base" != "$sha" ] || tb_fatal "no commit to trace"
tb_state_set commit_sha "$sha"
tb_state_set lineage_run_id "$sid"

tb_step "what the commit carries"
assert_eq "the git hook was installed ambiently" 1 "$([ -x "$TB_PROJECT/.git/hooks/prepare-commit-msg" ] && echo 1 || echo 0)"
trailer="$(git -C "$TB_PROJECT" log -1 --format='%(trailers:key=OpenBox-Session,valueonly)' | tr -d '[:space:]')"
assert_eq "the trailer names the authoring session" "$sid" "$trailer"
notes="$(git -C "$TB_PROJECT" notes --ref=openbox show "$sha" 2>/dev/null)"
assert_contains "the notes mirror carries it too" "$notes" "$sid"
attest="$(git -C "$TB_PROJECT" notes --ref=openbox-attest show "$sha" 2>/dev/null)"
if [ -n "$attest" ]; then
	assert_contains "the attestation is a signed envelope" "$attest" "sig_b64"
	assert_contains "…over canonical bytes" "$attest" "canonical_b64"
	assert_contains "…bound to a DID" "$attest" "did"
	canonical="$(tb_json "$attest" canonical_b64 | base64 -d 2>/dev/null)"
	assert_contains "…naming the commit it attests" "$canonical" "$sha"
	assert_contains "…and the policy bundle in force" "$canonical" "bundle_sha256"
else
	tb_bad "the commit carries a signed attestation" "an envelope in refs/notes/openbox-attest" "no note"
fi

# ── a human commit: the honest gap ────────────────────────────────────────────
tb_step "a commit with no session is a gap, not a guess"
printf 'human change %s\n' "$run" >>"$TB_PROJECT/README.md"
git -C "$TB_PROJECT" commit -aqm "docs: human edit $run"
human_sha="$(git -C "$TB_PROJECT" rev-parse HEAD)"
human_trailer="$(git -C "$TB_PROJECT" log -1 --format='%(trailers:key=OpenBox-Session,valueonly)' | tr -d '[:space:]')"
assert_eq "no trailer is invented for it" "" "$human_trailer"

# ── deploy ────────────────────────────────────────────────────────────────────
deploy() { # <environment> [extra args…]
	"$ACTION" --dir "$TB_PROJECT" --sha "$sha" --base "$base" \
		--repo "$REPO_SLUG" --environment "$1" "${@:2}" >"$TB_STATE/deploy-$1.out" 2>&1
}
links_for() { # <environment>
	tb_count "deploy_session_links where commit_sha='$sha' and deploy_id like 'deploy-$1-%'"
}

tb_step "resolve, then deploy to two environments"
"$ACTION" --dir "$TB_PROJECT" --sha "$sha" --base "$base" --repo "$REPO_SLUG" \
	--environment staging --dry-run >"$TB_STATE/deploy-dry.out" 2>&1
dry="$(cat "$TB_STATE/deploy-dry.out")"
assert_contains "the dry run resolves the session from the trailer" "$dry" "$sid"
assert_contains "…and marks the claim's source" "$dry" "trailer"

deploy staging
assert_eq "staging deploy emitted" 0 "$?"
deploy production
assert_eq "production deploy emitted" 0 "$?"

tb_step "the JOIN core materialised"
tb_wait_for 1 30 links_for staging
assert_eq "one link per environment (staging)" 1 "$(links_for staging)"
tb_wait_for 1 30 links_for production
assert_eq "one link per environment (production)" 1 "$(links_for production)"
row="$(tb_sql "select session_run_id||'|'||coalesce(session_id::text,'NULL')||'|'||verified||'|'||source from deploy_session_links where commit_sha='$sha' and deploy_id like 'deploy-staging-%';")"
assert_contains "the link names the authoring session" "$row" "$sid"
assert_absent "…and resolved it to a real session row" "$row" "|NULL|"
assert_contains "…from the trailer" "$row" "trailer"
tb_note "staging link: $row"

# verified=true needs the local DID verifier (KMS_PROVIDER=local) AND an
# attestation core can check. Report which state this stack produced rather
# than asserting one: both are legitimate, and the read side must render both.
case "$row" in
*"|t|"* | *"|true|"*) tb_ok "the link is attested and verified (green)" ;;
*) tb_note "the link is an unverified claim (amber) — the honest state when core cannot verify the attestation" ;;
esac

tb_step "the negatives"
# Re-deploying the same commit to the same environment must not duplicate:
# run_id is deploy-<env>-<sha>, and core writes ON CONFLICT DO NOTHING.
deploy staging
assert_eq "a re-deploy is idempotent" 1 "$(links_for staging)"

# The human commit has no trailer, so it must produce a deploy with no session
# link at all — a visible gap rather than an invented attribution.
"$ACTION" --dir "$TB_PROJECT" --sha "$human_sha" --base "$sha" \
	--repo "$REPO_SLUG" --environment staging >"$TB_STATE/deploy-human.out" 2>&1
tb_wait_for 1 20 tb_count "governance_events where run_id='deploy-staging-$human_sha'"
assert_eq "the unattributed deploy was still recorded" 1 "$(tb_count "governance_events where run_id='deploy-staging-$human_sha'")"
assert_eq "…with no session link invented for it" 0 "$(tb_count "deploy_session_links where commit_sha='$human_sha'")"
status="$(tb_val "select metadata::jsonb->>'attribution_status' from governance_events where run_id='deploy-staging-$human_sha';")"
tb_note "unattributed deploy attribution_status: $status"

# Fan-in: two sessions authoring one push range must each get their own link
# row — no "primary session" pick.
tb_step "multi-session fan-in"
printf 'second session change %s\n' "$run" >>"$TB_PROJECT/README.md"
sid2="$(tb_session "Stage README.md and commit it with the message 'feat: second testbed change $run'. Use git through the Bash tool." "Bash")"
sha2="$(git -C "$TB_PROJECT" rev-parse HEAD)"
if [ -n "$sid2" ] && [ "$sha2" != "$human_sha" ]; then
	"$ACTION" --dir "$TB_PROJECT" --sha "$sha2" --base "$base" \
		--repo "$REPO_SLUG" --environment staging >"$TB_STATE/deploy-fanin.out" 2>&1
	tb_wait_for 2 30 tb_count "deploy_session_links where deploy_id='deploy-staging-$sha2'"
	assert_ge "one row per authoring session, no primary pick" 2 "$(tb_count "deploy_session_links where deploy_id='deploy-staging-$sha2'")"
	tb_state_set fanin_sha "$sha2"
else
	tb_bad "a second session committed" "a new commit" "none"
fi

tb_finish
