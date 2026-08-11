# `contracts/dev-event` — normalized developer-runtime event contract

**Story:** STORY-SL-1 · **Version:** 1.0 · **Status:** built + validated; G1_READY + G3_REVIEW approved (2026-07-07)

The single, versioned, **tool-agnostic** event schema that every coding-tool adapter
maps its native payload onto (SPI `emit()`). The OpenBox client then re-expresses it onto
the base SDK's unified wire model (`Workflow*` / `SignalReceived` / `ActivityStarted` /
`ActivityCompleted`, all span-less) for openbox-core — see MAPPING.md; **ADR-0004** and
**ADR-0013**. Adding a provider (Claude Code, Codex, Cursor, …) never changes this contract
or the wire model (PRD **FR-4**, architecture **§1b**).

## Layout

| Path | What |
|---|---|
| [`schema/dev-event.schema.json`](schema/dev-event.schema.json) | The contract — JSON Schema (draft 2020-12), language-neutral. 7 lifecycle event types, common envelope, `tool{}`, `span`, gated `content`, canonical `verdict` enum. |
| [`MAPPING.md`](MAPPING.md) | How the contract maps onto the base-SDK unified wire model on openbox-core (ADR-0004, ADR-0013). SL-3 builds payloads from this without guessing. §3's field-home table is the authority on what the serializer reads; also carries the downstream-consumer sweep (INV-8) and client signing/transport notes. |
| [`COVERAGE.md`](COVERAGE.md) | How Claude Code / Cursor / Codex real event surfaces map onto the 7 types, field-derivation rules, and the bounded Phase-1 non-goals (v1.1 candidates). The reference for adapter authors (SL-4/7/8). |
| [`conformance/`](conformance/) | Go conformance harness (OD17). Dependency-free; validates samples against the schema and enforces the INV-2 content gate. |

## The 7 lifecycle event types

`SessionStarted` · `PromptSubmitted` · `ToolCall` · `ToolResult` · `SessionEnded` ·
`CommitCreated` · `Deploy`

These are the adapter-facing **lifecycle** axis. The client re-maps them onto the base
SDK's stock wire types — no core accept-list patch. `ToolCall`/`ToolResult` became
`ActivityStarted`/`ActivityCompleted` in ADR-0013; the schema itself did **not** change,
which is the two-layer split working as intended.

The `span` object stays in the contract and adapters keep populating it, but it is no
longer serialized as a span: the client reads locators and counts out of it into
`activity_input`/`activity_output`. One consequence is worth knowing before you write an
adapter — since no span reaches core, core computes no `semantic_type`, so
`file_write`/`mcp_tool_call`/`shell_command` classification does not happen for dev
sessions. `tool.kind` is what carries that distinction now. See MAPPING.md §3.

## Privacy (INV-2)

**Content capture is ON by default as of 2026-07-15** (brian; this reverses OD4's original
metadata-only-by-default posture). Prompt content is captured and egresses unless an org opts
OUT (`content_capture:false` or `OPENBOX_CONTENT_CAPTURE=0`).

What INV-2 still guarantees:

- Content lives **only** under the `content` object. With capture off, all of it is stripped
  before egress — including content-bearing keys in the `metadata` blob, which the client drops
  at the same gate (RF-S7; before that, metadata was a hole INV-2 rested on adapter convention
  to keep closed).
- `span.request_body`/`response_body` remain in the schema but are **no longer read by the
  client**, so they cannot egress at all (ADR-0013). No adapter ever set them; both adapters
  have tests asserting they stay empty.
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
