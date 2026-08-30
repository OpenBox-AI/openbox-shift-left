# Phase 03 — config parsers and atomic writes (TOML, .env, renameio)

## Context links

- Parent: [plan.md](plan.md) · Depends: [phase-01](phase-01-go-127-floor-raise.md)
  (renameio/v2 needs go 1.25; the other two would build at the old floor)
- Decisions: **D-OSS-6** (pelletier/go-toml/v2), **D-OSS-7** (joho/godotenv),
  **D-OSS-8** (google/renameio/v2)
- Scout: [scout-01](scout/scout-01-replacement-seams.md) §D-OSS-6/7/8

## Overview

- Date: 2026-08-27 · Priority: P2 · Effort: 5h
- Implementation status: **done** · Review status: pending
- Report: [verification-260828-phase-03](reports/verification-260828-phase-03-config-parsers-and-atomic-writes.md)
- Three small, independent swaps batched because each is a single file with a
  narrow consumer set. **One of them fixes a real posture bug**; the other two are
  code reduction and durability hygiene.

## Key insights

- **The TOML swap is a bug fix, not a cleanup.** `devconfig/toml.go:39` sets
  `inTable = true` on *any* line whose first character is `[`, then skips
  everything after it. A multi-line basic string or a wrapped array value
  containing such a line therefore hides every later top-level key. Its single
  consumer is `adapters/codex/posture.go:122` `codexMandated`, which decides
  whether Codex hooks are **mandated by `requirements.toml`** — so the failure
  mode is a mandated install silently reading as unmandated. Write the failing
  test against the current code *first*, so the fix is proven rather than assumed.
- **godotenv replaces the parse side only.** `WriteEnvFile` keeps 0600 and the
`TestEnvFileIsNotACoordinateSource` invariant — a DID in `.env` must stay ignored,
because relaxing it reopens the bug where a stale second copy reverted a corrected
DID on every install. godotenv has no opinion about that; it must not acquire one.
- **renameio is hygiene, and the plan should say so honestly.** The four sites do
  `CreateTemp → write → Close → Rename` with no `f.Sync()` and no directory fsync,
  so contents can be lost on crash while the rename looks durable. But the design
  *already* tolerates that loss — spool-then-cursor ordering deliberately
  over-reports into core's dedupe rather than losing a turn. This hardens a path
  whose failure mode is handled. Adopt it; do not claim it fixes a live bug.
- **`hookflow/advisory.go:121` is a different pattern** — `O_APPEND`, atomic by
  POSIX for small writes, deliberately so. renameio must not touch it.

## Requirements

1. `TopLevelTOMLKeys` parses via `pelletier/go-toml/v2`; `devconfig/toml.go`'s
   hand scanner deleted.
2. A regression test reproducing the `[`-in-string bug fails before the swap and
   passes after.
3. `ParseEnvFile` parses via `godotenv`; `unquote` deleted if it becomes dead.
   `WriteEnvFile` unchanged in behavior and permissions.
4. The four `CreateTemp→Rename` sites use `renameio/v2`; `advisory.go` untouched.
5. No public API of `devconfig` or `hookflow` changes.

## Architecture

```
devconfig.TopLevelTOMLKeys(raw []byte) map[string]bool     ← same signature
  └─ was: line scanner (57 loc)   now: go-toml Unmarshal into map[string]any,
                                        take keys whose value is not a table

devconfig.ParseEnvFile(path) (map[string]string, error)    ← same signature
  └─ was: hand parse + unquote     now: godotenv.Read
devconfig.WriteEnvFile(path, kv)                            ← UNCHANGED (0600)

hookflow/{duration,turncursor,findings}.go, gatewayservice/env.go
  └─ was: CreateTemp→Write→Close→Rename   now: renameio.WriteFile (fsyncs)
hookflow/advisory.go                                        ← UNCHANGED (O_APPEND)
```

Signatures deliberately unchanged: the consumers (`codexMandated`, the auth/init
paths, the hook engine) are not part of this phase.

## Related code files

- edit: `adapters/common/devconfig/toml.go` (replace body; keep signature)
- edit: `adapters/common/devconfig/envfile.go` (parse side only)
- edit: `adapters/common/hookflow/duration.go:45-64`,
  `adapters/common/hookflow/turncursor.go:124`,
  `adapters/common/hookflow/findings.go:253`,
  `cli/internal/gatewayservice/env.go`
- edit: `adapters/common/devconfig/go.mod`, `adapters/common/hookflow/go.mod`,
  `cli/go.mod` (new requires)
- untouched: `adapters/common/hookflow/advisory.go`
- test: new regression test in `devconfig`; existing
  `TestEnvFileIsNotACoordinateSource` must pass unmodified

## Implementation steps

1. **TOML, test first.** Add a case to the `devconfig` tests: a `requirements.toml`
   whose top level contains a multi-line string (or wrapped array) with a line
   starting `[`, followed by a real top-level key. Assert the key is found.
   Confirm it FAILS against the current scanner.
2. Swap `TopLevelTOMLKeys` to `go-toml/v2`: unmarshal into `map[string]any`, then
   take keys whose value is not a table/array-of-tables (that is what "top level"
   meant). Delete the scanner. Confirm the new test passes and
   `adapters/codex` tests stay green.
3. Swap `ParseEnvFile` to `godotenv.Read`. Verify quoting/escaping parity against
   the existing tests; delete `unquote` if unused. Leave `WriteEnvFile` alone.
4. Confirm `TestEnvFileIsNotACoordinateSource` passes **unmodified**.
5. Swap the four atomic-write sites to `renameio`. Preserve file modes (0600) and
   the enclosing 0700 dirs explicitly — do not assume the library's defaults match.
6. Verify `advisory.go` was not touched.
7. Per-module `go test ./...`, then `-race`, then both cross-compiles. renameio's
   Windows path differs from unix — the cross-compile is the check that matters.

## Todo

- [x] failing TOML regression test written first (the FIRST attempt did not reproduce — recorded)
- [x] `TopLevelTOMLKeys` → go-toml/v2; scanner deleted; `TestCodexMandated` green; real-fixture key sets identical
- [x] `ParseEnvFile` → godotenv at package defaults (owner ruling); `unquote` deleted as dead
- [x] `TestEnvFileIsNotACoordinateSource` green **unmodified**; 4 OTHER subtests changed by ruling, each labelled
- [x] 4 sites → one helper per module, renameio on unix / prior code on Windows (package is `!windows`); modes ASSERTED + drilled
- [x] `advisory.go` untouched (empty `git diff`)
- [x] `-race` + both cross-compiles + per-module `GOWORK=off` (12/12)

## Success criteria

- The TOML regression test fails on the old scanner and passes on go-toml.
- `codexMandated` returns the same answer as before on every existing fixture,
  and the correct answer on the new one.
- `.env` files written by the old code parse identically under godotenv
  (round-trip test over the existing fixtures).
- File permissions unchanged: `.env` 0600, session dirs 0700 — asserted, not
  assumed.
- No public signature in `devconfig` or `hookflow` changed.

## Risk assessment

| Risk | Mitigation | Signal it broke | Response |
|---|---|---|---|
| **go-toml's notion of "top level" differs from the scanner's** on real files | Run both implementations over every existing fixture and diff the key sets before deleting the scanner | Key sets differ on a fixture that is not the bug case | **Investigate before deleting.** A difference may be the bug or may be a new one — decide which, don't assume |
| godotenv's quoting/escaping differs from `unquote` | Round-trip every existing `.env` fixture through both | A fixture parses to a different value | Keep `unquote` for the divergent case, or reject the swap for `.env` and keep ours — say which in the phase report |
| **renameio changes file mode or ownership** | Assert modes explicitly in tests after each write | A 0600 file appears as 0644 | Stop — a world-readable `~/.openbox/.env` is a credential exposure |
| renameio behaves differently on Windows | Cross-compile + review its Windows path | Cross-compile fails, or Windows-only code path diverges | Windows is build-verified only; keep it that way and state the limit |
| Swap changes `.env` write behavior | Write side explicitly out of scope | `WriteEnvFile` appears in the diff | Revert that hunk |
| A "dead" helper is still referenced by a test | Compile before deleting | Build error naming `unquote` | Keep it; deletion is not the goal |

## Security considerations

- `envfile.go` handles `~/.openbox/.env` — the **plaintext credential store**
(that decision: 0600 on unix, unprotected on Windows). A parser swap must not
change which keys are honoured. `TestEnvFileIsNotACoordinateSource` is the
tripwire: a DID in `.env` must stay ignored, or the two-DID-stores revert bug
reopens.
- godotenv must not gain the ability to *expand* variables or read a second file;
  either would widen what the credential store can express. Use its plain `Read`.
- renameio touches files under `~/.openbox/` and the session spool. Modes are the
  security property here, and they must be asserted after the swap, not inherited.
- The TOML fix *tightens* posture detection (more mandated installs correctly
  detected). That direction is safe; the reverse would not be.

## Next steps

Phase 04 replaces the launchd plist generation. Phase 05 (credential-guard scope)
must land before phase 06 (gitleaks).

## Outcome (2026-08-28)

Done — see the
[verification report](reports/verification-260828-phase-03-config-parsers-and-atomic-writes.md).

**D-OSS-6 was the real bug and it is fixed.** Worth keeping: the first
reproduction attempt PASSED against the buggy scanner. `key = [` with indented
elements does not trip it — those lines start with a quote or digit. It takes a
continuation line whose own first character is `[`.

**Two decisions did not survive contact with the code, were escalated, and the
owner ruled: follow the package default, do not work around anything.**

- **D-OSS-7:** the plan's mitigation ("use its plain `Read`") does not exist —
godotenv expands variables unconditionally. Measured 5 divergences, all losses,
and found a 6th while implementing: its parse error ECHOES the offending line, so
a malformed credential line leaks the secret into logs. All accepted per the
ruling; the duplicate-key refusal that decision defended is gone. Everything is
pinned by tests, including the disclosure.
- **D-OSS-8:** `renameio/v2` is `!windows` on every file, so the Windows
  cross-compile broke — invisible to the workspace build and to every test. Taken
  as honouring the package's own declared scope: renameio on unix, prior code on
  Windows, four duplicated write blocks collapsed into two helpers. Unix gains
  the fsync; Windows is unchanged, stated rather than averaged away.
