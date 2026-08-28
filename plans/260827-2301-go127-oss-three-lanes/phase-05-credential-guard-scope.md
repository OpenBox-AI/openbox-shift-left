# Phase 05 — credential-guard scope: direct vs indirect requires (ADR)

## Context links

- Parent: [plan.md](plan.md) · Depends: [phase-01](phase-01-go-127-floor-raise.md)
- **Blocks: [phase-06](phase-06-gitleaks-detection-engine.md)** — gitleaks cannot
  land until this does
- Evidence: [audit-260827-2227](../reports/audit-260827-2227-oss-replacement-shipped-code.md) §4.2
- Scout: [scout-01](scout/scout-01-replacement-seams.md) §Guard

## Overview

- Date: 2026-08-27 · Priority: P1 · Effort: 3h
- Implementation status: **done** · Review status: pending
- Report: [verification-260828-phase-05](reports/verification-260828-phase-05-credential-guard-scope.md) · ADR: [ADR-0023](../../docs/adr/ADR-0023-credential-guard-scope.md)
- Narrow `gateway/guard_test.go`'s go.mod check from *every* requirement to
  *direct* requirements, and record why in an ADR. This deliberately weakens a
  load-bearing security test, so it lands alone and reviewed — never bundled into
  the change that needs it.

## Key insights

- **This is a security-test change, and it must not arrive as a side effect.**
  Phase 06 puts gitleaks into `decision/`, which `gateway/` imports; `go mod tidy`
  then writes gitleaks *and its whole transitive tree* into `gateway/go.mod` as
  `// indirect` requires. `guard_test.go:231-243` iterates every line beginning
  `github.com/` and fails any module not in the 2-entry allowlist — so the guard
  goes red on each of them. Doing the guard change inside the gitleaks commit
  would make a security test's weakening look like dependency housekeeping.
- **The narrowing is defensible on its own terms.** The guard has two halves
  (`guard_test.go:199-223` imports, `231-243` go.mod). The import half already
  covers what gateway's own source can reach; the go.mod half exists so the two
  "cannot drift apart" (its comment). An allowlisted module's *own* dependencies
  are that module's business, reviewed at that module's own guard — not
  gateway's. What must stay impossible is gateway importing something unreviewed
  **directly**, and the import half is what enforces that.
- **The wrong fix is listing the transitive tree in the allowlist.** That makes
  the allowlist unreviewable, which is the single thing it exists to be. The
  allowlist's value is that it is short enough to read.
- **A pre-existing gap must be fixed in the same change**, or the guard keeps
  claiming more than it checks: the go.mod scan only matches lines starting
  `github.com/`, so a `golang.org/x/…` or `go.opentelemetry.io/…` requirement is
  invisible to it today. The import half catches those (it tests for a dot in the
  first path segment); the go.mod half does not.
- **`decision/` needs its own guard once it grows a dependency.** Today the
  allowlist's promise is transitive-by-accident: `decision` has no external deps,
  so "gateway's imports are confined" happened to bound everything. After phase 06
  that is no longer true, and the promise has to be re-established one module down.
- The same per-module pattern extends to stage B: `telemetry/`
  ([phase 09](phase-09-telemetry-receiver-daemon.md)) and `transport/`
  ([phase 11](phase-11-transport-proxy-service.md)) each arrive with their own
  guard allowlist, enumerating their own direct requires. This ADR's boundary —
  direct requires bounded at each module, transitive bounded at the dependency's
  own module — is what makes those additions reviewable.

## Requirements

1. `guard_test.go`'s go.mod half considers **direct** requires only; `// indirect`
   lines are skipped explicitly and the reason is stated in the code.
2. The same half matches **any** non-stdlib module path, not just `github.com/`.
3. A test proves the narrowed guard still fails on a direct unreviewed require.
4. A test proves it now passes with an indirect unreviewed require.
5. `decision/` gains its own `guard_test.go` allowlist, mirroring gateway's, so
   the bound is re-established at the module that actually grows the dependency.
6. **ADR** recording the narrowing: what the guard promised before, what it
   promises now, and why the difference is acceptable.

## Architecture

```
BEFORE:  gateway/go.mod : every `github.com/...` line ∈ allowlist(2)
         gateway/*.go   : every non-stdlib import ∈ allowlist(2)

AFTER:   gateway/go.mod : every DIRECT require (any host) ∈ allowlist(2)
         gateway/*.go   : every non-stdlib import ∈ allowlist(2)      [unchanged]
         decision/go.mod: every DIRECT require ∈ decision's own allowlist   [NEW]
```

Parsing direct vs indirect: a `// indirect` comment on the require line. Prefer
`go list -m -f '{{if not .Indirect}}...'` over string matching if it can run
hermetically in the test; otherwise match the comment and say so.

## Related code files

- edit: `gateway/guard_test.go:225-244` (the go.mod half only — leave the source
  scan and `scanSource` alone)
- new: `decision/guard_test.go` (allowlist + go.mod check, ported from gateway's)
- new: `docs/adr/ADR-00XX-credential-guard-scope.md`
- reference: `gateway/guard_test.go:179-197` (the allowlist and its rationale
  comment — extend it to state the direct/indirect boundary)

## Implementation steps

1. Write the two new guard cases **first**, against the current code: a direct
   unreviewed require must fail; an indirect one must currently *also* fail
   (documenting today's behavior).
2. Narrow the go.mod half to direct requires; widen its host matching to any
   module path (dot in the first segment, matching the import half's rule).
3. Flip the indirect case's expectation to pass. The direct case must stay red
   when seeded — a guard that passes everything is worse than none.
4. Extend the allowlist comment (`guard_test.go:179-197`) to state the new
   boundary explicitly: direct imports bounded here, transitive bounded at the
   dependency's own module.
5. Add `decision/guard_test.go` with its own allowlist. Today it is empty; phase
   06 adds gitleaks to it deliberately, which is the point.
6. Write the ADR. It must state plainly that this is a **reduction** in what the
   guard proves, and what compensates (the import half, plus decision's new guard).
7. Run both modules' tests; confirm gateway's credential *source* scan
   (`scanSource`) is untouched by diffing that function.

## Todo

- [x] guard cases written first (7 fixtures + 2 live)
- [x] go.mod half narrowed to direct requires
- [x] host matching widened beyond `github.com/` (the pre-existing gap, closed)
- [x] seeded direct-require case still fails — drilled on the LIVE go.mod, both directions
- [x] allowlist comment states the new boundary
- [x] `decision/guard_test.go` added — no EXTERNAL entry; the one line is a sibling, listed like gateway's
- [x] ADR-0023 written, indexed, and cited from `gateway/guard_test.go`
- [x] `scanSource` unchanged (verified by diff)

## Success criteria

- Seeding `gateway/go.mod` with a direct unreviewed require → guard **red**.
- Seeding it with an indirect unreviewed require → guard **green**.
- Seeding with a direct `golang.org/x/…` require → guard **red** (the gap closed).
- `decision/guard_test.go` is red when `decision/go.mod` grows an unlisted direct
  require.
- The credential source scan (`scanSource`) and the import half are byte-identical
  to before.
- The ADR exists and is referenced from the test.

## Risk assessment

| Risk | Mitigation | Signal it broke | Response |
|---|---|---|---|
| **The narrowing hides a real credential path** — an indirect dep reads the developer's provider credential | The import half still bounds gateway's own source; `decision`'s new guard bounds the next hop. Neither is a *proof* about arbitrary transitive code | A dependency is found reading the provider credential env var | **This is the accepted residual risk and the ADR must name it.** Response is dependency review, not a guard tweak |
| Guard is weakened further later, citing this ADR as precedent | ADR states the boundary and that moving it again is a new decision | A future change widens the allowlist instead of justifying a direct import | Reject; the allowlist's value is being short |
| `// indirect` comment parsing is fragile (`go mod tidy` reformatting) | Prefer `go list -m` if hermetic; else assert on a fixture go.mod | Guard silently passes everything | **Mutation control** — the seeded direct case going green is the alarm |
| Bundling this into phase 06 | Separate phase, separate commit, separate review | The gitleaks diff contains guard_test.go | Split it out before review |
| `decision`'s guard is added but never enforced (empty allowlist reads as "nothing to check") | Seed test proving it fails on an unlisted direct require | Seeded case green | Fix before phase 06 lands |

## Security considerations

- This phase **reduces** what an existing security control proves. That is the
  whole change; it must be visible in the ADR title and in the test's own comment,
  not softened into "clarified scope".
- The compensating controls are: (a) gateway's source scan, untouched — its own
  files still resolve no credential; (b) gateway's import half, untouched — its
  direct surface stays two modules; (c) `decision`'s new guard, bounding the next
  hop's direct surface.
- What is genuinely no longer bounded: arbitrary transitive code linked into the
  binary. That was **already** true for anything `client` pulled in, and the old
  go.mod check only appeared to cover it. Making the real boundary explicit is
  part of the value here — an over-claiming control is worse than an honest one,
  which is this product's own stated principle.
- Do not weaken `scanSource`. The identifier-vs-import-path keying and the
  `syscall`/`io/ioutil` aliasing defenses are unrelated to this change.

## Next steps

Phase 06 (gitleaks) may proceed only after this is green and reviewed.

## Outcome (2026-08-28)

Done — see the
[verification report](reports/verification-260828-phase-05-credential-guard-scope.md)
and [ADR-0023](../../docs/adr/ADR-0023-credential-guard-scope.md).

Numbered **0023**, not 0022: the plan reserves 0022 for phase 08.

Both directions drilled on the LIVE `gateway/go.mod`, not only on fixtures —
direct unreviewed require ⇒ red, indirect ⇒ green. `scanSource` and the import
half are byte-identical, verified by diff.

**`TestNarrowingAppliesToTheLiveFile` currently SKIPs**, and that is the designed
state: `gateway/go.mod` has no `// indirect` requires yet, so the test says so
instead of passing silently. **Phase 06 makes it live** — putting gitleaks into
`decision/` is exactly what pushes a transitive tree into gateway's go.mod, which
is the situation this phase exists to make survivable.
