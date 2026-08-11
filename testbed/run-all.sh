#!/usr/bin/env bash
# testbed/run-all.sh — run the phases, in order.
#
#   ./testbed/run-all.sh                      # preflight → … → approver-auto
#   ./testbed/run-all.sh capture lineage      # preflight, then just those
#   ./testbed/run-all.sh teardown             # give the box back
#
# Preflight always runs: a green suite against a half-dead stack (OPA bound to
# its own localhost, say) is worse than no suite at all.
set -uo pipefail

TB_DIR="$(cd "$(dirname "$0")" && pwd)"

phases=(
	"onboard:10-onboard.sh"
	"capture:20-capture.sh"
	"realtime:25-realtime.sh"
	"enforce:30-enforce.sh"
	"approvals:40-approvals.sh"
	"lineage:50-lineage.sh"
	"visibility:60-visibility.sh"
	"auto:70-approver-auto.sh"
)

wanted=("$@")
selected() {
	[ ${#wanted[@]} -eq 0 ] && return 0
	local t
	for t in "${wanted[@]}"; do [ "$t" = "$1" ] && return 0; done
	return 1
}

failed=()
run() { # <tag> <script>
	printf '\n\033[1m══ %s ══\033[0m\n' "$1"
	if bash "$TB_DIR/$2"; then
		printf '\033[32m── %s ok\033[0m\n' "$1"
	else
		failed+=("$1")
		printf '\033[31m── %s FAILED\033[0m\n' "$1"
	fi
}

run preflight 00-preflight.sh
[ ${#failed[@]} -eq 0 ] || {
	echo
	echo "preflight failed — fix the stack before trusting anything below it." >&2
	exit 1
}

for entry in "${phases[@]}"; do
	selected "${entry%%:*}" && run "${entry%%:*}" "${entry#*:}"
done

# Teardown is opt-in only: it deactivates the harness credential and removes the
# governed project, so a plain run must leave the box ready for another pass.
if [ ${#wanted[@]} -gt 0 ] && selected teardown; then
	run teardown 99-teardown.sh
fi

echo
if [ ${#failed[@]} -eq 0 ]; then
	echo "all phases passed"
	exit 0
fi
echo "failed: ${failed[*]}" >&2
exit 1
