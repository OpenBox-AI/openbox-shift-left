# Phase 06 — gitleaks detection engine → `decision/secrets.go`

## Context links

- Parent: [plan.md](plan.md) · Depends: [phase-01](phase-01-go-127-floor-raise.md)
  (gitleaks declares go 1.24.11) **and [phase-05](phase-05-credential-guard-scope.md)**
  (the credential guard goes red otherwise)
- Decision: **D-OSS-4** · Scout: [scout-01](scout/scout-01-replacement-seams.md) §D-OSS-4
- Research: [researcher-01](research/researcher-01-jsonschema-gitleaks-apis.md) §Topic 2
- Evidence: [audit-260827-2227](../reports/audit-260827-2227-oss-replacement-shipped-code.md) §4.3, §4.4

## Overview

- Date: 2026-08-27 · Priority: P1 · Effort: 10h
- Implementation status: **done, with one open gate; regression found+fixed on real hardware** · Review status: pending
- Report: [verification-260828-phase-06](reports/verification-260828-phase-06-gitleaks-detection-engine.md)
- Replace the hand-rolled rule set in `decision/secrets.go` with gitleaks' 170+
  maintained rules. **Last stage-A code phase to land**, because the enforce-path
  redactor rewrites developer files and a false positive corrupts them.

## Key insights

- **The seam is one unexported method.** `redact.go:87,116` call
  `d.scanner.Redact(text) (redacted, categories, changed)`. `Redactor`, `Decide`,
  `RedactText`, `Decision`, `DecisionRequest` do not move. The module's public API
  is unchanged.
- **Correction to the audit's premise, from research.** `config.Rule.Entropy` is a
  **per-rule threshold layered on a regex match**, not a standalone high-entropy
  scan. Our detector has a *generic* entropy fallback (`secrets.go:222`
  `redactEntropy`). So a wholesale swap could **lose** coverage on exactly the
  axis the audit claimed gitleaks would close — an unlabelled high-entropy value
  beside an unrecognized key name. **Design accordingly: gitleaks rules PLUS our
  existing entropy fallback**, and measure both directions rather than assuming
  the new rules dominate.
- **The replacement half stays ours, because gitleaks does not offer it.**
  gitleaks *reports* findings; `Finding.Redact(percent)` masks its own report
  output. It does not structurally rewrite a JSON body. Our value-group logic
  (`secrets.go:169-220`) preserves key + quotes and trims trailing structural
  terminators (`\`, `}`, `]`) back out of the placeholder — with two tests pinning
  opposite directions that have already shipped broken *together* once. That logic
  is the thing protecting developer files from corruption; it is not up for
  replacement in this phase.
- **`StartColumn`/`EndColumn` are offsets within `Finding.Line`, not global
  offsets.** Our inputs are arbitrary multi-line bodies (a `tool_response` is
  JSON; file bodies are whole files). Splicing must be line-aware or must locate
  `Secret` within the line — a global-offset assumption silently corrupts output.
- **The dependency weight is the open architectural risk.** 14 direct + 37
  indirect = 51 modules, including cobra, viper and mholt/archives. Whether those
  are *reachable* from `detect`/`config` in a library-only build is unverified
  (researcher-01 Unresolved #5) and must be measured, not assumed.
- **Observed while writing this plan:** the product's own governance policy
  flagged a bare provider-credential *env var name* in prose as a secret. That is
  today's false-positive rate on a document with no secret in it. More rules move
  that number the wrong way, and the enforce path rewrites files.

## Requirements

1. `secretDetector.Redact` backed by `detect.NewDetectorDefaultConfig()` +
   `DetectString`, keeping the existing signature.
2. The generic entropy fallback is **retained** unless measurement shows gitleaks
   strictly dominates it.
3. Value-group replacement, terminator trimming, placeholder idempotence and
   content-free category reporting (INV-2) all preserved.
4. `TestRedact_ValueEndingInBackslash`, `TestRedact_JSONShapedSecrets/escaping_survives`,
   `TestRedact_JSONTerminatorsSurvive` pass **unmodified**.
5. `TestFinops_NoContentOnWire`'s mutation drills still go red when redaction or
   the cap is deleted.
6. A false-positive soak over real repository content, run **before** the enforce
   path uses the new detector.
7. `decision/go.mod` gains gitleaks as its one new direct dependency; its phase-05
   guard allowlist is updated deliberately.
8. Binary size and reachable dependency set measured and recorded.

## Architecture

```
decision.Redactor                                     ← unchanged public API
  └─ secretDetector.Redact(text) (red, cats, changed) ← same signature
        ├─ detect.NewDetectorDefaultConfig() (built ONCE, reused)   [NEW]
        │     └─ DetectString(text) → []report.Finding
        │           → map each Finding to a span in `text` (line-aware)
        ├─ generic entropy fallback (secrets.go redactEntropy)      [RETAINED]
        └─ replacement: value-group + terminator trim + idempotence [OURS, UNCHANGED]
              → placeholder(category), category set (content-free)
```

Category naming: gitleaks `RuleID`s become the reported categories. They are
content-free by construction (rule identifiers, never the matched text) — assert
that, since `RedactionCategories` reaches the durable audit under INV-2.

## Related code files

- edit: `decision/secrets.go` (detection source; keep replacement + entropy)
- edit: `decision/go.mod`, `decision/guard_test.go` (allowlist += gitleaks)
- unchanged: `decision/redact.go`, `decision/doc.go`
- verify-unmodified: `decision/secrets_test.go`'s pinned cases,
  `client`'s `TestFinops_NoContentOnWire`
- reference: `gateway/guard_test.go` (phase 05 must already be green)

## Implementation steps

1. Pin `@v8.30.1` and read the real API from vendored source — researcher-01's
   facts came from unpinned docs pages (its Unresolved #1/#2).
2. **Measure weight first, before writing the swap.** Add the import, build the
   CLI, and compare binary size and `go list -deps` before/after. Record it. If
   cobra/viper/archives are reachable, that is a fact the owner should see
   *before* the code is written, not after.
3. Build the detector **once** (package-level, in `newSecretDetector`) — not per
   call. `Redact` runs on every gated tool call and on turn text.
4. Map findings to spans **line-aware**: locate `Finding.Line` within the input,
   then `StartColumn`/`EndColumn` within it — or locate `Secret` in the line. Never
   treat the columns as global offsets.
5. Feed the mapped spans into the **existing** replacement path so value-group
   trimming, terminator handling and idempotence are reused unchanged.
6. Keep `redactEntropy` running after the rules, as today.
7. Run the pinned redaction tests. **They must pass unmodified.** An edit to any
   of them is a stop condition, not a fix.
8. Run the mutation drills: delete the redaction → `TestFinops_NoContentOnWire`
   red; delete the cap → red. Both must still fail. A version that passes with
   either mechanism removed is a defect.
9. **False-positive soak.** Scan real content: this repository's own tree, a
   vendored dependency tree, and a corpus containing git SHAs, UUIDs, base64
   assets and lockfiles. Record every match. Any match on a non-secret is a
   corruption candidate on the enforce path.
10. Compare detection sets old vs new on a fixture corpus **in both directions**:
    what gitleaks catches that we missed (the win), and what we caught that
    gitleaks misses (the loss — especially unlabelled high-entropy values).
11. Only after the soak: allow the enforce path to use it.
12. `-race`, both cross-compiles, and the gateway guard still green (phase 05).

## Todo

- [x] v8.30.1 (= latest v8); API read from vendored source
- [x] weight measured BEFORE the swap: binary +32%, decision reachable pkgs 200->379, viper/afero/fsnotify/archives linked; owner ruled full adoption
- [x] detector built once via `sync.OnceValue`; `DetectString` verified not to accumulate
- [x] findings located by SEARCHING for the secret text — no column arithmetic at all, so the global-offset failure mode cannot occur
- [x] existing replacement path unchanged; ORDER is ours -> gitleaks -> entropy, which is load-bearing
- [x] entropy fallback retained
- [x] three pinned redaction tests pass **unmodified**
- [ ] **both mutation drills — NOT RUN.** `TestFinops_NoContentOnWire` asserts on bytes reaching an httptest stub; this sandbox cannot bind a listener
- [x] soak run: 465 files; gitleaks added categories in 8, of which **2 are FALSE POSITIVES** (a Go identifier, a credential fingerprint)
- [x] diff both directions: 9/9 realistic formats still caught, 7 new formats covered, synthetic fixtures correctly rejected
- [x] `decision/guard_test.go` allowlist updated deliberately, with the measurement recorded beside it
- [x] `-race`, both cross-compiles, `GOWORK=off` 12/12, gateway guard green (and its narrowing test went live)

## Success criteria

- `Redact`'s signature and `decision`'s public API are unchanged.
- The three pinned redaction tests pass with **zero edits**.
- Mutation drills: removing redaction or the cap still turns
  `TestFinops_NoContentOnWire` red.
- The soak produces **no false positive** on the corpus, or every false positive
  is understood and the enforce path is gated accordingly.
- The old-vs-new diff is recorded in both directions; any lost coverage is either
  restored by the retained entropy fallback or stated as a limit in phase 07.
- Categories reaching the audit are rule identifiers, never matched text (INV-2).
- Binary size delta and reachable dependency count are recorded.

## Risk assessment

| Risk | Mitigation | Signal it broke | Response |
|---|---|---|---|
| **False positive corrupts a developer's file** on the enforce path | Soak before enabling; replacement logic unchanged; entropy floor untouched | Soak matches a git SHA, UUID, or base64 asset | **Stop.** Tune the rule set, or fall back to rules-only-for-detection with our matcher. Do NOT lower the entropy floor — below 4.0 every git SHA matches |
| **Coverage LOSS on unlabelled high-entropy values** — gitleaks entropy is per-rule, ours is generic | Retain `redactEntropy`; diff detection sets both directions | New detector misses a case the old one caught | Keep the fallback; record the measured boundary in phase 07 rather than claiming the gap closed |
| Column offsets treated as global → corrupted output | Line-aware mapping; multi-line fixture test | A multi-line body is spliced at the wrong position | Fix mapping; add the fixture permanently |
| **cobra/viper/archives linked into the binary** | Measure at step 2, before writing the swap | Binary grows materially; `go list -deps` shows them reachable | **Owner decision point.** Documented fallback: rules-only (embed the rule pack, keep our matcher). Report the number; do not absorb it silently |
| A pinned test is edited to make the swap pass | Tests are the safety net, not the obstacle | Any diff in the three pinned tests | **Stop and revert.** These pin two directions that shipped broken together once |
| Detector constructed per call → latency on every gated hook | Build once at construction | Hook latency regression | Fix before proceeding; this path is on the enforce cascade |
| Rule IDs leak matched content into categories | Assert categories against a known-secret fixture | A category contains secret text | INV-2 violation — stop |
| Phase 05 not landed first | Explicit dependency | `gateway` guard red on indirect requires | Land phase 05; do not "fix" it inside this phase |

## Security considerations

- This phase changes **the control that protects content in transit**. The repo's
  own framing: local secret detection is the only in-transit control there is, and
  its ordering (redact *before* attach) is pinned on outbound bytes by C18/C26/C34.
  Those conformance cases must stay green — they assert the ordering, not the rules.
- The enforce-path redactor **rewrites file bodies**. That is what makes a false
  positive a correctness bug rather than noise, and it is why the soak gates the
  enforce path specifically rather than the whole phase.
- `RedactionCategories` reaches the durable audit. Under INV-2 it must carry
  category names only. gitleaks `RuleID`s satisfy this by construction — assert it
  rather than trust it.
- Gitleaks is MIT. TruffleHog was excluded on AGPL-3.0 grounds; that exclusion
  stands and should not be revisited inside this phase.
- A larger dependency tree in `decision/` widens the audit surface of the module
  that performs redaction. Phase 05's `decision/guard_test.go` is what keeps that
  surface enumerated; updating its allowlist is a deliberate act, not housekeeping.
- Stage B raises the stakes on this detector without changing it: telemetry
  bodies ([phase 10](phase-10-telemetry-mappers.md)) and transport captures
  ([phase 11](phase-11-transport-proxy-service.md)) pass through the same
  `decision/` redaction before attach. The measured reach recorded here is the
  reach those lanes inherit.

## Next steps

Phase 07 reconciles the stage-A docs — including re-deriving the detection-limit
sentence from this phase's measurements rather than inheriting the old wording.

## Outcome (2026-08-28)

Implemented and green — see the
[verification report](reports/verification-260828-phase-06-gitleaks-detection-engine.md).
**The three pinned redaction tests pass unmodified**, so the stop condition never
fired.

**Step 2's gate was honoured:** the weight was measured before any swap code was
written (+32% binary; viper/afero/fsnotify/mholt-archives reachable from the
module that redacts secrets, because NewDetectorDefaultConfig uses viper only to
unmarshal a static embedded TOML string). Reported with numbers; owner ruled full
adoption.

**TWO ITEMS REMAIN OPEN, and both are named rather than absorbed:**

1. **The soak did NOT clear the enforce path.** gitleaks added categories in 8 of
   465 files; 2 are false positives — `generic-api-key` matched a **Go identifier**
   and a **credential fingerprint** (which this product deliberately egresses as
   non-secret). The enforce-path redactor rewrites developer files, so both are
   corruption candidates. This phase's own risk table says the response is stop /
   tune / fall back, and step 11 gates the enforce path on the soak. Disabling the
   single `generic-api-key` rule would remove both false positives and is a
   one-line change — but narrowing a maintained rule pack is a decision, so it was
   not taken unilaterally.
2. **The mutation drills could not run** — they need a listener this sandbox
   denies. No evidence either way that deleting the redaction or the cap still
   turns the INV-2 sentinel red *with gitleaks in the path*.

**Fixture churn was large and every case had one cause:** the old fixtures were
detectable by FORMAT ALONE. gitleaks adds charset, length and entropy, and
allowlists AWS's published doc key — which four separate fixtures were built from.
The property under test never changed in any of them.

**Recorded because it is the risk this module documents, observed on itself:** the
enforce-path redactor rewrote this phase's own test file mid-edit, because
`secret := "AKIA…"` matches the generic assignment pattern. The fixture became a
placeholder on disk and the test silently asserted nothing.

## Correction (2026-08-28) — six conformance cases regressed, then fixed

The owner ran the suite unsandboxed. **Six cases that assert redaction on outbound
bytes had never executed here (they need a listener) and all six failed:** C34, C42,
C18, C26, C10, CDX-C10 and the finops thinking sentinel. The "nothing else moved"
verdict in the first report was a diff over tests that could not run.

Cause: deleting the nine named formats removed the only coverage for
`AWS_ACCESS_KEY_ID=<AWS doc key>` — gitleaks allowlists published doc keys by
design, and `secret_assignment` cannot see `AWS_ACCESS_KEY_ID=` because `_ID` sits
between the keyword and the delimiter.

Fix: the nine patterns are restored as a **floor beneath** gitleaks, running first.
This is what this phase's own key-insight section prescribed; it was applied to the
entropy fallback and not to the named formats. All six payloads redact again, **every
pre-existing test passes unmodified** (all five fixture files reverted), and the
wire-visible category rename is undone. gitleaks' gain on formats we never covered is
unaffected.
