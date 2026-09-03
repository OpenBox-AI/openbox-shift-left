#!/usr/bin/env bash
# test/run-all.sh — run the phases, in order.
#
#   ./test/run-all.sh                      # preflight → … → approver-auto
#   ./test/run-all.sh capture lineage      # preflight, then just those
#   ./test/run-all.sh teardown             # give the box back
#
# Preflight always runs: a green suite against a half-dead stack (OPA bound to
# its own localhost, say) is worse than no suite at all.
set -uo pipefail

TB_DIR="$(cd "$(dirname "$0")" && pwd)"

phases=(
	"onboard:10-onboard.sh"
	"capture:20-capture.sh"
	"realtime:25-realtime.sh"
	"usage:28-usage.sh"
	"enforce:30-enforce.sh"
	# DORMANT: written, never run — no stack has been reachable. See its header.
	"telemetry:35-telemetry.sh"
	"approvals:40-approvals.sh"
	# The capture half of both lanes is proven on the RECORDED corpus without a
	# stack; these two hold the live claims. They no longer install a lane of
	# their own -- one `init` brings both up, so they assert against the shared
	# install, which is what exercises the disjoint activity_id namespaces and
	# the producer election together.
	"otel-lane:46-otel-lane.sh"
	"transport:47-transport.sh"
	"lineage:50-lineage.sh"
	"visibility:60-visibility.sh"
	"auto:70-approver-auto.sh"
)

# PARKED, and named rather than silently dropped. No command installs a gateway
# any more: --gateway went with the rest of init's flags, so 45-gateway.sh has
# no driver, and it still references $OPENBOX_BIN, which nothing under test/
# defines. Leaving it in the phase list would fail every full run.
#
# What has to stay covered is covered elsewhere: `openbox uninstall` unloads a
# gateway unit a machine may still carry, TestAnInstallRetiresARoutedGateway
# proves an install migrates a routed gateway off itself, and
# TestTheDaemonSubcommandsDispatchButAreNotAdvertised proves the daemon still
# dispatches for the units already out there. The file is left untouched
# pending the gateway-lane deletion.
parked=(
	"gateway:45-gateway.sh  no command installs a gateway; pending the gateway-lane deletion"
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

for entry in "${parked[@]}"; do
	printf '\033[33m── %s PARKED: %s\033[0m\n' "${entry%%:*}" "${entry#*  }"
done

run preflight 00-preflight.sh
[ ${#failed[@]} -eq 0 ] || {
	echo
	echo "preflight failed — fix the stack before trusting anything below it." >&2
	exit 1
}

# From here the phases install into the real ~/.claude/settings.json and the
# real supervisor, under fixed launchd labels. So every way out — a normal end,
# a failed phase, Ctrl-C, a TERM from CI — has to give the box back; teardown
# can no longer be opt-in. Armed after preflight on purpose: a host refused up
# there is left exactly as it was found.
#
# Two known limits. With a phase running in the foreground the TERM handler is
# deferred until that phase exits, so a CI cancel that escalates to SIGKILL
# inside its grace period can still leave the install behind — preflight's
# refusal is the backstop. And because the labels and the user-scope settings
# path are fixed, this suite cannot run on a machine that carries a real
# OpenBox install.
teardown_pending=1
teardown() {
	[ -n "${teardown_pending:-}" ] || return 0
	teardown_pending=
	# A second Ctrl-C during teardown aborts it, loudly: a named residue beats an
	# operator who cannot get their terminal back. Preflight refuses the next run.
	trap 'printf "\n\033[31m── teardown interrupted; the test install may still be on this machine. Run: ./test/run-all.sh teardown\033[0m\n" >&2; trap - EXIT; exit 130' INT TERM
	run teardown 99-teardown.sh
	trap - INT TERM
}
on_signal() { # <name> <number>
	printf '\n\033[31m── SIG%s: tearing down. The install and the scratch project go; test/.state and every database row stay.\033[0m\n' "$1" >&2
	teardown
	trap - EXIT
	exit $((128 + $2))
}
# An explicit INT trap is what makes this deterministic. A bare EXIT trap does
# run on signal death, but not when the leaf process catches SIGINT and exits 0
# — there the parent would carry on to the next phase instead of tearing down.
trap teardown EXIT
trap 'on_signal INT 2' INT
trap 'on_signal TERM 15' TERM

for entry in "${phases[@]}"; do
	selected "${entry%%:*}" && run "${entry%%:*}" "${entry#*:}"
done

teardown  # the normal path, before the summary so the report reads in order
trap - EXIT INT TERM

echo
if [ ${#failed[@]} -eq 0 ]; then
	echo "all phases passed"
	exit 0
fi
echo "failed: ${failed[*]}" >&2
exit 1
