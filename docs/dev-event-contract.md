# `api/`; normalized developer-runtime event contract

**Story:**  event contract · **Version:** the `schema_version` `const` in [the
schema](../api/dev-event.schema.json) is the authority; v1.1 added the turn
pair, v1.2 tool `status`, the subagent/denial/error types and the turn span,
v1.3 tool content and the signals' free text, v1.4 the turn's thinking, v1.5 the
gateway's span fields, v1.6 the `:otel:`/`:proxy:` producers, v1.7 `activity_type`
and the removal of the span carrier ·
**Status:** built + validated (v1.0 carried G1_READY + G3_REVIEW, 2026-07-07;
the later bumps are additive)

The single, versioned, **tool-agnostic** event schema that every coding-tool
adapter maps its native payload onto (SPI `emit`). The OpenBox client then
re-expresses it onto the base SDK's unified wire model (`Workflow*` /
`SignalReceived` / `ActivityStarted` / `ActivityCompleted`; **entirely span-less**
as of v1.7) for openbox-core; see mapping.md. Adding a
provider (Claude Code, Codex, Cursor, …) never changes this contract or the wire
model (PRD **FR-4**, architecture **§1b**).

## Layout

| Path | What |
|---|---|
| [`api/dev-event.schema.json`](../api/dev-event.schema.json) | The contract; JSON Schema (draft 2020-12), language-neutral. 7 lifecycle event types, common envelope, `tool{}`, `span`, gated `content`, canonical `verdict` enum. |
| [`mapping.md`](mapping.md) | How the contract maps onto the base-SDK unified wire model on openbox-core. the client builds payloads from this without guessing. §3's field-home table is the authority on what the serializer reads; also carries the downstream-consumer sweep (INV-8) and client signing/transport notes. |
| [`coverage.md`](coverage.md) | How Claude Code / Cursor / Codex real event surfaces map onto the lifecycle types, field-derivation rules, and the bounded non-goals. The reference for adapter authors (the Claude Code adapter/7/8). |
| [`conformance/`](../internal/conformance/) | Go conformance harness. Dependency-free; validates samples against the schema and enforces the INV-2 content gate. |

## The lifecycle event types

The `event_type` enum in [the schema](../api/dev-event.schema.json) is the list,
and coverage.md §1 maps each one onto the providers' native hooks. V1.0's
original seven have since been joined by the turn pair and by `SubagentStarted`
/ `PermissionDenied` / `APIError`.

These are the adapter-facing **lifecycle** axis. The client re-maps them onto
the base SDK's stock wire types; no core accept-list patch.
`ToolCall`/`ToolResult` became `ActivityStarted`/`ActivityCompleted` in; the
schema itself did **not** change, which is the two-layer split working as
intended.

The `span` object stays in the contract and adapters keep populating it, but it
is no longer serialized as a span: the client reads locators, counts **and
bodies** out of it into `activity_input`/`activity_output`. One consequence is
worth knowing before you write an adapter; **no event reaches core as a span at
all**, so core computes no `semantic_type` for one, and
`file_write`/`mcp_tool_call`/`shell_command` classification does not happen for
dev sessions. `tool.kind` is what carries that distinction now.

One thing the contract does **not** define, and no adapter should read as
defined: what the newest element of a model call's stored `messages` is. The
agent runtime appends elements that are not conversation -- the common one is a
`role:"system"` element carrying `<total_tokens>N tokens left</total_tokens>` --
and `role:"system"` *inside* `messages` is not documented Anthropic Messages API
surface, where `system` is a top-level field. It is a client artifact, not a
provider shape. `internal/gateway` drops such trailing elements so the newest
stored element is a real turn, and reports how many it dropped under
`openbox_selection.skipped_trailing_non_turns`; see mapping.md §The request
window is a SELECTION.

v1.7 removed the last span, and the reason is worth stating because the previous
answer looked like it worked. Core parses `spans[]` on the normal path, uses it in
memory, and then **discards it**: persistence is gated on `hook_trigger` plus a
pre-existing event row (`governance_workflow.go:234`, `validation.go:154-174`),
which this client deliberately never sets, because that would put a model turn on
core's approval-bypass fingerprint path. So every body sent there was dropped on
arrival, and every test asserting one "reached the wire" was true and pointless.
Separately, a span describes work nested *inside* an activity, and for a model
call `llm_completion` **is** the activity -- so nesting one would represent the same
call twice. See mapping.md §2.

Two fields changed egress behaviour with it, and neither is a loss:

- `span.credential_fingerprint` moved to `metadata.credential_fingerprint`, still
  ungated. A **rehoming**, not a regression: span attributes never persisted for a
  developer session, so account binding has never had anything to match on.
- `span.request_headers` / `span.response_headers` are **no longer emitted by any
  producer**. Nothing read them -- they only ever reached core inside the discarded
  array -- and they were the highest-risk class this client carried, since the
  developer's live provider credential is on every model request. They stay
  declared, and capture still redacts them by key name; they simply do not leave
  the machine. An adapter that later needs one specific header should add one
  allowlisted metadata key, decided then.

See mapping.md §3 for where every field lands.

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
  assistant text that *does* egress rides `activity_output.content`, which the
  control plane stores in a column of its own; so the two are not the same
  channel re-opened.
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
see `$defs.verdict` and mapping.md §4. Observe mode treats every verdict as
allow (INV-3); enforce mode, **on by default**, acts on
them, tighten-only. See coverage.md §4.

## Validate

```bash
go build./internal/conformance/... && go vet./internal/conformance/... && go test./internal/conformance/...
```

The harness is intentionally offline/zero-dependency, so this runs anywhere with
a Go toolchain and no module downloads.

## Consuming the contract

- **the client (client):** build the base-SDK wire payload per mapping.md; `import`
  the `conformance` package to validate outbound events before signing/POST.
- **Adapters (the Claude Code adapter/7/8):** map native tool payloads onto this schema in
  `emit`.
- **EXT-core, retired:** dev events now map to stock
  base wire types that openbox-core already accept-lists; no patch is needed.
  The ext-core patch set was retired 2026-07-15 and its tombstone deleted; that
  decision is the record. The one additive core change E7 keeps is the semantic
  classifier (`shell`→`shell_command`, `mcp`→`mcp_tool_call`), not an
  accept-list.

## Session-end evidence completeness

A `SessionEnded` event carries what the client knows about its own delivery, in
`metadata`. It is the only place a session says whether its telemetry is whole.

| Key | Scope | Means |
|---|---|---|
| `evidence_state` | this session | `complete`, or `degraded` when either count below is non-zero |
| `evidence_undelivered` | this session | events waiting in carry-over (recovery) files: an earlier flush failed and will retry. Omitted when zero |
| `evidence_discarded` | **machine-wide, cumulative** | events this machine gave up on entirely -- past `MaxRecoveryAttempts`, or past the retention age. Omitted when zero |

Three decisions a reader will otherwise find surprising.

**`evidence_undelivered` stays session-scoped and counts carry-over files only**,
which is what it has always measured. That is deliberately narrow, and the
narrowness is what made a real gap invisible: it read **71** on a machine where
**118** events sat in plain session files, which are not carry-over files and so
are not counted. The machine-wide backlog is surfaced by `openbox doctor`, where
it is actionable, rather than by widening a per-session field into something a
reader would misinterpret.

**`evidence_discarded` is machine-wide on purpose.** Loss is not scoped to the
session that happens to end next. It exists because the bound was already there
and silent: `writeRecovery` stopped re-queueing past `MaxRecoveryAttempts` with no
log line, no counter and no telemetry -- governance evidence discarded with nothing
said. Every discard is now also recorded, with a timestamp and a count, in
`.discarded` beside the spool.

**One definition, not one per adapter.** `EvidenceState` lives in
`internal/adapters/common/hookflow` and both adapters alias it. It was two
byte-identical copies producing one documented set of wire keys; one definition
cannot drift, where two copies plus a test asserting they agree can only detect
that they have.
