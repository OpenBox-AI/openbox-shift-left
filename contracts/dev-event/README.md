# `contracts/dev-event` — normalized developer-runtime event contract

**Story:** STORY-SL-1 · **Version:** 1.0 · **Status:** built + validated; G1_READY + G3_REVIEW approved (2026-07-07)

The single, versioned, **tool-agnostic** event schema that every coding-tool adapter
maps its native payload onto (SPI `emit()`). The OpenBox client then re-expresses it onto
the base SDK's unified wire model (Workflow*/SignalReceived/ActivityStarted+hook) for
openbox-core — see MAPPING.md; **ADR-0004**/E7. Adding a provider (Claude Code, Codex,
Cursor, …) never changes this contract or the wire model (PRD **FR-4**, architecture **§1b**).

## Layout

| Path | What |
|---|---|
| [`schema/dev-event.schema.json`](schema/dev-event.schema.json) | The contract — JSON Schema (draft 2020-12), language-neutral. 7 lifecycle event types, common envelope, `tool{}`, `span`, gated `content`, canonical `verdict` enum. |
| [`MAPPING.md`](MAPPING.md) | How the contract maps onto the base-SDK unified wire model (`ActivityStarted`+hook / `Workflow*` / `SignalReceived`) on openbox-core (built E7-S3/S4/S5; ADR-0004). SL-3 builds payloads from this without guessing. Includes the ToolCall/ToolResult span-pairing correction (E7-S4), the downstream-consumer sweep (INV-8), and client signing/transport notes. |
| [`COVERAGE.md`](COVERAGE.md) | How Claude Code / Cursor / Codex real event surfaces map onto the 7 types, field-derivation rules, and the bounded Phase-1 non-goals (v1.1 candidates). The reference for adapter authors (SL-4/7/8). |
| [`conformance/`](conformance/) | Go conformance harness (OD17). Dependency-free; validates samples against the schema and enforces the INV-2 content gate. |

## The 7 lifecycle event types

`SessionStarted` · `PromptSubmitted` · `ToolCall` · `ToolResult` · `SessionEnded` ·
`CommitCreated` · `Deploy`

These are the adapter-facing **lifecycle** axis. The client re-maps them onto the base
SDK's stock wire types (no core accept-list patch — EXT-core retired, E7-S2); they carry
core's **existing** **semantic-span** types (`file_write`, `mcp_tool_call`, `shell_command`,
`llm_completion`, …). See MAPPING.md §2 for the two axes.

## Privacy (INV-2)

**Content capture is ON by default as of 2026-07-15** (brian; this reverses OD4's original
metadata-only-by-default posture). Prompt content is captured and egresses unless an org opts
OUT (`content_capture:false` or `OPENBOX_CONTENT_CAPTURE=0`).

What INV-2 still guarantees:

- Content lives **only** under the `content` object and span `request_body`/`response_body`.
  With capture off, all of it is stripped before egress — including content-bearing keys in the
  `metadata` blob, which the client drops at the same gate (RF-S7; before that, metadata was a
  hole INV-2 rested on adapter convention to keep closed).
- Tool commands and file bodies never egress on observe events (SL3-SEC-3), capture on or off.
- The conformance harness rejects any event carrying content while content-capture is disabled.

What it does **not** guarantee today: captured content is meant to be Guardrail-redacted at
source, but that layer is inert (`[EXT-guardrail-redaction]`), so with capture on — the default —
prompt content egresses **unredacted**. Tier-1 local secret detection redacts Write/Edit bodies
in enforce mode only.

## Verdict vocabulary

Canonical (priority): `HALT > BLOCK > REQUIRE_APPROVAL > CONSTRAIN > ALLOW`. openbox-core
serializes the response `verdict` field as lowercase (`halt|block|require_approval|constrain|allow`)
plus a legacy `action` field — see `$defs.verdict` and MAPPING.md §4. Observe mode treats every
verdict as allow (INV-3); the opt-in enforce mode acts on them, tighten-only — see COVERAGE.md §4.

## Validate

```bash
cd conformance && go build ./... && go vet ./... && go test ./...
```

The harness is intentionally offline/zero-dependency, so this runs anywhere with a Go
toolchain and no module downloads.

## Consuming the contract

- **SL-3 (client):** build the base-SDK wire payload per MAPPING.md; `import` the `conformance`
  package to validate outbound events before signing/POST.
- **Adapters (SL-4/7/8):** map native tool payloads onto this schema in `emit()`.
- **EXT-core — RETIRED (ADR-0004 / E7-S2):** dev events now map to stock base wire types that
  openbox-core already accept-lists; no patch is needed. See [`ext-core/README.md`](ext-core/README.md).
  The one additive core change E7 keeps is the semantic classifier (`shell`→`shell_command`,
  `mcp`→`mcp_tool_call`), not an accept-list.
