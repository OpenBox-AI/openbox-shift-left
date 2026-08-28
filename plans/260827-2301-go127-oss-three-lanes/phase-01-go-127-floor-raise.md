# Phase 01 — Go 1.27 floor raise (blocking, lands alone)

## Context links

- Parent: [plan.md](plan.md) · Depends: — · **Blocks: every other phase except 02**
- Decision: **D-GO-1** (owner, 2026-08-27) — floor 1.23.0 → **1.27.0**
- Evidence: [audit-260827-2227](../reports/audit-260827-2227-oss-replacement-shipped-code.md) §2
- Research: [researcher-02](research/researcher-02-kardianos-go127-migration.md)

## Overview

- Date: 2026-08-27 · Priority: P1 · Effort: 4h
- Implementation status: **done** · Review status: pending
- Report: [verification-260827-phase-01](reports/verification-260827-phase-01-go-127-floor-raise.md)
- Raise the declared language floor across the workspace so every adopted
  dependency resolves at its latest version with no pin, and release the `x/term`
  pin that exists solely to hold 1.23.

## Key insights

- **CI is already ahead of the declared floor.** `.github/workflows/ci.yml:48` and
  `release.yml:28` both pin `go-version: "1.26"` while all 12 modules declare
  `go 1.23`. The floor is a *declaration* problem, not a toolchain-availability
  problem. The outlier is the local machine (go1.23.4).
- **This is what retires the round-2 pin scheme.** goproxy v1.9.0 needs 1.24,
  `otlpreceiver` v0.159.0 needs 1.25, gitleaks v8.30.1 needs 1.24.11, renameio/v2
  needs 1.25. All become reachable; none needs pinning.
- **`x/term` is pinned v0.34.0 *only* to hold this floor** — v0.35.0+ declares
  go 1.24.0. Once the floor moves the pin is obsolete, and the CLAUDE.md paragraph
  instructing future agents not to bump it becomes actively misleading. **Delete
  it, do not amend it.**
- **One commit, not twelve.** The workspace `go` directive must be ≥ every
  member's — a mismatch is not silently resolved by taking the max; the toolchain
  can refuse to build outright (go.dev/doc/toolchain). A split leaves intermediate
  commits unbuildable and bisect-hostile.
- **`GOWORK=off` makes the per-module directives the ones that matter.** The
  release build sets it (`.goreleaser.yaml:25`), which makes `go.work` invisible.
  Bumping the workspace alone would leave every released binary resolving against
  a stale floor. All 12 `go.mod` files must move, and each must be smoke-checked
  **with `GOWORK=off`**, not just through the workspace.
- **Contributors are not blocked by this.** `GOTOOLCHAIN` defaults to `auto`, so a
  developer on go1.23.4 auto-downloads and re-execs the required toolchain from
  the proxy (cached under `GOMODCACHE/golang.org/toolchain@*`). Only
  `GOTOOLCHAIN=local` hard-errors. This is a much softer migration than a floor
  raise usually implies.
- **One concrete 1.27 breaking change applies to build config, not code:**
  explicitly setting `GOEXPERIMENT=systemcrypto` *or* `nosystemcrypto` is now a
  hard error (the behavior stays on by default where supported). Grep the repo and
  CI for `GOEXPERIMENT=` before bumping.
- Released binaries are static, so **end users are unaffected**. The cost lands on
  CI and the release path only.

## Requirements

1. `go.work` and all 12 `go.mod` files declare `go 1.27.0`.
2. CI and release workflows build on 1.27.
3. `x/term` unpinned to latest (v0.45.0); its pin rationale deleted from
   `cli/go.mod`'s require block and from `CLAUDE.md`.
4. All 11 test-bearing modules green under `-race`; both cross-compiles
   (windows, linux/arm64) still pass.
5. No behavioral change anywhere. This phase adds no feature and fixes no bug.

## Architecture

No architectural change. Version-declaration surface only:

```
go.work:12                      go 1.23.0  -> go 1.27.0
12 × */go.mod                   go 1.23    -> go 1.27.0   (10 bare, 2 already .0)
.github/workflows/ci.yml:48     "1.26"     -> "1.27"
.github/workflows/release.yml:28 "1.26"    -> "1.27"
cli/go.mod                      x/term v0.34.0 -> v0.45.0, pin comment deleted
CLAUDE.md                       x/term pin paragraph deleted
```

## Related code files

- edit: `go.work`
- edit: `actions/openbox-git-action/go.mod`, `adapters/claude-code/go.mod`,
  `adapters/codex/go.mod`, `adapters/common/devconfig/go.mod`,
  `adapters/common/git/go.mod`, `adapters/common/hookflow/go.mod`, `cli/go.mod`,
  `client/go.mod`, `contracts/dev-event/conformance/go.mod`, `decision/go.mod`,
  `gateway/go.mod`, `provider/go.mod`
- edit: `.github/workflows/ci.yml`, `.github/workflows/release.yml`
- edit: `CLAUDE.md` (delete the `x/term` pin paragraph in "Next:")
- check: `.goreleaser.yaml` (comment says "all 11 modules"; go.work lists 12 —
  correct it while here)

## Implementation steps

0. **Read the release notes for 1.24, 1.25, 1.26 and 1.27 individually** and list
   every change that could touch this repo — `crypto/tls` defaults, `os`, x509
   verification, `vet` strictness (vet runs under `go test`), `encoding/json`.
   Researcher-02 could only enumerate 1.27's; treating four releases as one hop is
   how a silent behavior change gets shipped as a "version bump". Record the list
   in the phase report before touching a file.
1. `grep -rn "GOEXPERIMENT" . .github/` — an explicit `systemcrypto` /
   `nosystemcrypto` setting is a hard error on 1.27 and must be removed first.
2. Upgrade the local toolchain to go1.27.0 and confirm `go version`. Everything
   below is unverifiable without it.
3. Bump `go.work` to `go 1.27.0`, then all 12 `go.mod` files. Normalize the two
   spellings (`go 1.23` vs `go 1.23.0`) to one: `go 1.27.0`.
4. **Decide the `toolchain` directive** (see Risk table): default is to add
   `toolchain go1.27.0` to `go.work` only, leaving member modules free.
5. Bump both workflows to `"1.27"`.
6. `go get -u golang.org/x/term` in `cli/`; delete the pin comment from the
   require block. Delete CLAUDE.md's pin paragraph in the same commit — the code
   and the instruction must not disagree for even one commit.
7. Build and test every module: `go build ./cli/...`, then per-module
   `go test ./...`, then `-race`.
8. Both cross-compiles: `GOOS=windows` and `GOOS=linux GOARCH=arm64`.
9. **Per-module `GOWORK=off go build ./...`** — for every one of the 12 modules,
   not just `cli/`. This is the only check that exercises what the release path
   actually resolves.
10. `govulncheck` (CI runs it at `ci.yml:104`) — a floor change moves the stdlib
    baseline, so re-run it before declaring green.

## Todo

- [x] release notes read individually + directive-gated behavior MEASURED at 1.23 vs 1.27; impact list in the report
- [x] `GOEXPERIMENT=` grep clean (hard error on 1.27)
- [x] local toolchain already go1.27.0
- [x] `go.work` + 12 `go.mod` → `go 1.27.0`, spellings normalized
- [x] toolchain directive applied (`go.work` only)
- [x] ci.yml + release.yml → "1.27" (+ stale floor comment reconciled)
- [x] `x/term` v0.34.0 → v0.45.0; pin comment + CLAUDE.md paragraph deleted; ADR-0015 marked superseded (pin only)
- [x] verdict set IDENTICAL to HEAD (540 PASS / 19 FAIL / 1 SKIP); all 19 failures are sandbox listener blocks
- [x] `-race`: no data race; same set
- [x] both cross-compiles green
- [x] **per-module** `GOWORK=off go build ./...` green (all 12)
- [x] `govulncheck` clean 12/12 (was: reachable stdlib vulns in all 12 at 1.23)
- [x] `.goreleaser.yaml` "11 modules" → 12

## Success criteria

- Every module declares `go 1.27.0`; no module declares anything else.
- No `// pinned` / "do not bump" instruction referring to `x/term` survives
  anywhere in the tree.
- All 11 test-bearing modules green under `-race`; both cross-compiles pass;
  a `GOWORK=off` build succeeds in **each of the 12 modules**, not only `cli/`.
- Zero behavioral diff: no test's *expectations* changed in this phase. A test
  that had to be edited to pass is a signal, not a chore — see Risk table.

## Risk assessment

| Risk | Mitigation | Signal it broke | Response |
|---|---|---|---|
| **A 1.23→1.27 stdlib change alters behavior** (crypto/tls defaults, `encoding/json` behavior, x509 verification) | Full `-race` suite + conformance byte-golden fixtures before/after | A golden fixture or byte-identity test fails | **Stop and investigate.** These fixtures pin the wire; a change here is a contract change, not a migration detail. Do NOT re-bless a golden |
| A test needs editing to pass | Treat as evidence, not chore | Any expectation edit in this phase | **Stop.** This phase is declaration-only; an expectation change means real behavior moved. Split it into its own reviewed change |
| Contributor on an older toolchain is blocked | `toolchain` directive lets Go auto-download; document minimum in README | Contributor reports build failure naming a go version | Expected and correct — report the minimum, don't lower the floor |
| CI runner image lacks 1.27 | `actions/setup-go@v5` downloads it | CI fails at setup step | Pin a patch version in the workflow |
| `go get -u` on x/term drags other bumps | Bump x/term alone, review the diff | Unrelated modules move in go.sum | Revert, bump the single module explicitly |
| `govulncheck` reports new findings under the new stdlib | Run before declaring green | Non-empty govulncheck | Assess; a stdlib advisory may need a patch bump, not a rollback |
| **Silent scope creep** — "while we're here" fixes | Phase is declaration-only by construction | Diff touches a `.go` file other than version/require lines | Move it to its own phase |

## Security considerations

- A floor raise changes the **stdlib crypto baseline** the binary links. This is
  usually an improvement (newer `crypto/tls`, x509 defaults), but it is a real
  change to the code that signs events and terminates TLS. `govulncheck` and the
  full suite are the gate, not the assumption that newer is safer.
- `x/term` reads passwords in `cli/internal/prompt` (masked input). Unpinning
  moves that code. Re-verify the masked-input path against the real binary over a
  PTY — the same check the auth work used — rather than trusting unit tests.
- No credential, posture, or wire-format behavior may change in this phase. If any
  does, this phase is the wrong place for it.

## Next steps

Phase 02 (JSON Schema validator) is the only phase that does **not** depend on
this one — it can proceed in parallel if desired. Everything else waits for green.

## Outcome (2026-08-27)

Done and verified to this host's limit — see the
[verification report](reports/verification-260827-phase-01-go-127-floor-raise.md).
Zero behavioral diff proven by an identical verdict set against a detached
worktree at `HEAD`. No `.go` file touched; no test expectation edited, so the
stop-condition never fired.

**One required check could not run:** masked credential input over a PTY, which
this phase's own security note demands because `x/term` moved. The sandbox
denies PTY allocation. Outstanding before merge.

**Also blocked on this host, and it constrains later phases:** nothing may bind a
TCP or unix listener, so the enforce-path conformance suite, the gateway
listen/install suite and `gatewaycheck` cannot run — identically before and
after, so not a regression, but phase 02's acceptance ("C1-C41 pass unmodified")
has no evidence path here.
