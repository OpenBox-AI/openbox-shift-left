# `api/`; normalized developer-runtime event contract

**Story:**  event contract · **Version:** the `schema_version` `const` in [the
schema](../api/dev-event.schema.json) is the authority; v1.1 added the turn
pair, v1.2 tool `status`, the subagent/denial/error types and the turn span,
v1.3 tool content and the signals' free text, v1.4 the turn's thinking ·
**Status:** built + validated (v1.0 carried G1_READY + G3_REVIEW, 2026-07-07;
the later bumps are additive and were reviewed with their decision records)

The single, versioned, **tool-agnostic** event schema that every coding-tool
adapter maps its native payload onto (SPI `emit`). The OpenBox client then
re-expresses it onto the base SDK's unified wire model (`Workflow*` /
`SignalReceived` / `ActivityStarted` / `ActivityCompleted`; span-less apart from
the one content-gated turn span) for openbox-core; see MAPPING.md. Adding a
provider (Claude Code, Codex, Cursor, …) never changes this contract or the wire
model (PRD **FR-4**, architecture **§1b**).

## Layout

| Path | What |
|---|---|
| [`schema/dev-event.schema.json`](../api/dev-event.schema.json) | The contract; JSON Schema (draft 2020-12), language-neutral. 7 lifecycle event types, common envelope, `tool{}`, `span`, gated `content`, canonical `verdict` enum. |
| [`MAPPING.md`](MAPPING.md) | How the contract maps onto the base-SDK unified wire model on openbox-core. the client builds payloads from this without guessing. §3's field-home table is the authority on what the serializer reads; also carries the downstream-consumer sweep (INV-8) and client signing/transport notes. |
| [`COVERAGE.md`](COVERAGE.md) | How Claude Code / Cursor / Codex real event surfaces map onto the lifecycle types, field-derivation rules, and the bounded non-goals. The reference for adapter authors (the Claude Code adapter/7/8). |
| [`conformance/`](../internal/conformance/) | Go conformance harness. Dependency-free; validates samples against the schema and enforces the INV-2 content gate. |

## The lifecycle event types

The `event_type` enum in [the schema](../api/dev-event.schema.json) is the list,
and COVERAGE.md §1 maps each one onto the providers' native hooks. V1.0's
original seven have since been joined by the turn pair and by `SubagentStarted`
/ `PermissionDenied` / `APIError`.

These are the adapter-facing **lifecycle** axis. The client re-maps them onto
the base SDK's stock wire types; no core accept-list patch.
`ToolCall`/`ToolResult` became `ActivityStarted`/`ActivityCompleted` in; the
schema itself did **not** change, which is the two-layer split working as
intended.

The `span` object stays in the contract and adapters keep populating it, but it
is no longer serialized as a span: the client reads locators and counts out of
it into `activity_input`/`activity_output`. One consequence is worth knowing
before you write an adapter; no tool event reaches core as a span, so core
computes no `semantic_type` for one, and
`file_write`/`mcp_tool_call`/`shell_command` classification does not happen for
dev sessions. `tool.kind` is what carries that distinction now. The one span
that does reach core is minted by the *client* on a captured turn: no adapter
populates it, it carries no locator, and it exists because the alignment reader
accepts no other shape. See MAPPING.md §3.

## Privacy (INV-2)

**Content capture is ON by default as of 2026-07-15** (brian; this reverses
an owner decision's original metadata-only-by-default posture). Prompt content is captured and
egresses unless an org opts OUT (`content_capture:false` or
`OPENBOX_CONTENT_CAPTURE=0`).

What INV-2 still guarantees:

- Content lives **only** under the `content` object. With capture off, all of it
  is stripped before egress; including content-bearing keys in the `metadata`
  blob, which the client drops at the same gate (RF-S7; before that, metadata
  was a hole INV-2 rested on adapter convention to keep closed).
- `span.request_body`/`response_body` remain in the schema but are **no longer
  read by the client**, so nothing an adapter puts there can egress. No adapter
  ever set them; both adapters have tests asserting they stay empty. The
  assistant text that *does* egress rides a span the client mints from a hook
  field, not this one; so the two are not the same channel re-opened.
- ~~Tool commands and file bodies never egress on **observe** events.~~ **Retired in v1.3**. Tool input, tool output and the free-text
  failure detail now egress on ordinary tool events, under the same
  `content_capture` gate that covers a gated call's body; redacted before they
  are attached and capped at 64KB. The guarantee is a posture now, not a
  structural property, which is why the ordering and the gate are asserted on
  the outbound bytes (conformance C18, C26, C32–C38) rather than inferred from
  the absence of a field.
- The conformance harness rejects any event carrying content while
  content-capture is disabled.

What it does **not** guarantee today: captured content is meant to be
Guardrail-redacted at source, but that layer is inert
(`[EXT-guardrail-redaction]`), so with capture on, the default, prompt content
egresses **unredacted**. Local secret detection, the tier model is retired, so
this is the vocabulary; it is one of three independently named things now,
redacts Write/Edit bodies in enforce mode only, and only while
`secret_detection` is on.

## Verdict vocabulary

Canonical (priority): `HALT > BLOCK > REQUIRE_APPROVAL > CONSTRAIN > ALLOW`.
Openbox-core serializes the response `verdict` field as lowercase
(`halt|block|require_approval|constrain|allow`) plus a legacy `action` field;
see `$defs.verdict` and MAPPING.md §4. Observe mode treats every verdict as
allow (INV-3); enforce mode, **on by default**, acts on
them, tighten-only. See COVERAGE.md §4.

## Validate

```bash
go build./internal/conformance/... && go vet./internal/conformance/... && go test./internal/conformance/...
```

The harness is intentionally offline/zero-dependency, so this runs anywhere with
a Go toolchain and no module downloads.

## Consuming the contract

- **the client (client):** build the base-SDK wire payload per MAPPING.md; `import`
  the `conformance` package to validate outbound events before signing/POST.
- **Adapters (the Claude Code adapter/7/8):** map native tool payloads onto this schema in
  `emit`.
- **EXT-core, retired:** dev events now map to stock
  base wire types that openbox-core already accept-lists; no patch is needed.
  The ext-core patch set was retired 2026-07-15 and its tombstone deleted; that
  decision is the record. The one additive core change E7 keeps is the semantic
  classifier (`shell`→`shell_command`, `mcp`→`mcp_tool_call`), not an
  accept-list.
