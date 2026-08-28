# Phase 02 verification — JSON Schema validator → santhosh-tekuri/jsonschema/v6

**Date:** 2026-08-27/28 · **Host:** macOS 25.0.0 darwin/arm64, go1.27.0 ·
**Branch:** `feat/tool-content-capture` · **Decision:** D-OSS-5

Replaces the 211-line hand-rolled draft-2020-12 subset with the reference
library. Content-gate pass kept separate and unchanged.

## Verdict

Done and verified **without needing a relaxed sandbox** — see
[The acceptance criterion re-read](#the-acceptance-criterion-re-read). All 13
tests in the module pass, the four tests in other modules that exercise the
validator pass, and the whole-workspace verdict set moved by exactly the six
tests added and the one replaced — nothing else.

The library and the retired walk agreed on **1893 of 1893** instances. That
result was proven non-vacuous before being believed.

## The acceptance criterion re-read

The phase's stated acceptance is "every existing conformance case (C1–C41) passes
unmodified", and the working assumption entering this phase was that the sandbox
blocked it, because those cases drive a real `/evaluate` stub over HTTP and
nothing here can bind a listener.

**That was the wrong reading.** The C-numbered cases live in
`adapters/claude-code/enforce_conformance_test.go`, `enforce_test.go`,
`conformance_parity_test.go` and the codex equivalents, and **none of them
imports `contracts/dev-event/conformance`**. The module's only importers are:

| Test | Exercises | Result here |
|---|---|---|
| `adapters/claude-code/conformance_test.go` → `TestEmittedEventsAreConformant` | `ValidateDevEvent` on every emitted event | **PASS** |
| `adapters/codex/conformance_test.go` → `TestEmittedEventsAreConformant` | same, codex mapper | **PASS** |
| `client/acceptancetest/vocabulary_test.go` → `TestSchemaEnumMatchesClientConstants` | `LoadSchema` vs client constants | **PASS** |
| `client/acceptancetest/vocabulary_test.go` → `TestSessionOrderingCoversWholeVocabulary` | vocabulary coverage | **PASS** |

None needs a listener. So C1–C41 is a whole-suite regression tripwire that is
*insensitive to a validator swap*, and the swap's actual consumer surface is
fully verifiable on this host. C1–C41 themselves remain listener-blocked here
(unchanged from baseline, and unchanged by this phase); CI on ubuntu is where
they run.

## Differential result — the core evidence

`--tdd` for a swap means pinning current behavior before replacing it. The
existing suite has six invalid fixtures, which can catch a library that is
STRICTER (a valid fixture goes red) but **cannot catch one that is LOOSER** —
the dangerous direction, where an instance we used to reject is now accepted,
silently, and the contract stops being enforced.

So both validators were run side by side over a generated corpus: every fixture
in `testdata/{valid,invalid,content}` plus, per fixture, seven mutations of each
top-level key (drop, →object, →number, →null, →bool, →array, →empty string) and
eight targeted cases (unknown property, bad `event_type`, four timestamp
mutations, fractional/huge integer).

```
corpus size: 1893 instances
LOOSER:   0
STRICTER: 0
```

**Then the harness was mutation-tested, because a differential test that cannot
fail is worse than none.** Deleting `Compiler.AssertFormat()` produced **79
LOOSER instances** — so the zero above is a real measurement, not a vacuous pass.

The harness was deleted with the walk it compared against; its result is this
section. Recreating it means re-adding `validator.go`, so the finding is recorded
rather than the tooling.

## `AssertFormat()` is the one setting that had to be right

In draft/2020-12 `format` is an **annotation by default** — jsonschema/v6 follows
the spec (`compiler.go:53`: "for draft/2020-12: disabled unless metaschema says
`format-assertion` vocabulary is required"). The retired walk *did* enforce
`date-time`. Without `AssertFormat()` the schema's `format: "date-time"`
constraints would parse, report as satisfied, and enforce nothing — a silent
loosening, which is exactly the failure mode the phase's security section names
("a validator that silently stops enforcing … looks exactly like a passing
suite"), just on a different keyword than expected.

`TestDateTimeFormatIsAsserted` pins it. Drill: remove the call ⇒ that test AND
the pre-existing `TestInvalidSamplesRejected` both go red.

## What changed

| File | Change |
|---|---|
| `structural.go` | NEW — `compileSchema`, `refusingLoader`, the two load-bearing compiler settings |
| `contentgate.go` | NEW — what survives of `validator.go`: the `validator` struct (root only), `resolve`, `hasGatedContent` |
| `validator.go` | **DELETED** (211 loc): `validate`, `validateObject`, `typeMatches`, `numeric`, `jsonEqual`, and the unused `contentEnabled` field |
| `conformance.go` | structural pass now `sch.Validate(inst)`; content-gate pass unchanged and re-commented to say why it stays separate |
| `deps_test.go` | `TestModuleStaysDependencyFree` → `TestDependenciesAreOnTheAllowlist` |
| `structural_test.go` | NEW — 5 tests (below) |
| `schema_guard_test.go` | keyword guard retargeted; two now-false comments corrected |
| `schema.go` | package doc corrected — it claimed "intentionally dependency-free … runs offline with no module downloads" |
| `go.mod` (conformance) | +1 direct (`jsonschema/v6 v6.0.3`), +1 indirect (`x/text v0.14.0`) |
| `go.mod` (claude-code, codex, client) | the two modules recorded as indirect |
| `cli/go.mod` | comment scoped: "This repo's ONLY external dependency" → "This module's only …" |

New tests, each drilled:

- `TestSchemaCompilesWithoutFetching` — and it proves the loader is *installed*,
  not merely that the happy path needs no fetch: a schema with an external `$ref`
  must fail with the refusal message. Drill: remove `UseLoader` ⇒ red.
- `TestDateTimeFormatIsAsserted` — drill above.
- `TestContentGateIsItsOwnPass` — a content-carrying, structurally-sound event
  returns `ErrContentDisabled` *alone*, and a structurally-broken one never
  reports as a content violation.
- `TestGatedFieldsAreReachableWithoutOneOf` — see below. Drill: inject a gated
  field under a `oneOf` branch in the real schema ⇒ red with the leak explained.
- `TestOneOfDiscriminatorSemantics` — a throwaway three-branch
  presence-discriminated `oneOf` (the shape phase 08 adds) gives
  exactly-one-branch semantics: 1 discriminator valid, 0 / 2 / 3 rejected.

## Two things the phase's file list missed

**1. `deps_test.go` asserted the module had ZERO dependencies.** Requirement 6
says the module "gains exactly one direct dependency", so the two collide head-on
and this test had to be edited — which sits awkwardly beside the repo's rule that
a test edited to pass is a signal. Handled as a deliberate, recorded reversal:
the assertion became an allowlist of exactly `jsonschema/v6` + `x/text`, `replace`
stays forbidden outright, and the comment states what was bought and what it
cost. Drilled both ways (unreviewed require ⇒ red; `replace` ⇒ red).

The cost is real and worth naming: `golang.org/x/text` is a genuine non-test
requirement of the validator, so it joins the test dependency graph of
`adapters/claude-code`, `adapters/codex` and `client` too, and a cold-cache
offline `go test` in those modules now needs a download. Phase 14's dependency
story already accepts "1 external → module-scoped set"; this is the first
instalment of it.

**2. `TestSchemaUsesOnlySupportedKeywords` had its rationale removed by the
swap** — it existed because the walk silently ignored unimplemented keywords. It
was retargeted rather than deleted, because the guard has a second job that
survives: `hasGatedContent` is still hand-rolled and **does not descend `oneOf`**.
Contract v1.6 adds three `oneOf` discriminator branches, so a gated field landing
inside one would be invisible to the gate and its content would egress with
capture OFF. `TestGatedFieldsAreReachableWithoutOneOf` is that half, made
explicit; the keyword list stays as a scope guard (some keywords need a compiler
setting to do anything, which is now a demonstrated rather than theoretical
concern).

## Evidence

| Check | Result |
|---|---|
| conformance module, `go test -v` | **13/13 PASS** |
| the 4 validator-exercising tests in 3 other modules | **4/4 PASS** |
| differential vs retired walk | 1893 instances, 0 disagreements; harness proven to detect 79 |
| mutation drills (AssertFormat, UseLoader, gated-under-oneOf, allowlist ×2) | **5/5 go red as intended** |
| `gofmt -l` | clean |
| `go vet` (12 modules) | clean |
| both cross-compiles | clean |
| per-module `GOWORK=off go build` | **12/12 ok** |
| full suite `-race` | no `DATA RACE`; same 6 sandbox-red modules as baseline |
| **whole-workspace verdict-set diff vs post-phase-01** | +6 new conformance tests (PASS), −1 replaced; **FAIL count unchanged at 19; nothing else moved** |

No existing conformance case was edited. `go test -skip` was used only to keep
the sandbox's listener panics from truncating packages, identically on both sides
of the comparison.

## Findings worth keeping

- **The hand-rolled walk had two spec deviations that never bit.** `resolve()`
  discarded siblings of `$ref` (draft 2020-12 allows them), and `jsonEqual`
  compared via `fmt.Sprintf("%v")`, so `1` and `1.0` — and `true` and `"true"` —
  compared equal. The library is correct on both. Neither changed a verdict on
  the 1893-instance corpus, because the contract never exercises either shape;
  they were latent, not active.
- **`additionalProperties` was honoured in boolean form only**, silently skipped
  as a schema object. The library handles both; the guard now keeps the contract
  to the boolean form as a readability choice rather than a capability limit.

## Unresolved questions

1. **`x/text` in three more modules — acceptable, or should the validator move to
   a separate module?** Phase 14's dependency story accepts it, so it is recorded
   rather than escalated. Flagging because the alternative (a
   `conformance/validate` submodule that the adapters do not import) is cheaper
   now than after phases 08–13 add more importers.
2. **Should `TestSchemaUsesOnlySupportedKeywords` survive phase 08?** Its keyword
   list will need `oneOf` branch keywords added. It is a scope guard now, so
   widening it is routine — but if it becomes pure friction, deleting it is
   defensible once `TestGatedFieldsAreReachableWithoutOneOf` covers the part that
   matters.
3. `jsonschema/v6` v6.0.3 is the latest published v6 (checked against the proxy),
   so D-OSS-5's version and D-GO-1's "latest, no pin" agree today. Re-check if a
   v6.0.4 appears — D-GO-1 says latest wins.
