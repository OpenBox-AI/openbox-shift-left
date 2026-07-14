# STORY-E6-S4 — Guardrail redaction application (the `updatedInput` leg)

**Epic:** E6 (enforcement — apply the guardrail-redacted input). **Risk:** high (this is the FIRST path that (a) sends tool **content** anywhere — the local sidecar — and (b) rewrites a tool's input before it runs; a bug either leaks content across INV-2 or corrupts a tool call). **Status target:** review (build + validations + both reviews, pending brian G3 + Sam G_SEC).

## Source
- **Backlog:** `.fab7/sdlc/stories/E6-backlog.md` §E6-S4 — "apply `Evaluation.Guardrail` redacted input to the tool input before it runs (via `updatedInput`) — port the SDK's `_apply_input_redaction`. **Only meaningful when content-capture is on** (OD4); reuse the existing Guardrail API (do not build new — S4 §4)." Sequencing note: "redaction — **must be LOCAL in the sidecar, if content on**." Write scope: `adapters/claude-code/`. Deps: E6-S1, content-capture posture (OD4). Gates: G3, **G_SEC**. Invariants: **INV-2**, INV-3b.
- **OD4 (DECIDED 2026-07-07, spike S4):** metadata-only default; content **strictly opt-in per org**; when enabled, Guardrail-redacted at-source. So E6-S4 is NOT blocked on a human privacy decision — it builds the redaction-apply path that is active **only** under the existing content-capture opt-in (`DevConfig.ContentCapture` / `OPENBOX_CONTENT_CAPTURE`, `creds.go`). With content-capture OFF (the default), E6-S4 is fully inert.
- **E6-S1/S2/S3 (DONE, committed):** the PreToolUse enforce gate OBTAINS a `sidecar.Decision` (bounded, fail-open), applies the failure policy, then APPLIES the verdict — `mapVerdict` (SDK `enforce_verdict` cascade) → `applyDecision` writes a CC `permissionDecision`. E6-S2's rule: **governance only TIGHTENS**; only `deny`/`ask` ever hit stdout; `allow` is never emitted; a proceed verdict writes nothing (byte-identical to observe).

## Cross-repo recon (openbox-temporal-sdk-python — Explore, 2026-07-14)
- **`_apply_input_redaction`** (`openbox/activity_interceptor.py:441-478`): reads `verdict.guardrails_result.redacted_input`, **gated on `guardrails_result.input_type == "activity_input"`**; a `dict` → one-element list, non-list → warn + skip; replaces each positional arg with the redacted item; returns the re-serialized args. It is applied **strictly AFTER `enforce_verdict` returns without raising** (`activity_interceptor.py:258-262`) — i.e. on the **ALLOW/CONSTRAIN proceed path, immediately before the activity runs** — NOT on a blocked (HALT/BLOCK/guardrail-fail) or HITL-pending path (those raise first).
- **Carrier field:** `redacted_input: Any` on `GuardrailsCheckResult` (`types.py:104-123`), disambiguated by `input_type` ("activity_input" | "activity_output"). **There is NO `severity` field** on the guardrail type. Wire keys are identical (`guardrails_result.redacted_input`, `.input_type`, `.validation_passed`, `.reasons`).
- **Not gated on any `content_capture` flag in the SDK** (grepped) — the SDK always sends the full `activity_input` to its gate. In **shift-left**, tool content reaches the sidecar ONLY under the OD4 content-capture opt-in, so redaction is **naturally gated on content-capture** here.
- **CC contract** (code.claude.com/docs/en/hooks.md, confirmed): `updatedInput` sits under `hookSpecificOutput` alongside `permissionDecision`; it is a **FULL replacement** of `tool_input` (not a merge — missing fields are dropped); may be emitted **with or without** `permissionDecision`; requires **exit 0**. Emitting `updatedInput` alone rewrites the input and lets CC's normal permission flow proceed.

## The port (map `_apply_input_redaction` → CC `updatedInput`)
Redaction is applied on the **proceed path only** — after `mapVerdict` yields no `deny`/`ask` — exactly mirroring the SDK (redaction runs only when the enforce cascade passed without raising). Concretely, in `applyDecision`:

```
decision, reason := mapVerdict(e)         // E6-S2 cascade (unchanged)
if decision != "" {                        // deny/ask → emit permissionDecision, DO NOT redact
    emit {permissionDecision, reason}; return
}
                                            // proceed path (ALLOW/CONSTRAIN/UNKNOWN):
if contentCapture && redacted present && differs {
    emit {updatedInput: redacted}          // E6-S4: rewrite tool_input, NO permissionDecision
}                                           // else: nothing (byte-identical to E6-S3)
```

- **SDK arg → CC tool_input:** the SDK replaces `activity_input` args; the CC analog is a full `tool_input` object replacement via `hookSpecificOutput.updatedInput` (`json.RawMessage`, emitted verbatim).
- **Proceed-path only** (faithful): on `deny` the tool never runs; on `ask` the SDK raises pending BEFORE redaction, so we do not rewrite either (deferred consideration below). Redaction rewrites only when the tool is going to run under CC's own flow.

## The carrier (INV-2-confined: LOCAL sidecar protocol, never `client.Evaluation`)
The redacted `tool_input` is carried on **`sidecar.DecisionResponse.RedactedInput`** → **`sidecar.Decision.RedactedInput`** (`json.RawMessage`), NOT on `client.Evaluation`/`GuardrailResult`. This is deliberate and load-bearing for INV-2:
- `client.Evaluation` flows into the **advisory sink** (SL-9), the **enforcement audit** (E6-S2), and **core egress** (observe). Putting redacted content there would risk leaking it into all three. `client.GuardrailResult` **deliberately drops `redacted_input`** (SL-9 INV-2) — E6-S4 **preserves that decision byte-for-byte**.
- On the sidecar `Decision`, the redacted content stays confined to the **local Unix socket ↔ hook ↔ CC stdout** channel — same machine, never egressed, never logged, never in either JSONL sink. Same posture as the local-only `command` axis (E6-S1) and the `updatedInput`/reason strings (E6-S2).

## The redaction SOURCE gap — `[EXT-guardrail-redaction]` (honest, like `[EXT-opa-bundle]`)
The local `bundleEvaluator` is **metadata-only** (matches rules → verdict). It has **no guardrail/redaction engine**, so it produces **no `redacted_input`** — S4 §4 says reuse OpenBox's **server-side** Guardrail API (PII/NSFW/regex/Qwen), do NOT build a new model; but the enforce decision is LOCAL (S2: `/evaluate` ~1.6 s NO-GO). So today **nothing populates `Decision.RedactedInput` in the field** → the apply is a faithful, fully-tested **SEAM that is INERT until the engine lands** — exactly the E6-S5 `[EXT-opa-bundle]` pattern (built the Evaluator seam + faithful local default; flagged the real signed-bundle distribution). Flagged `[EXT-guardrail-redaction]`: a redaction-capable local evaluator (embed the Guardrail API, or a content-on `/evaluate` mirror that returns `redacted_input`) — out of adapter scope. The protocol-shape reconciliation (the request's `Content{Prompt,Output,FileText}` vs a redacted `tool_input` OBJECT) belongs with that engine work / E6-S7.

## Scope boundary (what this story IS and is NOT)
- **IS:** `applyInputRedaction` — the proceed-path `updatedInput` emitter (port of `_apply_input_redaction`), gated on content-capture, rewrite-only-when-present-and-different; the `updatedInput` field on the PreToolUse output struct; the `RedactedInput` carrier on `sidecar.DecisionResponse` + `sidecar.Decision` (+ Client copy); `ResolveContentCapture()` (cheap hot-path resolver); populate `DecisionRequest.Content` (gated, LOCAL-only file body) so a future engine has content to redact; `HookEvent.fileText()`; thread content-capture into the PreToolUse enforce branch; tests + a content-capture-on E2E.
- **IS NOT:** the verdict cascade / deny-ask writer (E6-S2, unchanged); the failure policy (E6-S3, unchanged); a **local redaction/guardrail engine** (`[EXT-guardrail-redaction]`); parsing `redacted_input` into `client.Evaluation`/`GuardrailResult` (would cross INV-2 into the advisory/audit/egress paths — explicitly NOT done); **output** redaction (`updatedToolOutput`, PostToolUse — the SDK's `_apply_output_redaction`, deferred); ask-path redaction (below). NO core/backend surface.

## Acceptance Criteria
1. **Faithful proceed-path apply** — on the enforce PreToolUse gate, when `mapVerdict` yields proceed (no deny/ask) AND content-capture is on AND `dec.RedactedInput` is present, non-empty, valid JSON, and DIFFERS from the original `tool_input`, `applyDecision` emits `hookSpecificOutput.updatedInput` (the redacted object, verbatim) with NO `permissionDecision`, exit 0. Mirrors `_apply_input_redaction` running only after the enforce cascade passes.
2. **Content-capture gate (OD4 / INV-2)** — with content-capture OFF (the default), `applyInputRedaction` emits nothing and `buildDecisionRequest` sets `Content` nil → the whole PreToolUse path is **byte-identical to E6-S3**. A test asserts inert-when-off for both legs.
3. **Never on deny/ask; never loosens** — a HALT/BLOCK/guardrail-fail still emits `deny`; REQUIRE_APPROVAL still emits `ask`; neither emits `updatedInput`. `permissionDecision: "allow"` is NEVER emitted (tighten-only preserved). `updatedInput` only ever STRIPS content (a rewrite to the sanitized input).
4. **Carrier is LOCAL-only (INV-2)** — `RedactedInput` lives on `sidecar.Decision`/`DecisionResponse`, never on `client.Evaluation`; `client.GuardrailResult` is unchanged (still drops `redacted_input`). Tests assert the enforcement audit (`enforcements.jsonl`), the advisory sink, and the emitted /evaluate payload NEVER contain the redacted content.
5. **Request content is LOCAL-only + gated** — `buildDecisionRequest` populates `Content` (file body for Write/Edit) ONLY when content-capture is on; it goes ONLY to the local socket and is NEVER egressed (the observe Mapper path is unchanged, still metadata-only). A test asserts Content nil when off, populated when on, and that no content reaches the egress payload either way.
6. **No-op when no redaction / unchanged input** — a present-but-equal `RedactedInput`, an empty one, or invalid JSON → emit nothing (never rewrite to identical/garbage input). Faithful to the SDK's non-dict/non-list warn-and-skip.
7. **`[EXT-guardrail-redaction]` flagged** — a doc/comment records that the local evaluator produces no `redacted_input` today, so the apply is inert in the field until a redaction-capable evaluator or content-on `/evaluate` mirror lands (carry the protocol-shape reconciliation to E6-S7 / that work).

## Write Scope
- `adapters/claude-code/enforce.go` — `applyInputRedaction`; `updatedInput` on `hookSpecificOutput`; `applyDecision` gains `contentCapture bool` + the redacted `tool_input`; `buildDecisionRequest` gains content gating; header doc; `[EXT-guardrail-redaction]`.
- `adapters/claude-code/hookevent.go` — `fileText()` local-only extractor (Write `content` / Edit `new_string`).
- `adapters/claude-code/creds.go` — `ResolveContentCapture()` (cheap config+env, mirrors `ResolveEnforce`).
- `adapters/claude-code/hookrun.go` — thread `ResolveContentCapture()` into the PreToolUse enforce branch (pass to `buildDecisionRequest`/`applyDecision`).
- `sidecar/protocol.go` — `DecisionResponse.RedactedInput json.RawMessage` (LOCAL-only doc).
- `sidecar/client.go` — `Decision.RedactedInput`; copy `resp.RedactedInput` in `Decide`.
- `adapters/claude-code/enforce_test.go`, `hookevent_test.go`, `creds_test.go`, `sidecar/*_test.go` — the ACs above.

## Invariants
- **INV-2 (load-bearing):** redacted content and request Content are LOCAL-only (Unix socket ↔ hook ↔ CC stdout), gated on the OD4 content-capture opt-in, never egressed / logged / in either JSONL sink; `client.Evaluation` never carries redacted content.
- **INV-3b:** redaction is applied pre-execution on the proceed path; it adds no new blocking and no unbounded wait (it reads an already-obtained `Decision`).
- **Tighten-only:** stdout carries only `deny`/`ask` OR a content-STRIPPING `updatedInput`; never `permissionDecision: allow`.

## Human Gates
| Gate | Question | Owner | Outcomes |
|---|---|---|---|
| G3_REVIEW | Does the apply faithfully port `_apply_input_redaction` (proceed-path only, full tool_input replacement via `updatedInput`), gated on content-capture, with the source gap honestly flagged? | brian | approve / revise |
| G_SEC | Is redacted content strictly LOCAL (never on `client.Evaluation`, never egressed/audited), the content-capture gate airtight (inert when off = byte-identical to E6-S3), and does the rewrite never loosen or corrupt a tool call? | Sam | approve / revise / block |

## Validation
```bash
cd adapters/claude-code && go build ./... && go vet ./... && go test -race ./...
cd ../../sidecar && go build ./... && go vet ./... && go test -race ./...
cd ../cli && go build ./... && go vet ./... && go test ./...
# Live: enforce on + content_capture on + `openbox sidecar serve` with a bundle that
#   returns a RedactedInput for a Write → stdout carries updatedInput (redacted), no permissionDecision.
# Live: enforce on + content_capture OFF → byte-identical to E6-S3 (no updatedInput, Content nil).
# Assert: enforcements.jsonl + advisories.jsonl + the /evaluate payload contain NO redacted content.
```

## Stop conditions
- If redacted content (or request Content) ever appears on `client.Evaluation`, in `enforcements.jsonl`/`advisories.jsonl`, or in an egressed /evaluate payload → STOP (INV-2).
- If `applyInputRedaction` emits anything when content-capture is OFF → STOP (the OD4 default must be byte-identical to E6-S3).
- If a redaction ever emits `permissionDecision: allow`, or rewrites to an empty/identical/invalid input → STOP.
- If this story modifies `mapVerdict`/`applyFailurePolicy`/the `sidecar.Client` fail-open primitive, or parses `redacted_input` into `client.GuardrailResult` → STOP (out of scope; E6-S4 is a proceed-path apply layered on top).
- If the build introduces a local guardrail/redaction engine → STOP (that is `[EXT-guardrail-redaction]`, not this story).
