#!/bin/zsh
# Publish a verified project security report to the local dashboard, and give
# the demo agent its two runtime checks. Publishing writes a report, never a
# control: rules are applied only by a person clicking Accept in the dashboard.
#
#   publish-report.zsh <security-report-pack>
set -euo pipefail

pack="${1:?usage: publish-report.zsh <security-report-pack>}"
repo_root="$(git -C "${0:A:h}" rev-parse --show-toplevel)"
cli="$repo_root/testbed/.state/project-assurance-demo/bin/openbox"
backend="${OPENBOX_BACKEND_URL:-http://127.0.0.1:3000}"
token="${OPENBOX_CONTROL_TOKEN:-$(cat "${repo_root:h}/local-stack/.state/control-token")}"

"$cli" project verify "$pack"
agent_id="$(jq -er '.agent_id' "$pack/observation/run.json")"

api() {
  curl --fail-with-body --silent --show-error \
    -H "X-API-Key: $token" -H 'Content-Type: application/json' \
    "$@"
}

jq -n --slurpfile report "$pack/report.json" \
      --slurpfile manifest "$pack/manifest.json" \
      --slurpfile run "$pack/observation/run.json" \
      '{report: $report[0], manifest: $manifest[0], run: $run[0]}' |
  api -X POST "$backend/agent/$agent_id/security-reports" --data @- |
  jq -r '"published: \(.data.digest) (\(.data.result))"'

existing="$(api "$backend/agent/$agent_id/evaluation/criteria" | jq -c '[.data[].check_type]')"
seed() {
  jq -e --arg type "$2" 'index($type) == null' <<<"$existing" >/dev/null || return 0
  api -X POST "$backend/agent/$agent_id/evaluation/criteria" --data "$1" |
    jq -r '"criterion: \(.data.name)"'
}
seed '{"name":"sendSupportReport requires approval","check_type":"approval_required","params":{"activity_type":"sendSupportReport"},"standard":"OWASP LLM06"}' approval_required
seed '{"name":"Untrusted input is not acted on","check_type":"untrusted_input_not_acted_on","standard":"OWASP LLM01"}' untrusted_input_not_acted_on

print "open: http://localhost:3233/agents/$agent_id?tab=evaluation&subtab=reports"
