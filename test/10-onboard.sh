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
#   * provisioning without a terminal, through the two files the product
#     documents — no secret on argv (INV-1);
#   * `init` with its one flag, run from inside the project. There is no scope to
#     pass, and no lane to opt into.
#
# It also asserts the POSITIVE control: a directory where `init` was never run is
# governed anyway, because one install covers the machine. That is what global
# a governance product should demonstrate its own limits rather than assert them.
set -uo pipefail

TB_DIR="$(cd "$(dirname "$0")" && pwd)"
. "$TB_DIR/env.sh"
. "$TB_DIR/lib/assert.sh"
. "$TB_DIR/lib/sql.sh"

CONFIG="$OPENBOX_HOME/dev.json"
# An install registers user-wide now, so this is where the hooks land. HOOKS is
# kept and still asserted on, because a legacy project-level entry surviving an
# install is exactly the double-registration the sweep exists to prevent.
USER_HOOKS="$HOME/.claude/settings.json"
HOOKS="$TB_PROJECT/.claude/settings.local.json"
# A second project, never initialized. It used to prove sessions there were
# UNgoverned; with user-wide hooks it proves the opposite, which is what global
# scope means.
TB_UNGOVERNED="${TB_UNGOVERNED:-/tmp/openbox-test-ungoverned}"

# hook_count sums a registration across BOTH levels. Neither file alone can show
# the same gate registered twice, which is the failure the sweep prevents.
hook_count() { # <marker>
	local n total=0 f
	for f in "$USER_HOOKS" "$HOOKS"; do
		[ -f "$f" ] || continue
		n="$(grep -c "$1" "$f" 2>/dev/null)"
		total=$((total + ${n:-0}))
	done
	echo "$total"
}

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

tb_step "provision credentials (registers the agent, writes both files)"
[ -n "${OPENBOX_CONTROL_TOKEN:-}" ] || tb_fatal "no control token — run ./test/env.sh mint"

# `openbox auth` takes no flags and requires a terminal, so the harness
# provisions the two files directly. That is not a workaround: it is one of the
# two routes the product documents, and both outrank anything auth writes --
# a real environment variable beats both files at read time.
#
# One stable agent per machine, so repeated runs do not sprawl the org's seats.
# Registration is a single control-plane call rather than a CLI invocation,
# reused from state when this machine already has an agent.
tb_provision() {
	local agent did key
	agent="$(tb_state_get agent_id)"
	if [ -n "$agent" ] && [ -r "$TB_ENV_FILE" ]; then
		tb_note "reusing the agent recorded in $TB_STATE (no new org seat)"
		return 0
	fi
	local body
	body="$(tb_api "/agent" -X POST -H 'content-type: application/json' \
		-d "{\"agent_name\":\"$TB_AGENT_NAME\",\"agent_type\":\"developer\",\"icon\":\"🤖\",\"description\":\"OpenBox testbed agent\"}")"
	case "$(tb_status)" in
	2*) ;;
	*)
		printf '%s\n' "$body" >"$TB_STATE/auth.out"
		return 1
		;;
	esac
	agent="$(tb_json "$body" id)"
	did="$(tb_json "$body" did)"
	key="$(tb_json "$body" api_key)"
	priv="$(tb_json "$body" private_key)"
	[ -n "$agent" ] && [ -n "$key" ] && [ -n "$priv" ] || {
		printf '%s\n' "$body" >"$TB_STATE/auth.out"
		return 1
	}
	# .env holds ONLY secrets and dev.json ONLY coordinates. Mixing them
	# reintroduces the stale-copy bug that reverted a corrected DID on every
	# install, which is why the product keeps two files.
	umask 077
	printf 'OPENBOX_API_KEY=%s\nOPENBOX_AGENT_PRIVATE_KEY=%s\n' "$key" "$priv" >"$TB_ENV_FILE"
	chmod 600 "$TB_ENV_FILE"
	printf '{\n  "agent_id": "%s",\n  "developer_did": "%s",\n  "base_url": "%s",\n  "backend_url": "%s"\n}\n' \
		"$agent" "$did" "$OPENBOX_BASE_URL" "$OPENBOX_BACKEND_URL" >"$OPENBOX_HOME/dev.json"
	tb_state_set agent_id "$agent"
	tb_state_set developer_did "$did"
}

if tb_provision; then
	tb_ok "credentials provisioned"
else
	tb_bad "credentials provisioned" 0 "$(tail -3 "$TB_STATE/auth.out" 2>/dev/null)"
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

tb_step "openbox init (one flag, from inside the project)"
# Run from inside $TB_PROJECT so the sweep of this project's own settings file is
# exercised; the install itself is user-wide either way.
(cd "$TB_PROJECT" && "$TB_BIN" init \
	--provider claude-code) >"$TB_STATE/init.out" 2>&1 ||
	tb_bad "init succeeded" 0 "$(tail -3 "$TB_STATE/init.out")"
tb_ok "init succeeded"

init_out="$(cat "$TB_STATE/init.out")"
assert_contains "init states what it governs" "$init_out" "EVERY SESSION"
assert_contains "init names the file it wrote" "$init_out" "$USER_HOOKS"
assert_contains "init says the change is immediate" "$init_out" "IMMEDIATELY"
# The understatement to avoid now. It used to be over-claiming; one install really
# does cover the machine, so a leftover sentence about one directory, or about an
# administrator having to finish the job, is the wrong direction.
assert_absent "init does not describe per-directory scope" "$init_out" "THIS PROJECT ONLY"
assert_absent "init does not claim activation is pending" "$init_out" "NOTHING YET"

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
# ENFORCE BY DEFAULT, and by ABSENCE. Nothing can write this key any more: a
# bool defaulting to true cannot express "said nothing", so a written `true`
# would be indistinguishable from an explicit opt-in and would revert anyone who
# had opted out. The resolved posture is what doctor reports, asserted below.
assert_eq "no enforce key is written" "" "$(tb_json "$cfg" enforce)"
# And no secret leaked into the coordinate file.
assert_absent "dev.json carries no api key" "$cfg" "obx_"
assert_eq "backend_url persisted" "$OPENBOX_BACKEND_URL" "$(tb_json "$cfg" backend_url)"
# The data-plane URL a self-hosted install must carry: without it the hook and
# doctor's reachability check sign their requests at the SaaS core and get a 401
# that reads as a broken install.
assert_eq "base_url persisted (self-hosted core)" "$OPENBOX_BASE_URL" "$(tb_json "$cfg" base_url)"
assert_eq "git-hook stamping enabled" true "$(tb_json "$cfg" install_git_hook)"

tb_step "the agent it registered"
assert_eq "agent row is a developer agent" developer "$(tb_val "select agent_type from agents where id='$agent_id';")"
assert_eq "AIP signing required on every event" t "$(tb_val "select signing_required from agents where id='$agent_id';")"
assert_eq "agent belongs to the harness org" "$OPENBOX_ORG_ID" "$(tb_val "select organization_id from agents where id='$agent_id';")"

tb_step "hooks, user-wide"
# $USER_HOOKS, not $HOOKS: the install registers here and SWEEPS the project
# file, so a readable project file would be the defect rather than the evidence.
[ -r "$USER_HOOKS" ] || tb_fatal "no $USER_HOOKS — the install merged no hook block"
hooks="$(cat "$USER_HOOKS")"
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
	--provider claude-code) >"$TB_STATE/reinit.out" 2>&1 ||
	tb_bad "re-init succeeded" 0 "$(tail -3 "$TB_STATE/reinit.out")"

hooks="$(cat "$USER_HOOKS" 2>/dev/null)$(cat "$HOOKS" 2>/dev/null)"
assert_eq "PreToolUse registered exactly once across both levels" 1 "$(hook_count 'hook claude-code PreToolUse')"
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
	--provider claude-code) >"$TB_STATE/reinit-dup.out" 2>&1 ||
	tb_bad "re-init succeeded" 0 "$(tail -3 "$TB_STATE/reinit-dup.out")"

assert_eq "Stop registered exactly once again" 1 "$(hook_count 'hook claude-code Stop\"')"
# The gate and the watcher share the PreToolUse key and differ only by invocation;
# collapsing by event would delete the watcher and no approval hold would wake.
assert_eq "the approval watcher is untouched" 1 "$(hook_count 'rewake claude-code')"
assert_contains "re-init names the duplicate it removed" "$(cat "$TB_STATE/reinit-dup.out")" "removed duplicate OpenBox hook registrations"
doctor_out="$(cd "$TB_PROJECT" && "$TB_BIN" doctor 2>&1 || true)"
assert_absent "the warning doctor raised is now clear" "$doctor_out" "more than once"

# The user file is now the activation, so a substring test against it cannot
# work: every hook command embeds the engine path, which contains
# "openbox-observe". What matters is the COUNT of the gate, asserted above and
# again here at the level that governs.
assert_eq "the user file carries exactly one PreToolUse gate" 1 \
	"$(grep -c 'hook claude-code PreToolUse' "$USER_HOOKS")"
assert_eq "and exactly one approval watcher" 1 "$(grep -c 'rewake claude-code' "$USER_HOOKS")"

tb_step "the positive control: a directory where init was never run"
# Inverted with user-wide hooks. This used to demonstrate the accepted cost of
# project scope -- a session here produced nothing, so absence of events proved
# nothing about the work. That cost is gone, and the claim worth proving now is
# the opposite one: a session in a directory this suite never initialized is
# governed, because that is what global scope means.
#
# It is a control rather than an assertion about files: the directory must carry
# no hook config of its own, so anything it produces came from the user file.
[ -e "$TB_UNGOVERNED/.claude/settings.local.json" ] &&
	tb_bad "the control project has no hook config of its own" "absent" \
		"$TB_UNGOVERNED/.claude/settings.local.json exists"
assert_absent "the control project has no .claude dir at all" "$(ls -a "$TB_UNGOVERNED" 2>/dev/null)" ".claude"
tb_ok "no hook config in $TB_UNGOVERNED — anything it produces comes from $USER_HOOKS"
# 20-capture.sh drives a real session in this directory and asserts rows DO
# appear; recording the path in state is what lets it do that without
# re-deriving it. The genuine negative moved to after `openbox uninstall`.
tb_state_set ungoverned_project "$TB_UNGOVERNED"
tb_state_set user_hooks "$USER_HOOKS"

tb_step "doctor"
# The reachability check that `dev verify` used to own is a doctor line now, and
# doctor must report it without ever exiting non-zero: it is what you run when
# something is already wrong.
"$TB_BIN" doctor >"$TB_STATE/doctor.out" 2>&1
assert_eq "doctor exits 0" 0 "$?"
doctor="$(cat "$TB_STATE/doctor.out")"
assert_contains "doctor authenticated to the control plane" "$doctor" "authenticated as"
assert_contains "doctor names the DID it authenticated as" "$doctor" "$did"
assert_contains "doctor reports enforce" "$doctor" "enforce"
assert_contains "doctor reports the resolved ENFORCE posture" "$doctor" "enforce"
assert_contains "doctor reports provenance, not just values" "$doctor" "from "

tb_finish
