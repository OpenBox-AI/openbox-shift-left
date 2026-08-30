# ADR-0023 — The credential guard bounds DIRECT requires only

**Status:** Accepted
**Amended by:** [ADR-0024](ADR-0024-single-module-layout.md) (2026-08-30) — the bound moves from the module to the package subtree
**Date:** 2026-08-28
**Supersedes in part:** the go.mod half of `gateway/guard_test.go` as written for
[ADR-0021](ADR-0021-openbox-local-gateway.md)
**Context:** plan `260827-2301-go127-oss-three-lanes`, phase 05 (blocks phase 06)

## This is a reduction in what a security control proves

Stated first and plainly, because the rest of this ADR is the argument for
accepting it and an argument is easy to mistake for a denial that anything was
lost.

`gateway/guard_test.go` had two halves. The **import half** parses every file in
the module and rejects any non-stdlib import outside a two-entry allowlist. The
**go.mod half** read the file and rejected any requirement outside that same
allowlist — *including transitive ones*. Together they were often described as
bounding "everything the gateway can execute".

The go.mod half now considers **direct** requirements only. An `// indirect`
requirement is skipped. So the guard no longer says anything at all about
arbitrary transitive code linked into the binary.

## Why

`gateway/` imports `decision/` (secret redaction) and `client/` (AIP signing),
both deliberately — ADR-0021 §Reuse argues that reimplementing signing or secret
detection inside the gateway would be far worse than importing them.

The moment an allowlisted module grows an external dependency, `go mod tidy`
writes that dependency **and its whole transitive tree** into `gateway/go.mod` as
`// indirect` lines, and the old check went red on every one. Phase 06 puts
`gitleaks` into `decision/`, which brings a large tree.

There were three ways out:

1. **List the transitive closure in the allowlist.** Rejected. The allowlist's
   entire value is that it is short enough for a human to read and object to.
   A list of ~40 transitive modules is not reviewed, it is tolerated.
2. **Refuse the dependency.** Rejected: it would mean the gateway can never reuse
   a module that itself has dependencies, which contradicts the reuse rule this
   repo is built on and would push the gateway back toward its own copy of
   redaction.
3. **Bound direct requires here; bound transitive at the module that took the
   dependency.** Accepted.

## What the guarantee is now

One broad statement is replaced by three narrower ones, each enforced where it is
actually checkable:

| Claim | Enforced by | Status |
|---|---|---|
| The gateway's own files resolve no credential | `scanSource` + `TestGatewayReadsNoCredential` | **unchanged, byte-identical** |
| The gateway imports nothing outside two modules | the import half of `TestGatewayImportsAreConfined` | **unchanged** |
| The gateway *requires* nothing outside two modules, directly | `unallowedDirectRequires` | **narrowed** — indirect skipped |
| `decision`'s direct dependencies are reviewed | `decision/guard_test.go` | **new** |
| `contracts/dev-event/conformance`'s are reviewed | `conformance/deps_test.go` | added by D-OSS-5 |
| Arbitrary transitive code resolves no provider credential | — | **not bounded by any test** |

The last row is the accepted residual risk. It is named rather than softened.

**It was already true.** The old go.mod check only *appeared* to cover it: it
matched lines beginning `github.com/` and nothing else, so a direct
`golang.org/x/…` or `go.opentelemetry.io/…` requirement was invisible to it even
before this change, while its transitive coverage depended entirely on the
accident that `client` and `decision` had no external dependencies of their own.
An over-claiming control is worse than an honest one — that is this product's own
stated principle, and applying it to our own test is the point of this ADR.

While narrowing the check, that host-matching gap was **closed**: the rule is now
the same one the import half uses — a dot in the first path segment means the path
is not stdlib — so a direct requirement on any host is caught.

## What compensates

- The import half is untouched. A credential read cannot hide in a local package,
  because there are no non-stdlib local packages to hide in beyond the two.
- `scanSource` is untouched, and verified so by diff. Its identifier-vs-import-path
  keying and its `syscall` / `io/ioutil` aliasing defenses are unrelated to this
  change and must not be weakened by it.
- The per-module pattern is now the repo's convention, not a one-off: every module
  that takes a non-stdlib dependency carries its own allowlist test. `decision/`
  gains one here, with an empty allowlist, specifically so that phase 06 has to
  add `gitleaks` to it **deliberately** and visibly.
- Stage B extends the same shape: `telemetry/` (phase 09) and `transport/`
  (phase 11) each arrive with their own guard enumerating their own direct
  requires.

## Consequences

- A dependency review is now the control for transitive code, and there is no test
  standing in for it. If a dependency is found reading the developer's provider
  credential, the response is to remove or replace that dependency — **not** to
  widen an allowlist.
- **Moving this boundary again is a new decision.** Citing this ADR to justify
  adding an unreviewed *direct* import would invert its reasoning: the narrowing
  was acceptable only because the direct surface stayed small and enumerated.
- The `// indirect` marker is matched textually rather than via `go list -m`,
  which would need a resolved module graph and make the test depend on a populated
  cache. `TestGoModGuardScope` is what keeps the textual match honest: it seeds a
  direct unreviewed require and requires it to be rejected, so a parser that
  stopped discriminating fails there rather than passing everything.

## Verification

`gateway/guardscope_test.go` seeds fixture go.mod bodies rather than observing the
live file, because the live file passing says nothing about what the check would
reject:

- a direct unreviewed require ⇒ **red** (mutation control);
- an indirect unreviewed require ⇒ **green** (the narrowing);
- a direct `golang.org/x/…` and `go.opentelemetry.io/…` require ⇒ **red** (the
  closed gap);
- `module` and `replace` lines are not requirements;
- both the block and single-line `require` forms are scanned.

`decision/guard_test.go` mirrors it with an empty allowlist and its own seeded
case.

## Amendment, 2026-08-30 — the bound is a package subtree now

This ADR's scope sentence says transitive code is bounded **at the module that
took the dependency**. With one module ([ADR-0024](ADR-0024-single-module-layout.md))
that sentence has no referent, and a guard whose documentation no longer describes
what it does is worse than no guard.

The full amendment is in ADR-0024 under *The ADR-0023 amendment*. In brief:

- the bound is the **package subtree**, and the guards live in `internal/depguard`;
- membership is **entry-or-subpackage with a slash boundary**, because allowlists
  name module paths while imports name package paths;
- an `// indirect` requirement is invisible to an import walk;
  `internal/conformance` keeps a package **closure** check for exactly that
  reason;
- the transitive hole this ADR already accepted is **unchanged in kind and wider
  in reach**, because any package may now import any other;
- `gateway`'s go.mod cross-check is **dissolved, conditionally** — only because
  the import walk covers `_test.go` files and classifies repo-local imports;
- **ten previously unguarded modules lose their `go.mod` as a review surface**,
  accepted as a named loss rather than answered with a root-level allowlist.

**Which allowlist fails first, for someone adding a dependency:** the subtree
guard in `internal/depguard` for `internal/{decision,telemetry,transport,gateway}`,
and the closure check for `internal/conformance`. Everywhere else nothing fails —
that is the named loss above, not an oversight.
