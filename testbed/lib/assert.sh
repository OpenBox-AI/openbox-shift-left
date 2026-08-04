#!/usr/bin/env bash
# testbed/lib/assert.sh — assertions, and the two shared helpers every phase
# needs (JSON extraction, an authenticated backend call).
#
# Phase scripts do NOT use `set -e`: a failed assertion has to be recorded and
# the run continued, or the first regression hides every later one. Genuine
# prerequisites (a build, a required id) use tb_fatal instead.

TB_PASS=0
TB_FAIL=0
TB_SKIP=0

tb_step() { printf '\n\033[1m%s\033[0m\n' "$*"; }
tb_note() { printf '    %s\n' "$*"; }

tb_ok() {
	TB_PASS=$((TB_PASS + 1))
	printf '  ✓ %s\n' "$1"
}

tb_bad() {
	TB_FAIL=$((TB_FAIL + 1))
	printf '  ✗ %s\n      want: %s\n      got : %s\n' "$1" "$2" "$3"
}

# tb_skip records a check that could not run — visible in the summary, so a
# skipped assertion never reads as a passing one.
tb_skip() {
	TB_SKIP=$((TB_SKIP + 1))
	printf '  – %s  (skipped: %s)\n' "$1" "$2"
}

# tb_fatal aborts the phase: a precondition, not an assertion.
tb_fatal() {
	printf '\n  !! %s\n' "$*" >&2
	TB_FAIL=$((TB_FAIL + 1))
	tb_finish
}

assert_eq() { # <label> <want> <got>
	if [ "$2" = "$3" ]; then tb_ok "$1"; else tb_bad "$1" "$2" "$3"; fi
}

assert_ne() { # <label> <unwanted> <got>
	if [ "$2" != "$3" ]; then tb_ok "$1"; else tb_bad "$1" "not $2" "$3"; fi
}

assert_ge() { # <label> <min> <got>
	if [ "${3:-0}" -ge "$2" ] 2>/dev/null; then tb_ok "$1"; else tb_bad "$1" ">= $2" "${3:-<empty>}"; fi
}

assert_nonempty() { # <label> <got>
	if [ -n "${2:-}" ]; then tb_ok "$1"; else tb_bad "$1" "non-empty" "<empty>"; fi
}

assert_contains() { # <label> <haystack> <needle>
	case "$2" in *"$3"*) tb_ok "$1" ;; *) tb_bad "$1" "contains $3" "${2:0:200}" ;; esac
}

# assert_absent is the privacy workhorse (INV-2): the needle must appear
# NOWHERE in the haystack.
assert_absent() { # <label> <haystack> <needle>
	case "$2" in *"$3"*) tb_bad "$1" "no occurrence of $3" "found it" ;; *) tb_ok "$1" ;; esac
}

assert_file_contains() { # <label> <path> <needle>
	if [ -r "$2" ] && grep -qF -- "$3" "$2"; then tb_ok "$1"; else tb_bad "$1" "$2 contains $3" "$([ -r "$2" ] && echo "no match" || echo "unreadable")"; fi
}

# tb_json extracts a dotted path from a JSON document; objects and arrays come
# back re-serialised, a missing path comes back empty.
tb_json() { # <json> <dotted.path>
	python3 - "$1" "$2" <<'PY'
import json, sys
try:
    doc = json.loads(sys.argv[1] or "{}")
except Exception:
    print(""); raise SystemExit
cur = doc
for key in [k for k in sys.argv[2].split(".") if k]:
    if isinstance(cur, list):
        try:
            cur = cur[int(key)]
        except (ValueError, IndexError):
            cur = None
    elif isinstance(cur, dict):
        cur = cur.get(key)
    else:
        cur = None
    if cur is None:
        break
# Booleans print as JSON (true/false), not as Python (True/False): the
# assertions compare against what the file says.
print("" if cur is None else (cur if isinstance(cur, str) else json.dumps(cur)))
PY
}

# tb_api calls the backend under the harness credential and prints the body.
# Asserting through the read API rather than SQL is the point: it tests the
# surface a user actually has (plan §3 P6).
#
# The status goes to a file rather than a variable because the natural way to
# call this is `body="$(tb_api …)"` — a subshell, where an assignment would be
# lost. tb_status reads it back.
tb_api() { # <path> [extra curl args…]
	local path="$1"
	shift
	local out
	if ! out="$(curl -sS -w $'\n%{http_code}' \
		-H "X-API-Key: ${OPENBOX_CONTROL_TOKEN:-}" \
		"$OPENBOX_BACKEND_URL$path" "$@" 2>/dev/null)"; then
		printf '000' >"$TB_STATE/http-status"
		return 0
	fi
	printf '%s' "${out##*$'\n'}" >"$TB_STATE/http-status"
	printf '%s' "${out%$'\n'*}"
}

# tb_status is the HTTP status of the last tb_api call.
tb_status() { cat "$TB_STATE/http-status" 2>/dev/null; }

tb_finish() {
	printf '\n  %d passed, %d failed, %d skipped\n' "$TB_PASS" "$TB_FAIL" "$TB_SKIP"
	[ "$TB_FAIL" -eq 0 ] || exit 1
	exit 0
}
