#!/bin/zsh
# Build the intentionally vulnerable demo image, check the local services it
# needs, then run it through the OpenBox sandbox and ask the backend to evaluate
# the run. The evaluation, the report and the suggested rule appear in the
# dashboard; this script prints the summary.
set -euo pipefail

demo_dir="${0:A:h}"
repo_root="$(git -C "$demo_dir" rev-parse --show-toplevel)"
local_stack="${repo_root:h}/local-stack"
state_root="$repo_root/testbed/.state/project-assurance-demo"
cli="$state_root/bin/openbox"
token_file="$local_stack/.state/control-token"
config_file="$HOME/.openbox/dev.json"

mkdir -p "$state_root/bin"
chmod 700 "$state_root" "$state_root/bin"

print "== project contract and image =="
node "$demo_dir/scenario-contract-test.mjs"
docker build --pull=false --file "$demo_dir/Dockerfile" \
  --tag ai.openbox/mastra-security-demo:local "$demo_dir"

print "== current CLI =="
(cd "$repo_root/cli" && go build -o "$cli" ./cmd/openbox)
chmod 700 "$cli"
"$cli" version

print "== local services =="
curl --fail --silent --show-error http://127.0.0.1:3000/health >/dev/null
openshell status -o json | jq -e '.status == "connected" and .authentication.status == "authenticated"' >/dev/null
# The sandbox reaches its model only through the gateway's inference route.
inference_route="$(openshell inference get)"
[[ "$inference_route" == *granite4.1:3b* ]]
docker image inspect 'registry:2.8.3@sha256:a3d8aaa63ed8681a604f1dea0aa03f100d5895b6a58ace528858a7b332415373' >/dev/null

print "== credentials =="
[[ -s "$token_file" && "$(stat -f '%Lp' "$token_file")" == "600" ]]
agent_id="$(jq -er '.agent_id | select(test("^[0-9a-f-]{36}$"))' "$config_file")"

print "== run, then evaluate =="
OPENBOX_CONTROL_TOKEN="$(cat "$token_file")" "$cli" project evaluate \
  --image ai.openbox/mastra-security-demo:local \
  --env-file "$demo_dir/evaluation.env" \
  --openbox-agent "$agent_id" \
  --wait
