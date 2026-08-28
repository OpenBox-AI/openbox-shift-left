# Audit — OSS replacement across shipped code + Go floor raise

Date: 2026-08-27 · Scope: all 12 modules, ~24k LOC non-test · Trigger: owner asked
what *already-implemented* code can be replaced by trusted packages, and stated a
preference for the latest Go.

Companion to [validation-260827-2154](validation-260827-2154-oss-reuse-vs-from-scratch.md)
(which covered the *unbuilt* three-lanes plan). That report's unresolved Q1
(pin vs raise) is **resolved here: raise.**

## 1. Decisions (owner, 2026-08-27)

| ID | Decision |
|---|---|
| **D-GO-1** | Raise Go floor **1.23.0 → 1.27.0** across `go.work` + all 12 `go.mod` |
| **D-OSS-4** | `decision/secrets.go` → **full gitleaks detect engine** (v8.30.1, MIT) |
| **D-OSS-5** | `conformance/validator.go` → **santhosh-tekuri/jsonschema v6.0.3** (Apache-2.0) |
| **D-OSS-6** | `devconfig/toml.go` → **pelletier/go-toml/v2 v2.4.3** (MIT) |
| **D-OSS-7** | `devconfig/envfile.go` parse side → **joho/godotenv v1.5.1** (MIT) |
| **D-OSS-8** | atomic writes → **google/renameio/v2 v2.0.2** (Apache-2.0) |

Carried from round 2, now **unpinned** by D-GO-1: goproxy **v1.9.0** (was pinned
v1.8.2), otlpreceiver **v0.159.0** (was v0.120.0), kardianos/service v1.3.0.

## 2. Go floor — latest stable is **go1.27.0** (verified `go.dev/dl?mode=json`)

Local toolchain go1.23.4. Go ≥1.21 makes a dep's `go` directive a hard build
requirement, which is what forced round 2's pins.

**Raising to 1.27.0 buys:**
- every dependency at latest, **no pins** — the round-2 pin scheme is retired;
- **`x/term` unpinned** v0.34.0 → v0.45.0. CLAUDE.md's "pinned, don't let `go mod
  tidy` bump it" paragraph becomes obsolete and must be deleted, not amended;
- unlocks gitleaks (needs go 1.24.11) and renameio/v2 (needs 1.25), neither of
  which was reachable at 1.23.

**Costs:** local toolchain upgrade; CI runner images; `.goreleaser.yaml`; both
cross-compiles re-verified; contributors need ≥1.27. Released binaries are static,
so **end users are unaffected**.

**Migration:** `go.work` + 12 `go.mod` in one commit — a split leaves the workspace
unbuildable, since the workspace `go` directive must be ≥ every member's.

## 3. Replacement audit — shipped code

### Tier A — genuine reinvention, replace (all decided)

| Code | LOC | → | Why |
|---|---|---|---|
| `contracts/dev-event/conformance/validator.go` | 211 | santhosh-tekuri/jsonschema v6 | hand-rolled draft-2020-12 subset ($ref, oneOf, format). Validates the contract the whole product rests on, incl. the new `oneOf` discriminators. Works at the OLD floor (go 1.21) — independent of D-GO-1 |
| `decision/secrets.go` | 353 | gitleaks v8.30.1 | 170+ maintained rules vs our ~10 patterns; closes the documented "keyword decides, not the charset" gap |
| `cli/internal/gatewayservice/unit.go` | ~577 pkg | kardianos/service | plist built by string concat + hand-rolled `xmlEscape`. Confirms D-OSS-3 replaces **existing** code, not just new |
| `adapters/common/devconfig/toml.go` | 57 | pelletier/go-toml/v2 | **correctness bug**, see §4.1 |
| `adapters/common/devconfig/envfile.go` | 218 | joho/godotenv | parse side only; write side stays custom (0600 + `TestEnvFileIsNotACoordinateSource`) |

### Tier B — adopted, low real-world gain (honest)

`adapters/common/hookflow/{duration,turncursor,findings}.go`, `gatewayservice/env.go`
→ `renameio/v2`. All do `CreateTemp → write → Close → Rename` with **no `f.Sync()`
and no directory fsync**, so contents can be lost on crash while the rename appears
durable. Caveat: the design *already* tolerates crash loss (spool-then-cursor
deliberately over-reports into core's dedupe), so this hardens a path whose failure
mode is already handled. Adopted as hygiene, not as a bug fix.

### Tier C — correctly NOT reinvented, leave alone

- **`adapters/common/git/`** — shells out to the real `git` binary (`exec.Command`).
  Correct: respects the developer's gitconfig, signing and credential helpers.
  go-git would *lose* capability. Do not "modernize" this.
- **`client/signing.go`** — stdlib `crypto/ed25519` + the platform's AIP canonical
  form. The protocol is the contract; no OSS equivalent exists.
- **`cli/cmd/openbox`** — already uses stdlib `flag` (`flags.go:10`), not a
  hand-rolled parser. Cobra/urfave would add a dependency and a help-output
  rewrite for zero capability.
- **`gateway/` relay** — byte-identity proven and tested; established round 2.
- **`hookflow` engine, mappers, contract, `aivss`** — the product itself.

### Excluded

**TruffleHog — AGPL-3.0.** Copyleft on a distributed commercial binary. Gitleaks
(MIT) is the correct pick. All other adoptions are permissive: Apache-2.0
(jsonschema, renameio, collector), MIT (gitleaks, go-toml, godotenv), BSD-3
(goproxy), Zlib (kardianos).

## 4. Consequences that must be handled

### 4.1 The TOML scanner is wrong on valid input (fixed by D-OSS-6)

`toml.go:39` sets `inTable = true` on **any** line whose first character is `[`,
then skips everything after. A multi-line basic string, or an array value
continued across lines, containing such a line makes every later top-level key
invisible. `codexMandated` (`posture.go:122`) consumes exactly this to decide
whether Codex hooks are mandated by `requirements.toml` — so a legitimately
mandated install can read as unmandated. Not just LOC: a posture bug.

### 4.2 Full gitleaks BREAKS the gateway credential guard — needs a deliberate fix

`gateway/guard_test.go:231-243` iterates every line in `gateway/go.mod` starting
with `github.com/` and fails on any module not in the two-entry allowlist. It does
**not** distinguish direct from `// indirect`.

`gateway` imports `decision`. Putting gitleaks in `decision/` makes gitleaks **and
its entire transitive tree** (zerolog, viper, go-gitdiff, semgroup, …) indirect
requires of `gateway/go.mod` — so `TestGatewayImportsAreConfined` goes red on
every one of them.

**Resolution (recommended):** change the go.mod half to skip `// indirect` lines.
Defensible — the *import* half already covers what gateway's own source imports,
and an allowlisted module's own dependencies are that module's business, reviewed
at its own guard. Do **not** resolve it by listing the transitive tree in the
allowlist: that makes the allowlist unreviewable, which is the one thing it exists
to be.

Two things to record while touching it:
- this weakens a load-bearing security test → **ADR-worthy**, not a quiet commit;
- pre-existing gap found while reading it: the go.mod scan only matches
  `github.com/` prefixes, so a `golang.org/x/…` or `go.opentelemetry.io/…`
  requirement is invisible to it today. Fix in the same change or the guard keeps
  claiming more than it checks.

### 4.3 Dependency surface of the most sensitive module

`decision/` is imported by `gateway/`, `client/` paths and every adapter. Gitleaks
v8 brings a large tree into it, inflating the binary and the audit surface of the
module that performs redaction. Accepted with D-OSS-4. If the tree proves
unacceptable at implementation time, the documented fallback is **rules-only**:
embed gitleaks' TOML rule pack, keep our matcher and its pinned tests.

### 4.4 The redaction tests are the safety net — they must not be rewritten to fit

`TestRedact_ValueEndingInBackslash` and `TestRedact_JSONShapedSecrets/escaping_survives`
pin two directions that have already shipped broken once **together**. The
enforce-path redactor rewrites file bodies, so a gitleaks false positive corrupts
a developer's file. Requirements for D-OSS-4:
- both tests pass **unmodified** against the gitleaks-backed detector;
- `TestFinops_NoContentOnWire`'s mutation drills still go red when redaction or the
  cap is deleted;
- a false-positive soak over a real repo (git SHAs, UUIDs, base64 assets) **before**
  the enforce path uses it — the 4.5-bit entropy floor exists because below 4.0
  every git SHA matches.

### 4.5 INV-2 / content-gate semantics must survive the validator swap

`validator.go` carries `x-content-gated` as a custom keyword and runs the content
gate as a **separate pass**, deliberately, so a posture violation is never
conflated with a structural mismatch during `oneOf` branch trials. Port it as a
custom vocabulary in santhosh-tekuri and keep the two passes separate; collapsing
them into one changes which error a failing branch reports.

## 5. Sequencing

1. **D-GO-1 first, alone.** Floor + toolchain + CI + `.goreleaser`, `x/term`
   unpinned, CLAUDE.md pin paragraph deleted. Green under `-race` + both
   cross-compiles before anything else lands. Everything else depends on it
   (except D-OSS-5, which is floor-independent).
2. **D-OSS-5** (jsonschema) — self-contained, highest correctness value, no floor
   dependency. Good first swap.
3. **D-OSS-6, D-OSS-7, D-OSS-8** — small, independent, low risk.
4. **D-OSS-4** (gitleaks) **last**, with §4.2's guard change as its own reviewed
   commit + ADR, and §4.4's soak gating the enforce path.
5. Then the three-lanes plan re-cut, now unpinned.

## 6. Effect on plan 260827-1602-three-lanes-convergence

> **2026-08-27:** that plan and `260827-2245-go127-and-oss-consolidation` were
> merged into `plans/260827-2301-go127-oss-three-lanes/`; the effects below are
> incorporated there (D-GO-1 = phase 01, the re-cut = phases 08–14).

- version pins **retired** — goproxy v1.9.0, otlpreceiver v0.159.0 at latest;
- round-2 unresolved Q1 **resolved** (raise, not pin);
- round-2 Q2/Q3 (goproxy SSE fallback, `transport/` as its own module) **still open**;
- `transport/` as its own module gains a second reason: §4.2 shows how fast an
  allowlist erodes once a heavy dependency lands in a module others import.

## Unresolved questions

1. **§4.2 guard fix** — skip `// indirect` (recommended), or another shape? It
   weakens a security test either way and wants an ADR.
2. **Toolchain directive** — pin an exact `toolchain go1.27.0` line alongside
   `go 1.27.0`, or leave toolchain selection to the developer? Exact pin makes CI
   and local builds identical; it also forces every contributor onto that patch.
3. **D-OSS-4 abort criterion** — if the false-positive soak (§4.4) shows corruption
   risk on real repos, is the fallback rules-only, or keep the current detector?
4. Carried from round 2: goproxy SSE fallback, and `transport/` module placement.
