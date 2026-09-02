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

mkdir -p "$state_root/bin"
chmod 700 "$state_root" "$state_root/bin"

print "== source and image contracts =="
node "$repo_root/testbed/project-assurance/mastra-security-demo/scenario-contract-test.mjs"
docker build --pull=false \
  --file "$repo_root/testbed/project-assurance/mastra-security-demo/Dockerfile" \
  --tag ai.openbox/mastra-security-demo:local \
  "$repo_root/testbed/project-assurance"
OPENBOX_MASTRA_IMAGE=ai.openbox/mastra-security-demo:local \
  node "$repo_root/testbed/project-assurance/mastra-conformance/image-contract-test.mjs"

print "== current CLI =="
(cd "$repo_root/cli" && go build -o "$cli" ./cmd/openbox)
chmod 700 "$cli"
"$cli" version

print "== local services =="
curl --fail --silent --show-error http://127.0.0.1:3000/health >/dev/null
ollama show granite4.1:3b >/dev/null
openshell status -o json | jq -e '
  .status == "connected" and
  .version == "0.0.111" and
  .authentication.status == "authenticated"
' >/dev/null
docker image inspect 'registry:2.8.3@sha256:a3d8aaa63ed8681a604f1dea0aa03f100d5895b6a58ace528858a7b332415373' >/dev/null

print "== local credentials and installed skill =="
[[ -s "$token_file" && "$(stat -f '%Lp' "$token_file")" == "600" ]]
[[ -s "$credential_file" && "$(stat -f '%Lp' "$credential_file")" == "600" ]]
jq -e '
  (.agent_id | type == "string" and test("^[0-9a-f-]{36}$")) and
  .backend_url == "http://127.0.0.1:3000" and
  .base_url == "http://127.0.0.1:8086"
' "$config_file" >/dev/null
jq -e '
  .name == "openbox-security-evaluation" and
  .version == "1.0.0" and
  .digest == "sha256:817e35e1db637d3c9a68ea7b0adf444aa1b5e9c2ad3eaa75c22496506ce0fe13"
' "$skill_dir/bundle.json" >/dev/null

print "demo preflight passed"
print "next: ./testbed/project-assurance/mastra-security-demo/launch-claude.zsh"
