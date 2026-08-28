# Phase 05 verification — credential-guard scope (ADR-0023)

**Date:** 2026-08-28 · **Host:** macOS 25.0.0 darwin/arm64, go1.27.0 ·
**Branch:** `feat/tool-content-capture` · **Blocks:** phase 06

Deliberately weakens a security test, lands alone, and records why.
**ADR: [ADR-0023](../../../docs/adr/ADR-0023-credential-guard-scope.md).**

## Verdict

Done. The narrowing is in, the pre-existing host-matching gap is closed,
`decision/` has its own guard, and the ADR states the reduction in its title
rather than softening it into "clarified scope".

Both directions drilled **on the live `gateway/go.mod`**, not just on fixtures:
a direct unreviewed require ⇒ red; an indirect one ⇒ green.

## What changed

| Surface | Change |
|---|---|
| `gateway/guard_test.go` | go.mod half extracted to `unallowedDirectRequires`; skips `// indirect`; matches **any** module host |
| `gateway/guard_test.go` (allowlist comment) | states the boundary: direct bounded here, transitive bounded at the dependency's own module |
| `gateway/guardscope_test.go` | NEW — 7 seeded fixture cases + 2 live-file cases |
| `decision/guard_test.go` | NEW — its own allowlist + 5 seeded cases |
| `docs/adr/ADR-0023-…md` | NEW; indexed in `docs/adr/README.md` |
| `scanSource`, `forbiddenCalls`, `forbiddenLiterals`, the import half | **untouched — verified by diff** |

ADR numbered **0023**, not 0022: the plan reserves 0022 for phase 08's OSS-adoption
ADR. Numbering follows the plan rather than creation order, and the code comment
cites 0023 so the two cannot disagree.

## Evidence

Seeded fixtures rather than observations of the live file, because a live file that
passes says nothing about what the check would *reject*:

| Case | Expected | Result |
|---|---|---|
| the two real allowlisted requires | accept | pass |
| **a direct unreviewed require** | **REJECT** | pass — the mutation control |
| **an indirect unreviewed require** | **accept** | pass — the narrowing itself |
| **a direct `golang.org/x/…` + `go.opentelemetry.io/…`** | **REJECT** | pass — the closed gap |
| single-line `require path v1` form | scanned | pass |
| the `module` line | not a requirement | pass |
| a `replace` line | not a requirement | pass |

`decision/guard_test.go` mirrors it: external direct ⇒ reject; non-github direct ⇒
reject; indirect ⇒ accept; the allowlisted sibling ⇒ accept; **an unlisted sibling
⇒ reject** (being in-repo is not a free pass).

Live-file drills, which are the ones that matter:

```
append `require github.com/some/unreviewed v1.0.0`            -> RED
  guard_test.go:234: gateway/go.mod directly requires
  "github.com/some/unreviewed", which the import allowlist does not name
append `require github.com/some/transitive v1.0.0 // indirect` -> GREEN
```

Whole-workspace: gofmt clean, `go vet` clean, both cross-compiles clean, `-race`
with no data races, verdict set **+2** (`decision`'s two tests) and FAIL count
unchanged at 19.

**A gap in my own measurement, worth stating:** gateway's three new tests do NOT
appear in the whole-workspace verdict capture, because gateway's test binary
panic-truncates on a sandbox listener bind before reaching them. They were run
directly and pass — `TestGoModGuardScope` PASS, `TestLiveGoModPassesTheGuard`
PASS, `TestNarrowingAppliesToTheLiveFile` SKIP. The capture method under-reports
any test that sits after a panic in its package, which is the same limitation
recorded in the phase-01 report.

`TestNarrowingAppliesToTheLiveFile` skipping is the designed state: `gateway/go.mod`
has no `// indirect` requires yet, and the test says so rather than passing
silently. **Phase 06 is what makes it live** — gitleaks in `decision/` puts a
transitive tree into gateway's go.mod, which is the whole reason this phase exists.

## What the guarantee is now

One broad claim replaced by narrower ones, each enforced where it is checkable —
the full table is in the ADR. The honest summary:

- gateway's own files resolve no credential — **unchanged**;
- gateway imports nothing outside two modules — **unchanged**;
- gateway *requires* nothing outside two modules **directly** — narrowed;
- `decision`'s direct dependencies are reviewed — **new**;
- arbitrary transitive code resolves no provider credential — **bounded by no
  test**. Accepted residual risk, named in the ADR.

That last one **was already true**. The old check matched only lines starting
`github.com/`, so a direct `golang.org/x/…` require was invisible to it even
before this change, and its transitive coverage rested on the accident that
`client` and `decision` had no external dependencies. Making the real boundary
explicit — and closing the host gap while narrowing the scope — is most of the
value here.

## Unresolved questions

1. **`// indirect` is matched textually**, not via `go list -m` (which needs a
   resolved module graph and would make the test depend on a populated cache).
   The seeded direct case is what keeps that honest. Flagging in case a future
   `go mod` format change is considered likely enough to justify the heavier check.
2. **`unallowedDirectRequires` is duplicated** in `gateway` and `decision` rather
   than shared. Sharing it would invert the module dependency direction (ADR-0003),
   and it is test-only in a module that must not grow dependencies. The two copies
   are pinned by carrying the same seeded cases, not by sharing code — if a third
   module needs it (phases 09 and 11 will), that trade should be revisited rather
   than copied a third time.
3. **`decision`'s allowlist lists a sibling module explicitly** rather than
   pattern-excluding the repo's own prefix, to match how gateway's allowlist is
   written. It means an in-repo module still has to be listed, which the seeded
   "unlisted sibling is still rejected" case pins deliberately.
