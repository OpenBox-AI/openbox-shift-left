# Phase 02 — JSON Schema validator → santhosh-tekuri/jsonschema/v6

## Context links

- Parent: [plan.md](plan.md) · Depends: **none** — the library declares a Go
  floor well under the current one, so this phase may run before or beside
  [phase-01](phase-01-go-127-floor-raise.md)
- Decision: **D-OSS-5** · Scout: [scout-01](scout/scout-01-replacement-seams.md) §D-OSS-5
- Research: [researcher-01](research/researcher-01-jsonschema-gitleaks-apis.md) §Topic 1

## Overview

- Date: 2026-08-27 · Priority: P2 · Effort: 5h
- Implementation status: **done** · Review status: pending
- Report: [verification-260827-phase-02](reports/verification-260827-phase-02-jsonschema-validator.md)
- Replace the 211-line hand-rolled draft-2020-12 subset in
  `contracts/dev-event/conformance/validator.go` with the reference library.
  Highest correctness value in the plan, lowest blast radius.

## Key insights

- **Blast radius is tests only.** The module's entire public API is
  `ValidateDevEvent(raw []byte, contentCaptureEnabled bool) error` and
  `LoadSchema()`, and the only importers are `adapters/claude-code/conformance_test.go`,
  `adapters/codex/conformance_test.go`, `client/acceptancetest/vocabulary_test.go`.
  No production code path runs this. A regression here breaks CI, not a developer.
- **The `x-content-gated` pass probably needs no custom vocabulary.**
  `validator.go:147-154` already walks the raw `map[string]any` schema as a
  **separate pass**, independent of structural validation — deliberately, so a
  posture violation is never conflated with a structural mismatch during `oneOf`
  branch trials (`validator.go:38`). That walk keeps working untouched.
  **Preferred design: keep the content-gate walk, delete only the structural
  validator.** `RegisterVocabulary` is the fallback if the walk turns out to need
  compiled-schema context, not the default plan.
- In-memory compile is a direct swap: `json.Unmarshal` → `Compiler.AddResource(url, doc)`
  → `Compiler.Compile(url)`. `AddResource` pins the URL to the in-memory document,
  so the compiler never fetches anything over the network — assert that.
- `$ref: "#/$defs/…"` and `oneOf` branch trials are core spec features, not
  extensions. The hand-rolled `resolve()` (validator.go:20-36) handled only local
  `$defs` refs; the library handles the whole draft. **This is the correctness
  gain** — and it matters now, because [phase 08](phase-08-contract-decision.md)'s
  contract bump adds `oneOf` discriminator branches (`otel_request_id` /
  `proxy_request_id` / `gateway_request_id`) that the hand-rolled trial logic has
  never been stressed on.
- Error quality is a deliberate choice: start with `ValidationError.Error()`;
  reach for `DetailedOutput()` only if conformance failures become unreadable.

## Requirements

1. Structural validation performed by `santhosh-tekuri/jsonschema/v6`.
2. `ValidateDevEvent` and `LoadSchema` signatures unchanged.
3. The `x-content-gated` / INV-2 content-gate pass stays a **separate pass** with
   identical semantics.
4. Every existing conformance case (C1–C41) passes unmodified.
5. The compiler is proven not to perform network or filesystem fetches.
6. `contracts/dev-event/conformance/go.mod` gains exactly one direct dependency.

## Architecture

```
ValidateDevEvent(raw, contentCaptureEnabled)          ← signature unchanged
  ├─ structural  : jsonschema/v6  (NEW — replaces validator.go's walk)
  │     LoadSchema() → map[string]any
  │       → Compiler.AddResource("mem://dev-event.schema.json", doc)
  │       → Compiler.Compile(...) → *Schema → Schema.Validate(instance)
  └─ content gate: existing separate walk over the raw schema map  (UNCHANGED)
        reads schema["x-content-gated"], enforces INV-2 against contentEnabled
```

The two passes stay separate and stay in this order, for the reason
`validator.go:38` already records.

## Related code files

- delete: the structural half of `contracts/dev-event/conformance/validator.go`
  (the `validate`/`resolve` walk, ~211 loc)
- keep: the content-gate walk (`validator.go:147-154` and its helpers)
- edit: `contracts/dev-event/conformance/conformance.go:21-32` (wire the compiler)
- edit: `contracts/dev-event/conformance/go.mod`
- unchanged: `contracts/dev-event/conformance/schema.go` (`LoadSchema`)
- verify-only: `adapters/claude-code/conformance_test.go`,
  `adapters/codex/conformance_test.go`, `client/acceptancetest/vocabulary_test.go`

## Implementation steps

1. Pin the docs to the real version: `go get github.com/santhosh-tekuri/jsonschema/v6@v6.0.3`
   and read the API off the vendored source, not a docs summary
   (researcher-01 Unresolved #1 — its API facts came from unpinned pages).
2. Wire `AddResource` + `Compile` in `conformance.go`. Keep `LoadSchema` as the
   single source of the schema document.
3. Run the full conformance suite. **Do not edit a single case.** Every failure is
   information: either the hand-rolled validator was wrong (a win — record it in
   the phase report) or the wiring is wrong.
4. Delete the structural walk only after the suite is green.
5. Confirm the content-gate pass still runs independently: add a case where the
   instance is structurally invalid **and** content-gated, and assert the reported
   error names the content violation as its own finding, not folded into a
   `oneOf` branch failure.
6. Assert no network/filesystem access: run the suite with a compiler whose
   loader would fail if invoked (or with network disabled) and confirm green.
7. Check the library's own go floor and add its module to `go.mod` as the one new
   direct dependency.
8. `go test ./...` in every module that imports conformance, then `-race`.

## Todo

- [x] v6.0.3 (= latest v6); API read from the vendored source in the module cache
- [x] `AddResource` + `Compile` wired via `structural.go`'s `compileSchema`
- [x] module suite 13/13 green + the 4 validator-exercising tests in 3 other modules, **zero case edits**
- [x] differential: 1893 instances, 0 disagreements; 2 latent spec deviations in the retired walk recorded
- [x] structural walk deleted (211 loc) after green; content-gate walk moved to `contentgate.go`
- [x] content-gate pass proven independent (`TestContentGateIsItsOwnPass`)
- [x] no-fetch assertion — and it proves the loader is INSTALLED (external `$ref` must refuse)
- [x] `-race` + both cross-compiles + per-module `GOWORK=off` (12/12)

## Success criteria

- C1–C41 pass unmodified.
- The `oneOf` discriminator cases [phase 08](phase-08-contract-decision.md)
  will add can be expressed and validated — sanity-check with a throwaway
  two-branch `oneOf`.
- A structurally-invalid **and** content-gated instance reports the content
  violation distinctly.
- `validator.go`'s structural walk is gone; the content-gate walk survives.
- Exactly one new direct dependency in that module.

## Risk assessment

| Risk | Mitigation | Signal it broke | Response |
|---|---|---|---|
| **The library is stricter and an existing case fails** | Run the suite before deleting anything | A conformance case goes red | **Investigate, do not re-bless.** Most likely the hand-rolled validator was permissive — that is a found bug and belongs in the phase report |
| The library is *looser* somewhere (silently accepts what we rejected) | Add a deliberately-invalid instance per structural rule the old validator enforced | An invalid instance passes | Add the missing constraint to the schema itself — the schema, not the validator, should carry it |
| Content-gate semantics fold into structural errors | Keep the passes separate; test the combined case | The combined case reports only a structural error | Restore the separation; this is the one design property to preserve |
| `RegisterVocabulary` turns out to be required and its failure-reporting API is unclear (researcher-01 Unresolved #3) | Preferred design avoids it entirely | The gate walk needs compiled context | Fall back to the vocabulary; budget +2h and confirm the `ValidatorContext` API from source |
| Compiler fetches a remote `$schema` meta-schema | `AddResource` pins the doc; assert no-fetch | Test hangs or fails offline | Register the meta-schema in-memory too |
| Researcher-01's API facts drift from v6.0.3 | Step 1 verifies against vendored source | Compile errors on the sketched API | Expected; read the real source |

## Security considerations

- This module is **test-only**, so a defect here cannot leak content at runtime.
  It can, however, make the conformance suite *stop proving* what it claims —
  which is how a content-gate regression would reach production unnoticed. The
  INV-2 gate cases are the ones to watch: a validator that silently stops
  enforcing `x-content-gated` looks exactly like a passing suite.
- Do not let the compiler resolve remote schemas. A conformance run that reaches
  the network can be influenced off-host, and CI would be the place it happened.
- `LoadSchema` stays the only source of the schema document — one source, so the
  gate and the structure cannot disagree about which schema they enforced.

## Next steps

Phase 03 swaps the config parsers. Phase 05 must precede phase 06. Landing this
phase before [phase 08](phase-08-contract-decision.md) is strongly
recommended, so the contract's new `oneOf` branches are validated by the library
from their first commit.

## Outcome (2026-08-27/28)

Done — see the
[verification report](reports/verification-260827-phase-02-jsonschema-validator.md).

**The preferred design held:** the content-gate walk was kept and only the
structural half deleted. `RegisterVocabulary` was not needed.

**The acceptance criterion needed re-reading, not a relaxed sandbox.** C1-C41 do
not import this module at all — they are a whole-suite tripwire insensitive to a
validator swap. The swap's real consumer surface is four tests in three modules,
none of which needs a listener, and all four pass here.

**Two items the phase's file list missed**, both handled and recorded:
`deps_test.go` asserted ZERO dependencies (collides head-on with requirement 6,
so it became an allowlist), and `TestSchemaUsesOnlySupportedKeywords` lost its
rationale to the swap but keeps a second job — `hasGatedContent` still does not
descend `oneOf`, which phase 08's three new branches make urgent.

**One setting was the whole risk:** draft/2020-12 makes `format` an annotation by
default, so without `Compiler.AssertFormat()` the schema's `date-time`
constraints would have stopped being enforced silently. Removing it turns 79
corpus instances green, which is how that was measured rather than assumed.
