# `contracts/dev-event` — normalized developer-runtime event contract

**Story:** STORY-SL-1 · **Version:** 1.0 · **Status:** built + validated; G1_READY + G3_REVIEW approved (2026-07-07)

The single, versioned, **tool-agnostic** event schema that every coding-tool adapter
maps its native payload onto (SPI `emit()`), and that the OpenBox client emits verbatim
onto openbox-core. Adding a provider (Claude Code, Codex, Cursor, …) never changes the
core or the wire format (PRD **FR-4**, architecture **§1b**).

## Layout

| Path | What |
|---|---|
| [`schema/dev-event.schema.json`](schema/dev-event.schema.json) | The contract — JSON Schema (draft 2020-12), language-neutral. 7 lifecycle event types, common envelope, `tool{}`, `span`, gated `content`, canonical `verdict` enum. |
| [`MAPPING.md`](MAPPING.md) | How the contract maps onto openbox-core's `GovernanceEventPayload` / `SpanData` (verified S6 + live code). SL-3 builds payloads from this without guessing. Includes the EXT-core session-lifecycle dependency, the verified downstream-consumer sweep (INV-8), and client signing/transport notes. |
| [`COVERAGE.md`](COVERAGE.md) | How Claude Code / Cursor / Codex real event surfaces map onto the 7 types, field-derivation rules, and the bounded Phase-1 non-goals (v1.1 candidates). The reference for adapter authors (SL-4/7/8). |
| [`conformance/`](conformance/) | Go conformance harness (OD17). Dependency-free; validates samples against the schema and enforces the INV-2 content gate. |

## The 7 lifecycle event types

`SessionStarted` · `PromptSubmitted` · `ToolCall` · `ToolResult` · `SessionEnded` ·
`CommitCreated` · `Deploy`

These are the **lifecycle** axis (new, added additively to core by EXT-core). They carry
core's **existing** **semantic-span** types (`file_write`, `mcp_tool_call`, `llm_completion`, …).
See MAPPING.md §2 for the two axes.

## Privacy (INV-2 / OD4)

Metadata-only by default. Raw content lives **only** under the `content` object (and span
`request_body`/`response_body`), which are **absent by default** and Guardrail-redacted at
source when an org opts in. The conformance harness **rejects any event carrying content
while content-capture is disabled**.

## Verdict vocabulary

Canonical (priority): `HALT > BLOCK > REQUIRE_APPROVAL > CONSTRAIN > ALLOW`. openbox-core
serializes the response `verdict` field as lowercase (`halt|block|require_approval|constrain|allow`)
plus a legacy `action` field — see `$defs.verdict` and MAPPING.md §4. Phase-1 observe treats
every verdict as allow (INV-3).

## Validate

```bash
cd conformance && go build ./... && go vet ./... && go test ./...
```

The harness is intentionally offline/zero-dependency, so this runs anywhere with a Go
toolchain and no module downloads.

## Consuming the contract

- **SL-3 (client):** build `GovernanceEventPayload` per MAPPING.md; `import` the `conformance`
  package to validate outbound events before signing/POST.
- **Adapters (SL-4/7/8):** map native tool payloads onto this schema in `emit()`.
- **EXT-core (assumed-satisfied):** add the 7 lifecycle strings to `isValidGovernanceEventType`
  (3 additive edits, S6 §3) so events are *accepted* end-to-end. Authored independently here.
