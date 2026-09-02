#!/bin/zsh
set -euo pipefail

repo_root="$(git -C "${0:A:h}" rev-parse --show-toplevel)"
local_stack="${repo_root:h}/local-stack"
state_root="$repo_root/testbed/.state/project-assurance-demo"
cli="$state_root/bin/openbox"
token_file="$local_stack/.state/control-token"
credential_file="$HOME/.openbox/.env"
config_file="$HOME/.openbox/dev.json"
skill_dir="${CLAUDE_CONFIG_DIR:-$HOME/.claude}/skills/openbox-security-evaluation"

for required in "$cli" "$token_file" "$credential_file" "$config_file" "$skill_dir/bundle.json"; do
  if [[ ! -e "$required" ]]; then
    print -u2 "demo precondition missing: $required"
    print -u2 "run testbed/project-assurance/mastra-security-demo/prepare-demo.zsh"
    exit 1
  fi
done

run_root="$(mktemp -d "$state_root/run.XXXXXX")"
chmod 700 "$run_root"

export PATH="${cli:h}:$PATH"
export OPENBOX_CONTROL_TOKEN="$(<"$token_file")"
export OPENBOX_BACKEND_URL="http://127.0.0.1:3000"
export OPENBOX_BASE_URL="http://127.0.0.1:8086"
export OPENBOX_DEMO_AGENT_ID="$(jq -er '.agent_id' "$config_file")"
export OPENBOX_DEMO_IMAGE="ai.openbox/mastra-security-demo:local"
export OPENBOX_DEMO_ENV="$repo_root/testbed/project-assurance/mastra-security-demo/evaluation.env"
export OPENBOX_DEMO_OBSERVATION="$run_root/observation"
export OPENBOX_DEMO_CANDIDATE="$run_root/security-analysis.json"
export OPENBOX_DEMO_REPORT="$run_root/security-report"
export OPENBOX_DEMO_GUIDE="$repo_root/testbed/project-assurance/mastra-security-demo/DEMO.md"

cd "$repo_root"
print "OpenBox security demo ready"
print "  project:     $repo_root"
print "  agent:       $OPENBOX_DEMO_AGENT_ID"
print "  image:       $OPENBOX_DEMO_IMAGE"
print "  outputs:     $run_root"
print "  walkthrough: $OPENBOX_DEMO_GUIDE"
print "  credentials: loaded from owner-only local files (values not printed)"
print
exec claude
