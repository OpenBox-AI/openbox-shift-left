#!/usr/bin/env bash
# apply.sh — apply the EXT-core dev-event-type dependency to an openbox-core checkout.
#
# The one external dependency shift-left needs: openbox-core must accept-list the
# 7 developer-runtime event types (else emitted events fail-open drop on a
# 400 "invalid event_type"). See README.md for scope/rationale (arch D4/INV-8).
#
# Usage:
#   ./apply.sh /path/to/openbox-core          # apply the patch
#   ./apply.sh --check /path/to/openbox-core  # dry-run: report whether it would apply cleanly
#
# The patch is strictly additive (3 files, no migration). It is idempotent-safe:
# --check first tells you if it is already applied.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PATCH="$HERE/openbox-core-dev-event-types.patch"

CHECK=0
if [[ "${1:-}" == "--check" ]]; then
  CHECK=1
  shift
fi

CORE="${1:-}"
if [[ -z "$CORE" ]]; then
  echo "usage: $0 [--check] /path/to/openbox-core" >&2
  exit 2
fi
if [[ ! -f "$CORE/internal/content/governance.go" ]]; then
  echo "error: $CORE does not look like an openbox-core checkout (internal/content/governance.go missing)" >&2
  exit 2
fi

cd "$CORE"

if git apply --reverse --check "$PATCH" >/dev/null 2>&1; then
  echo "✓ already applied: the 7 developer-runtime event types are present in $CORE"
  exit 0
fi

if ! git apply --check "$PATCH" >/dev/null 2>&1; then
  echo "✗ patch does not apply cleanly to $CORE — the accept-list mechanism may have moved." >&2
  echo "  Regenerate the artifact against the current core (STORY-SL-13 stop condition) or apply the 3 edits by hand (see README.md)." >&2
  exit 1
fi

if [[ "$CHECK" == "1" ]]; then
  echo "✓ would apply cleanly (dry-run). Re-run without --check to apply."
  exit 0
fi

git apply "$PATCH"
echo "✓ applied openbox-core-dev-event-types.patch to $CORE"
echo "  Next: rebuild core, then run the acceptance test (contracts/dev-event/acceptance) with OPENBOX_URL + dev creds to confirm all 7 types return non-400."
