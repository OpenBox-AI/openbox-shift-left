# `api/` — normalized developer-runtime event contract

**Story:** STORY-SL-1 · **Version:** the `schema_version` `const` in
[the schema](schema/dev-event.schema.json) is the authority — v1.1 added the turn pair
(ADR-0014), v1.2 tool `status`, the subagent/denial/error types and the turn span
(ADR-0018), v1.3 tool content and the signals' free text (ADR-0019 P1), v1.4 the turn's
thinking (ADR-0019 P3) · **Status:** built + validated (v1.0 carried G1_READY + G3_REVIEW,
2026-07-07; the later bumps are additive and were reviewed with their ADRs)

The single, versioned, **tool-agnostic** event schema that every coding-tool adapter
maps its native payload onto (SPI `emit()`). The OpenBox client then re-expresses it onto
the base SDK's unified wire model (`Workflow*` / `SignalReceived` / `ActivityStarted` /
`ActivityCompleted`; span-less apart from the one content-gated turn span of
**ADR-0018**) for openbox-core — see MAPPING.md; **ADR-0004**, **ADR-0013** and
**ADR-0018**. Adding a provider (Claude Code, Codex, Cursor, …) never changes this contract
or the wire model (PRD **FR-4**, architecture **§1b**).

## Layout

| Path | What |
|---|---|
| [`schema/dev-event.schema.json`](schema/dev-event.schema.json) | The contract — JSON Schema (draft 2020-12), language-neutral. 7 lifecycle event types, common envelope, `tool{}`, `span`, gated `content`, canonical `verdict` enum. |
| [`MAPPING.md`](MAPPING.md) | How the contract maps onto the base-SDK unified wire model on openbox-core (ADR-0004, ADR-0013). SL-3 builds payloads from this without guessing. §3's field-home table is the authority on what the serializer reads; also carries the downstream-consumer sweep (INV-8) and client signing/transport notes. |
| [`COVERAGE.md`](COVERAGE.md) | How Claude Code / Cursor / Codex real event surfaces map onto the lifecycle types, field-derivation rules, and the bounded non-goals. The reference for adapter authors (SL-4/7/8). |
| [`conformance/`](conformance/) | Go conformance harness (OD17). Dependency-free; validates samples against the schema and enforces the INV-2 content gate. |

## The lifecycle event types

The `event_type` enum in [the schema](schema/dev-event.schema.json) is the list, and
COVERAGE.md §1 maps each one onto the providers' native hooks. v1.0's original seven
have since been joined by the turn pair (ADR-0014) and by `SubagentStarted` /
`PermissionDenied` / `APIError` (ADR-0018).

These are the adapter-facing **lifecycle** axis. The client re-maps them onto the base
SDK's stock wire types — no core accept-list patch. `ToolCall`/`ToolResult` became
`ActivityStarted`/`ActivityCompleted` in ADR-0013; the schema itself did **not** change,
which is the two-layer split working as intended.

The `span` object stays in the contract and adapters keep populating it, but it is no
longer serialized as a span: the client reads locators and counts out of it into
`activity_input`/`activity_output`. One consequence is worth knowing before you write an
adapter — no tool event reaches core as a span, so core computes no `semantic_type` for
one, and `file_write`/`mcp_tool_call`/`shell_command` classification does not happen for
dev sessions. `tool.kind` is what carries that distinction now. The one span that does
reach core is minted by the *client* on a captured turn (ADR-0018): no adapter populates
it, it carries no locator, and it exists because the alignment reader accepts no other
shape. See MAPPING.md §3.

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
  client**, so nothing an adapter puts there can egress (ADR-0013). No adapter ever set them;
  both adapters have tests asserting they stay empty. The assistant text that *does* egress
  (ADR-0018) rides a span the client mints from a hook field, not this one — so the two are
  not the same channel re-opened.
- ~~Tool commands and file bodies never egress on **observe** events (SL3-SEC-3).~~
  **Retired in v1.3** (ADR-0019 P1). Tool input, tool output and the free-text failure
  detail now egress on ordinary tool events, under the same `content_capture` gate that
  covers a gated call's body — redacted before they are attached and capped at 64KB.
  The guarantee is a posture now, not a structural property, which is why the ordering
  and the gate are asserted on the outbound bytes (conformance C18, C26, C32–C38) rather
  than inferred from the absence of a field.
- The conformance harness rejects any event carrying content while content-capture is disabled.

What it does **not** guarantee today: captured content is meant to be Guardrail-redacted at
source, but that layer is inert (`[EXT-guardrail-redaction]`), so with capture on — the default —
prompt content egresses **unredacted**. Local secret detection — ADR-0017 retired the tier
vocabulary; it is one of three independently named things now — redacts Write/Edit bodies in
enforce mode only, and only while `secret_detection` is on.

## Verdict vocabulary

Canonical (priority): `HALT > BLOCK > REQUIRE_APPROVAL > CONSTRAIN > ALLOW`. openbox-core
serializes the response `verdict` field as lowercase (`halt|block|require_approval|constrain|allow`)
plus a legacy `action` field — see `$defs.verdict` and MAPPING.md §4. Observe mode treats every
verdict as allow (INV-3); enforce mode — **on by default** since ADR-0016 — acts on them,
tighten-only. See COVERAGE.md §4.

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
