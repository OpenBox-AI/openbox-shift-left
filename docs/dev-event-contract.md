# `api/`; normalized developer-runtime event contract

> **Who this is for.** Someone adding or changing a coding-tool adapter, or
> consuming these events downstream. It assumes the shape in
> [Architecture](architecture.md): one engine, one thin adapter per tool, one
> event schema between them. Nothing here is needed to set a machine up.

**Story:**  event contract · **Version:** the `schema_version` `const` in [the
schema](../api/dev-event.schema.json) is the authority, and its `x-changelog`
records what every bump added and why — one entry per version, in the file that
defines them. A ledger here is a second copy of that list, and the one that used
to be here fell a version behind. ·
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
| [`api/dev-event.schema.json`](../api/dev-event.schema.json) | The contract; JSON Schema (draft 2020-12), language-neutral. **33** lifecycle event types (7 original, growing to 33 as of v1.8), common envelope, `tool{}`, `span`, gated `content`, canonical `verdict` enum, three additive run-identity fields. |
| [`mapping.md`](mapping.md) | How the contract maps onto the base-SDK unified wire model on openbox-core. the client builds payloads from this without guessing. §3's field-home table is the authority on what the serializer reads; also carries the downstream-consumer sweep (INV-8) and client signing/transport notes. |
| [`coverage.md`](coverage.md) | How Claude Code / Cursor / Codex real event surfaces map onto the lifecycle types, field-derivation rules, and the bounded non-goals. The reference for adapter authors (the Claude Code adapter/7/8). |
| [`conformance/`](../internal/conformance/) | Go conformance harness. Dependency-free; validates samples against the schema and enforces the INV-2 content gate. |

## The lifecycle event types

The `event_type` enum in [the schema](../api/dev-event.schema.json) is the list,
and coverage.md §1 maps each one onto the providers' native hooks. V1.0's
original seven have since been joined by the turn pair, by `SubagentStarted`
/ `PermissionDenied` / `APIError`, and, in v1.8, by **21** more observe-only
lifecycle signals — `Setup`, `InstructionsLoaded`, `UserPromptExpansion`,
`MessageDisplay`, `PermissionRequest`, `PostToolBatch`, `Notification`,
`TaskCreated`, `TaskCompleted`, `TeammateIdle`, `ConfigChange`, `CwdChanged`,
`DirectoryAdded`, `FileChanged`, `WorktreeRemove`, `PreCompact`, `PostCompact`,
`PreModelSwitch`, `PostModelSwitch`, `Elicitation`, `ElicitationResult` — every
one riding stock `SignalReceived`, taking the enum from 12 to **33**.
`WorktreeCreate` is deliberately **not** among them: it is refused, not
missing (coverage.md §3); the ceiling is 32 of 33 documented Claude Code
hooks, never 33 of 33.

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

**What core stores under `metadata` is not what this client sent under it.**
The stored object is a *union* of the wire's `metadata` and the flattened
`span`, and `span.invocation_id` is renamed to `tool_use_id` on the way. Reading
a stored row as the wire's own shape will mislead in both directions: a key that
is absent here may be present there, and one named here may be named differently
there.

The schema also no longer takes `metadata` on trust. It was `{"type": "object"}`
with nothing else -- closed at the root, wide open one level down -- so a mapper
that renamed or dropped a structural key validated clean. It is now a closed
union of the keys the producers in this repository actually emit, which is what
makes a rename a validation failure rather than a silent change. INV-1 and INV-2
are still enforced by `contentMetadataKeys` in code; the schema constrains
membership and types, deliberately not credential shapes.

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

## Invariants

One glossary entry per INV, collecting the wording seven other sites cite
without defining (`docs/mapping.md`, `docs/architecture.md`, `docs/coverage.md`,
`internal/provider/provider.go`,
`internal/cli/devinit/devinit.go`, `internal/cli/gatewayemit/emitter_test.go`).
Where two sites paraphrased the same INV differently, the stricter wording
below wins.

| INV | Statement | Pinned by |
|---|---|---|
| **INV-1** | A credential value never egresses and never appears in a local decision request, a log line, or an argv; only the file path or a one-way fingerprint is ever shown or sent. | `internal/cli/devinit/devinit.go` (the credential-write path prints the file path, never a value); `internal/provider/provider.go`'s `CredentialRef` (never carries a credential value); `MaxCommandLen` bounds a local decision request, never egress. |
| **INV-2** | Content is gated at one choke point (`content_capture`); with it off, no content-bearing field reaches the wire — including content-bearing keys inside `metadata`, and, since v1.9, inside `signal_args`, which carries the same keys — and only structural identifiers (paths, tool names, ids) always flow. Local secret detection is keyword-driven, so an unlabelled high-entropy value below the floor is invisible to it. | `internal/conformance`'s content-gate harness (a gated field is asserted absent with capture off, present/redacted/capped with it on); `contentMetadataKeys` in `internal/client/payload.go` (every content key must be listed there or an adapter routes around the gate). Both destinations are filtered by ONE function, `eventMetadataForEgress`, so `metadata` and `signal_args` cannot disagree about a key; `TestContentBearingMetadataIsGatedInSignalArgs` and `TestEveryContentKeyIsGatedInSignalArgs` assert the second destination on the outbound bytes. |
| **INV-3** | Observe mode treats every verdict as advisory (fail-open): a verdict from `/evaluate` never blocks a call by itself. Enforcement, a separate local decision, is tighten-only — it never turns a provider's own deny into an allow. | `internal/adapters/claude-code/hookrun.go`'s observe path; `internal/cli/gatewayemit/emitter_test.go`'s `TestEmitSurvivesAnUnwritableSpool`. |

## Privacy (INV-2)

**Content capture is ON by default as of 2026-07-15** (brian; this reverses
an owner decision's original metadata-only-by-default posture). Prompt content is captured and
egresses unless an org opts OUT (`content_capture:false` or
`OPENBOX_CONTENT_CAPTURE=0`).

What INV-2 still guarantees:

- Content lives **only** under the `content` object. With capture off, all of it
  is stripped before egress; including content-bearing keys in the `metadata`
  blob, which the client drops at the same gate (RF-S7; before that, metadata
  was a hole INV-2 rested on adapter convention to keep closed). Since v1.9 a
  signal's `metadata` keys are also projected into `signal_args`, through that
  same drop — and content keys arriving that way are capped by `capBody` on
  egress, which they were not before, because a free-text key riding `metadata`
  had no bound at all.
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
  the outbound bytes (conformance C18, C26, C32–C38, C40–C49 and C51–C56) rather
  than inferred from the absence of a field. C50 is deliberately unused, retired
  when `UserPromptExpansion` became structural-only, and C39 was never assigned a
  case at all — the gaps are left rather than renumbered, so a reader does not
  "fix" the sequence and lose that history.
- The conformance harness rejects any event carrying content while
  content-capture is disabled.

What it does **not** guarantee today: captured content is meant to be
Guardrail-redacted at source, but that layer is inert
(`[EXT-guardrail-redaction]`), so nothing on the receiving side redacts what
arrives. Local secret detection is the only in-transit control, it runs only
while `secret_detection` is on, and how much it covers is **the adapter's
property rather than this contract's**: the Claude Code mapper runs it over
every content class it attaches — prompt, tool input, tool output, the
assistant's reply, thinking — while the Codex mapper has no redactor at all and
redacts only the enforce path's file body, so a Codex prompt egresses unscanned
with `secret_detection` on. The gated copy of a shell or MCP call is verbatim on
both, by decision; see
[data-and-privacy.md](data-and-privacy.md#what-an-enforced-call-sends) for that
carve-out and for what the detector reaches.

## Verdict vocabulary

Canonical (priority): `HALT > BLOCK > REQUIRE_APPROVAL > CONSTRAIN > ALLOW`.
Openbox-core serializes the response `verdict` field as lowercase
(`halt|block|require_approval|constrain|allow`) plus a legacy `action` field;
see `$defs.verdict` and mapping.md §4. Observe mode treats every verdict as
allow (INV-3); enforce mode, **on by default**, acts on
them, tighten-only. See coverage.md §4.

## Validate

```bash
go build ./internal/conformance/... && go vet ./internal/conformance/... && go test ./internal/conformance/...
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
| `evidence_undelivered` | this session | this session's own not-yet-**attempted** backlog: head-file lines (attempted too late to start a pass, requeued ahead of the tail) plus tail-file lines never yet drained at all (`Spool.PendingCount`, via `UndeliveredCountFor`). Never counts an event that WAS attempted and refused -- that is ledgered and gone immediately (see `evidence_discarded`), never left pending. Omitted when zero |
| `evidence_discarded` | **machine-wide, cumulative** | events this machine's ledger recorded as gone: an attempted delivery core explicitly did not accept, a crash-orphaned line whose outcome could not be proven, a legacy artifact discarded on sight, or a genuinely unattempted backlog past the 30-day retention age (`Spool.DiscardedCount`, reading `.discarded`). Omitted when zero |

Three decisions a reader will otherwise find surprising.

**`evidence_undelivered` stays session-scoped, and now sums the session's
entire own queue (head-plus-tail), not a narrower "recovery file" subset.**
Historically it counted only carry-over (recovery) files -- a narrower
category than the session's real backlog -- and that narrowness is what made
a real gap invisible: it read **71** on a machine where **118** events sat in
plain session files, which were not carry-over files and so went uncounted.
Delivery is single-attempt now and there is no separate recovery-file stage
any more (a spool file is a tail, a head, or a reclaimed orphan; `PendingCount`
already sums the first two), so that specific gap is closed at the session
level: the field is the session's own complete still-queued count. It
remains deliberately narrower than a machine-wide figure in one respect
only -- another session's own backlog sitting in the same spool directory is
never counted here, by design; that machine-wide view is surfaced by
`openbox doctor`, where it is actionable, rather than by widening a
per-session field into something a reader would misinterpret.

**`evidence_discarded` is machine-wide on purpose.** Loss is not scoped to the
session that happens to end next. It exists because an earlier bound was
already there and silent (the pre-single-attempt design stopped re-queueing
past a retry count with no log line, no counter and no telemetry -- governance
evidence discarded with nothing said); every discard is now recorded, with a
timestamp, an event count and a reason, in `.discarded` beside the spool --
which today is most discards' only path in, since a single failed attempt is
ledgered immediately rather than retried toward any count.

**One definition, not one per adapter.** `EvidenceState` lives in
`internal/adapters/common/hookflow` and both adapters alias it. It was two
byte-identical copies producing one documented set of wire keys; one definition
cannot drift, where two copies plus a test asserting they agree can only detect
that they have.
