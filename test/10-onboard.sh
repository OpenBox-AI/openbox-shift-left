#!/usr/bin/env bash
# 10-onboard.sh — onboard for real, through the product's own two-command front
# door: `openbox auth` registers a developer agent against the local backend and
# writes credentials; `openbox init` governs exactly one scratch project.
#
# This is not a fixture: the same commands a developer runs, against the same
# backend, writing the same files. What the harness changes is only WHERE they
# write — every path is pinned into test/.state by env.sh.
#
# Two things this phase proves that unit tests cannot:
#
#   * `auth` non-interactively via the stdin path — the automation contract, with
#     no secret on argv (INV-1);
#   * `init` at its DEFAULT scope, run from inside the project. The default is new
#, so passing --scope explicitly would test something no user does.
#
# It also asserts the NEGATIVE: a directory where `init` was not run has no hook
# config, so sessions there are ungoverned. That gap is what that decision accepts, and
# a governance product should demonstrate its own limits rather than assert them.
set -uo pipefail

TB_DIR="$(cd "$(dirname "$0")" && pwd)"
. "$TB_DIR/env.sh"
. "$TB_DIR/lib/assert.sh"
. "$TB_DIR/lib/sql.sh"

CONFIG="$OPENBOX_HOME/dev.json"
HOOKS="$TB_PROJECT/.claude/settings.local.json"
# A second project, never initialized, for the negative scope assertion.
TB_UNGOVERNED="${TB_UNGOVERNED:-/tmp/openbox-test-ungoverned}"

tb_step "build the binary under test"
go build -o "$TB_BIN" "$TB_REPO/cmd/openbox" || tb_fatal "go build failed"
tb_ok "built $TB_BIN"

tb_step "a fresh governed project"
# The identity survives a re-run by default: an agent's API key and signing key
# are shown exactly once, so wiping the credential store strands the agent in
# the org. TB_FRESH=1 opts into a clean registration (and a new agent).
rm -rf "$TB_PROJECT" "$TB_UNGOVERNED"
[ "${TB_FRESH:-0}" = "1" ] && rm -rf "$TB_STATE/state" "$OPENBOX_HOME"
mkdir -p "$OPENBOX_HOME"
mkdir -p "$TB_PROJECT"
git -C "$TB_PROJECT" init -q -b main || tb_fatal "git init failed"
git -C "$TB_PROJECT" config user.name "OpenBox Testbed"
git -C "$TB_PROJECT" config user.email "test@openbox.ai"
git -C "$TB_PROJECT" config commit.gpgsign false
printf '# OpenBox test project\n\nA scratch project whose Claude Code sessions are governed.\n' >"$TB_PROJECT/README.md"
git -C "$TB_PROJECT" add README.md && git -C "$TB_PROJECT" commit -qm "chore: init test project"
# The ungoverned twin: a real project, deliberately never initialized.
mkdir -p "$TB_UNGOVERNED"
git -C "$TB_UNGOVERNED" init -q -b main || tb_fatal "git init failed"
git -C "$TB_UNGOVERNED" config user.name "OpenBox Testbed"
git -C "$TB_UNGOVERNED" config user.email "test@openbox.ai"
git -C "$TB_UNGOVERNED" config commit.gpgsign false
printf '# Ungoverned twin\n\nNo `openbox init` was run here. Sessions started here must produce nothing.\n' >"$TB_UNGOVERNED/README.md"
git -C "$TB_UNGOVERNED" add README.md && git -C "$TB_UNGOVERNED" commit -qm "chore: init ungoverned twin"
tb_ok "governed project at $TB_PROJECT; ungoverned twin at $TB_UNGOVERNED"

tb_step "openbox auth (registers the agent, writes credentials)"
[ -n "${OPENBOX_CONTROL_TOKEN:-}" ] || tb_fatal "no control token — run ./test/env.sh mint"

# One stable agent per machine, so repeated runs do not sprawl the org's seats.
#
# --yes rather than a prompt, and the coordinates as FLAGS while the secrets would
# come over stdin: no flag on this command accepts a secret value (INV-1). On the
# register path there is nothing to pipe — the server issues both credentials — so
# stdin stays closed and `auth` short-circuits past the credential prompts.
tb_auth() { # [extra flags…]
	"$TB_BIN" auth \
		--provider claude-code \
		--agent-name "$TB_AGENT_NAME" \
		--org "$OPENBOX_ORG" \
		--backend-url "$OPENBOX_BACKEND_URL" \
		--base-url "$OPENBOX_BASE_URL" \
		--yes "$@" >"$TB_STATE/auth.out" 2>&1
}

if tb_auth; then
	tb_ok "auth succeeded"
elif grep -q "already exists in this org" "$TB_STATE/auth.out"; then
	# The org holds an agent of this name whose one-time keys we no longer have.
	# Registering a distinctly-named one is one of the two recoveries the product
	# offers; `auth --rotate` is the other, and it needs the agent id we lack here.
	tb_note "$TB_AGENT_NAME exists remotely with no local credentials — registering a new one (--force)"
	if tb_auth --force; then tb_ok "auth succeeded (--force)"; else tb_bad "auth succeeded" 0 "$(tail -2 "$TB_STATE/auth.out")"; fi
else
	tb_bad "auth succeeded" 0 "$(tail -2 "$TB_STATE/auth.out")"
fi

tb_step "the credential file it wrote"
[ -r "$TB_ENV_FILE" ] || tb_fatal "no credential file at $TB_ENV_FILE — $(tail -3 "$TB_STATE/auth.out")"
env_body="$(cat "$TB_ENV_FILE")"
assert_contains "credential file is test-scoped, not ~/.openbox" "$TB_ENV_FILE" "$TB_STATE"
assert_contains "api key written under the documented name" "$env_body" "OPENBOX_API_KEY="
assert_contains "signing key written under the documented name" "$env_body" "OPENBOX_AGENT_PRIVATE_KEY="
# that decision's one-store-per-field split: a coordinate in the credential file is the
# two-store bug that made a stale DID revert a corrected one on every install.
assert_absent "no DID in the credential file (secrets and coordinates never share)" "$env_body" "OPENBOX_AGENT_DID="
assert_absent "no agent id in the credential file" "$env_body" "OPENBOX_AGENT_ID="
# The header is a security control: it is where a human learns the file is
# plaintext, is the only copy, and must not be committed.
assert_contains "header states the plaintext posture" "$env_body" "PLAINTEXT"
assert_contains "header says do not commit" "$env_body" "DO NOT COMMIT"
if [ "$(uname)" != "MINGW"* ]; then
	assert_eq "credential file is 0600" 600 "$(stat -f '%Lp' "$TB_ENV_FILE" 2>/dev/null || stat -c '%a' "$TB_ENV_FILE")"
fi
# INV-1 on the command's own output: auth may report WHICH credential it wrote,
# never the value.
auth_out="$(cat "$TB_STATE/auth.out")"
for v in $(python3 -c "
import sys,re
body=open('$TB_ENV_FILE').read()
for line in body.splitlines():
    if line.startswith(('OPENBOX_API_KEY=','OPENBOX_AGENT_PRIVATE_KEY=')):
        print(line.split('=',1)[1].strip().strip(chr(39)))
"); do
	assert_absent "auth output does not echo a credential value" "$auth_out" "$v"
done
assert_contains "auth names init as the next step" "$auth_out" "openbox init"

tb_step "openbox init (DEFAULT scope, from inside the project)"
# No --scope: project scope is the default since that decision, and the default is what
# a user gets. Running from inside $TB_PROJECT is how that default resolves.
(cd "$TB_PROJECT" && "$TB_BIN" init \
	--provider claude-code \
	--install-git-hook) >"$TB_STATE/init.out" 2>&1 ||
	tb_bad "init succeeded" 0 "$(tail -3 "$TB_STATE/init.out")"
tb_ok "init succeeded at default scope"

init_out="$(cat "$TB_STATE/init.out")"
assert_contains "init names the one governed project" "$init_out" "$TB_PROJECT"
assert_contains "init states what is NOT governed" "$init_out" "not governed"
assert_contains "init states the audit consequence" "$init_out" "absence of events is not evidence"
# The overstatement this product exists to avoid: a project-scoped install must
# never read as machine-wide coverage.
assert_absent "init does not claim ambient coverage" "$init_out" "Governance is ambient"

[ -r "$CONFIG" ] || tb_fatal "no dev.json at $CONFIG — $(tail -3 "$TB_STATE/init.out")"

cfg="$(cat "$CONFIG")"
agent_id="$(tb_json "$cfg" agent_id)"
did="$(tb_json "$cfg" developer_did)"
tb_state_set agent_id "$agent_id"
tb_state_set did "$did"

tb_step "the config it wrote"
assert_contains "config is test-scoped, not the real home" "$CONFIG" "$TB_STATE"
assert_nonempty "agent_id persisted" "$agent_id"
assert_nonempty "developer_did persisted" "$did"
# ENFORCE BY DEFAULT : nothing above passed --enforce.
assert_eq "enforce persisted with no flag asking for it" true "$(tb_json "$cfg" enforce)"
# And no secret leaked into the coordinate file.
assert_absent "dev.json carries no api key" "$cfg" "obx_"
assert_eq "backend_url persisted" "$OPENBOX_BACKEND_URL" "$(tb_json "$cfg" backend_url)"
# The data-plane URL a self-hosted install must carry: without it the hook and
# `dev verify` sign their requests at the SaaS core and get a 401 that reads as a
# broken install.
assert_eq "base_url persisted (self-hosted core)" "$OPENBOX_BASE_URL" "$(tb_json "$cfg" base_url)"
assert_eq "git-hook stamping enabled" true "$(tb_json "$cfg" install_git_hook)"

tb_step "the agent it registered"
assert_eq "agent row is a developer agent" developer "$(tb_val "select agent_type from agents where id='$agent_id';")"
assert_eq "AIP signing required on every event" t "$(tb_val "select signing_required from agents where id='$agent_id';")"
assert_eq "agent belongs to the harness org" "$OPENBOX_ORG_ID" "$(tb_val "select organization_id from agents where id='$agent_id';")"

tb_step "hooks, scoped to one project (the default)"
[ -r "$HOOKS" ] || tb_fatal "no $HOOKS — default (project) scope did not merge the hook block"
hooks="$(cat "$HOOKS")"
for ev in SessionStart UserPromptSubmit PreToolUse PostToolUse SessionEnd; do
	assert_contains "$ev wired" "$hooks" "hook claude-code $ev"
done
assert_contains "PreToolUse holds to 30s (E9-S4)" "$hooks" '"timeout": 30'
assert_contains "PreToolUse reads as a reason, not a freeze" "$hooks" "OpenBox governance"
assert_contains "rewake watcher rides alongside the gate" "$hooks" "rewake claude-code"
assert_contains "rewake is async, so it cannot gate" "$hooks" '"asyncRewake": true'

tb_step "a re-init replaces an entry left at another engine path"
# The live failure this pins: a project inited twice, the second time with a
# different HOME, kept BOTH registrations — the merge matched on the exact command
# string, so an entry at another path read as a foreign hook and was preserved.
# Both engines then fired for every hook and every governed tool call was stored
# twice, silently, for the life of the project.
BOGUS_ENGINE="/tmp/openbox-from-another-home/bin/openbox"
# Derive the installed engine path from the file rather than reconstructing it:
# the path is wrapped in ESCAPED quotes in the JSON, so a pattern anchored on the
# quotes has to know how Go encoded them. Matching the path itself does not.
real_engine="$(grep -o '[^"\\]*/bin/openbox' "$HOOKS" | head -1)"
[ -n "$real_engine" ] || tb_fatal "no engine path found in $HOOKS"
sed -i.bak "s#$real_engine#$BOGUS_ENGINE#g" "$HOOKS"
rm -f "$HOOKS.bak"
grep -q "$BOGUS_ENGINE" "$HOOKS" || tb_fatal "could not plant a stale engine path in $HOOKS"

(cd "$TB_PROJECT" && "$TB_BIN" init \
	--provider claude-code \
	--install-git-hook) >"$TB_STATE/reinit.out" 2>&1 ||
	tb_bad "re-init succeeded" 0 "$(tail -3 "$TB_STATE/reinit.out")"

hooks="$(cat "$HOOKS")"
assert_eq "PreToolUse registered exactly once" 1 "$(grep -c 'hook claude-code PreToolUse' "$HOOKS")"
assert_absent "the stale engine path is gone" "$hooks" "$BOGUS_ENGINE"
assert_contains "the rewake watcher survived the replacement" "$hooks" "rewake claude-code"
# Swapping a governing binary without saying so is the same class of problem as
# the silent duplicate it repairs.
assert_contains "re-init names what it replaced" "$(cat "$TB_STATE/reinit.out")" "$BOGUS_ENGINE"
# doctor and init must agree about what "ours" means, or doctor keeps warning
# after the command it recommends.
doctor_out="$(cd "$TB_PROJECT" && "$TB_BIN" doctor 2>&1 || true)"
assert_absent "doctor sees one engine after the repair" "$doctor_out" "OpenBox engines are registered"

tb_step "a re-init collapses one of our hooks registered twice at the same path"
# The other shape doctor warns about, and it names the same remedy for both: our
# own invocation registered twice at ONE engine double-counts exactly as a second
# engine does. A re-init that could not clear it would leave a developer running
# the recommended command forever while the warning stayed.
python3 - "$HOOKS" <<'PY' || tb_fatal "could not plant a duplicate registration"
import json, sys
p = sys.argv[1]
s = json.load(open(p))
s["hooks"]["Stop"].append(json.loads(json.dumps(s["hooks"]["Stop"][0])))
json.dump(s, open(p, "w"), indent=2)
PY
doctor_out="$(cd "$TB_PROJECT" && "$TB_BIN" doctor 2>&1 || true)"
assert_contains "doctor flags the duplicate first" "$doctor_out" "more than once"

(cd "$TB_PROJECT" && "$TB_BIN" init \
	--provider claude-code \
	--install-git-hook) >"$TB_STATE/reinit-dup.out" 2>&1 ||
	tb_bad "re-init succeeded" 0 "$(tail -3 "$TB_STATE/reinit-dup.out")"

assert_eq "Stop registered exactly once again" 1 "$(grep -c 'hook claude-code Stop"' "$HOOKS")"
# The gate and the watcher share the PreToolUse key and differ only by invocation;
# collapsing by event would delete the watcher and no approval hold would wake.
assert_eq "the approval watcher is untouched" 1 "$(grep -c 'rewake claude-code' "$HOOKS")"
assert_contains "re-init names the duplicate it removed" "$(cat "$TB_STATE/reinit-dup.out")" "removed duplicate OpenBox hook registrations"
doctor_out="$(cd "$TB_PROJECT" && "$TB_BIN" doctor 2>&1 || true)"
assert_absent "the warning doctor raised is now clear" "$doctor_out" "more than once"

# The plugin bundle is materialised into ~/.claude/plugins but activation is a
# separate, deliberate step. If it were enabled here, every session on this
# machine would be governed by the test's config — the exact accident
# a global enforce posture causes.
enabled="$(cat "$HOME/.claude/settings.json" 2>/dev/null || echo '{}')"
assert_absent "plugin not globally enabled — scope holds" "$enabled" "openbox-observe"

tb_step "the negative: a directory where init was not run"
# that decision's accepted cost, demonstrated rather than asserted. A session started
# here produces NOTHING — no session row, no events — so on a machine set up this
# way, absence of events is not evidence of absence of work.
[ -e "$TB_UNGOVERNED/.claude/settings.local.json" ] &&
	tb_bad "ungoverned twin has no hook config" "absent" "$TB_UNGOVERNED/.claude/settings.local.json exists"
assert_absent "ungoverned twin has no .claude dir at all" "$(ls -a "$TB_UNGOVERNED" 2>/dev/null)" ".claude"
tb_ok "no hook config in $TB_UNGOVERNED — sessions there are ungoverned"
# 20-capture.sh drives a real session in this directory and asserts zero rows;
# recording the path in state is what lets it do that without re-deriving it.
tb_state_set ungoverned_project "$TB_UNGOVERNED"

tb_step "verify + doctor"
"$TB_BIN" dev verify >"$TB_STATE/verify.out" 2>&1
assert_eq "dev verify succeeded" 0 "$?"
assert_file_contains "verify names the DID it authenticated as" "$TB_STATE/verify.out" "$did"

"$TB_BIN" doctor >"$TB_STATE/doctor.out" 2>&1
doctor="$(cat "$TB_STATE/doctor.out")"
assert_contains "doctor reports enforce" "$doctor" "enforce"
assert_contains "doctor reports require_verified_bundle (G8)" "$doctor" "require_verified_bundle"
assert_contains "doctor reports provenance, not just values" "$doctor" "from "

tb_finish
