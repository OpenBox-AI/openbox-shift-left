#!/usr/bin/env bash
# verify-tree.sh: what a fresh clone must be true of, checked in one place.
#
# Every row is a claim this repository makes about its own tree. Run it before
# publishing and on any change that touches prose, layout or dependencies. It
# has no side effects and needs no credentials, no network and no stack.
#
# Exit 0 when every row passes. A failing row prints what it measured, because
# a check whose failure you cannot read gets weakened rather than fixed.
set -uo pipefail
cd "$(dirname "$0")/.."

pass=0
fail=0

row() { # row <name> <expected> <actual>
	if [ "$2" = "$3" ]; then
		printf '  ok    %-46s %s\n' "$1" "$3"
		pass=$((pass + 1))
	else
		printf '  FAIL  %-46s want %s, got %s\n' "$1" "$2" "$3"
		fail=$((fail + 1))
	fi
}

cmd() { # cmd <name> <command...>
	local name=$1
	shift
	if "$@" >/dev/null 2>&1; then
		printf '  ok    %s\n' "$name"
		pass=$((pass + 1))
	else
		printf '  FAIL  %s\n' "$name"
		fail=$((fail + 1))
	fi
}

echo "Build and portability"
cmd "go build ./..." go build ./...
cmd "go vet ./..." go vet ./...
GOOS=windows GOARCH=amd64 go build ./... >/dev/null 2>&1 && { printf '  ok    cross-compile windows/amd64\n'; pass=$((pass+1)); } || { printf '  FAIL  cross-compile windows/amd64\n'; fail=$((fail+1)); }
GOOS=linux GOARCH=arm64 go build ./... >/dev/null 2>&1 && { printf '  ok    cross-compile linux/arm64\n'; pass=$((pass+1)); } || { printf '  FAIL  cross-compile linux/arm64\n'; fail=$((fail+1)); }

echo
echo "Nothing of anyone else's"
# The pattern is ASSEMBLED rather than written, so this file never contains the
# names it looks for. A denylist spelled out in the checker makes the checker its
# own first hit, which is how this row failed on its first fresh clone.
third_party="claude""kit|mrg""oonie|duy""nguyen|bnq""toan|MA""MP"
row "third-party identifiers" 0 "$(git grep -icE "$third_party" -- . | wc -l | tr -d ' ')"
row "links to a private repository" 0 "$(git grep -cE 'openbox-core/(issues|pull)' -- . | wc -l | tr -d ' ')"
row "tracked delivery records" 0 "$(git ls-files plans | wc -l | tr -d ' ')"
row "probe evidence on disk" 0 "$([ -d .probe-evidence ] && echo 1 || echo 0)"
# A dotted directory whose own .gitignore ignores its contents still ships the
# .gitignore, so every clone carries the directory. git status never says so.
row "tracked files under a dotted tooling dir" 0 "$(git ls-files | grep -cE '^\.(codegraph|claude|fab7|probe-evidence)/' | tr -d ' ')"
for p in 'plans/' '.claude/' '.fab7/' '.probe-evidence/' '.codegraph/'; do
	row "gitignored: $p" 1 "$(grep -cFx "$p" .gitignore | tr -d ' ')"
done

echo
echo "Prose"
row "CLAUDE.md at or under 150 lines" 1 "$([ "$(wc -l < CLAUDE.md)" -le 150 ] && echo 1 || echo 0)"
row "em-dashes in Go" 0 "$(git ls-files '*.go' | xargs grep -ho '—' 2>/dev/null | wc -l | tr -d ' ')"
row "em-dashes in documentation" 0 "$(git ls-files '*.md' | xargs grep -ho '—' 2>/dev/null | wc -l | tr -d ' ')"
row "comment blocks over 12 lines" 0 "$(awk 'FNR==1{run=0} /^[ \t]*\/\//{run++; if(run>12) over[FILENAME]=1; next} {run=0} END{n=0; for(f in over) n++; print n}' $(git ls-files '*.go'))"
row "Known Limitations in the README" 1 "$(grep -c '^## Known Limitations' README.md | tr -d ' ')"

density() { # density <predicate>
	git ls-files '*.go' | awk -v want="$1" '
		(want == "test") == (/_test\.go$/) { print }
	' | xargs awk '{t++} /^[ \t]*\/\//{c++} END{printf "%.0f", (c*100)/t}'
}
row "non-test comment density at or under 12%" 1 "$([ "$(density src)" -le 12 ] && echo 1 || echo 0)"
row "test comment density at or under 12%" 1 "$([ "$(density test)" -le 12 ] && echo 1 || echo 0)"
printf '        (measured: non-test %s%%, test %s%%)\n' "$(density src)" "$(density test)"

echo
echo "Documentation integrity"
layout=$(sed -n '/^## Layout/,/^## /p' docs/architecture.md)
undocumented=""
for dir in */; do
	d="${dir%/}"
	case "$d" in .* | plans) continue ;; esac
	printf '%s\n' "$layout" | awk -F'|' -v want="\`$d/\`" '
		NF > 2 { cell = $2; gsub(/^[ \t]+|[ \t]+$/, "", cell); if (cell == want) found = 1 }
		END { exit !found }' || undocumented="$undocumented $d"
done
row "top-level directories documented" "" "$undocumented"

broken=$(python3 - <<'PY'
import io, os, re, subprocess
files = subprocess.run(['git', 'ls-files', '*.md'], capture_output=True, text=True).stdout.split()
n = 0
for f in files:
    base = os.path.dirname(f)
    for m in re.finditer(r'\]\(([^)#][^)]*)\)', io.open(f, encoding='utf-8').read()):
        t = m.group(1).split('#')[0]
        if not t or t.startswith(('http://', 'https://', 'mailto:')):
            continue
        if not os.path.exists(os.path.normpath(os.path.join(base, t))):
            n += 1
print(n)
PY
)
row "broken relative links" 0 "$broken"

echo
if [ "$fail" -eq 0 ]; then
	printf '%d rows, all green.\n' "$pass"
else
	printf '%d green, %d FAILED.\n' "$pass" "$fail"
fi
exit $((fail > 0))
