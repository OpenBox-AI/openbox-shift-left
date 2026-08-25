# Phase 01 — Tool content capture

## Context links

- Parent: [plan.md](plan.md)
- Design: [advise-260824-1841](../reports/advise-260824-1841-full-io-capture-gateway.md)
- Authority: `docs/adr/ADR-0019-full-content-capture.md` P1 (Proposed — accept in phase 03)
- Hook surface evidence: `plans/reports/probe-260813-2329-claude-code-hook-surface.md`
- Verification: [verification-260825-0328](../reports/verification-260825-0328-tool-content-capture.md)
  — every claim filed by evidence strength
- Depends on: none. Ships alone.

## Overview

- Date: 2026-08-25
- Description: bind the tool content already arriving on hook stdin — output, input on the
  observe path, and the three free-text failure fields — and egress it content-gated.
- Priority: P1
- Implementation status: implemented (2026-08-25) — unit- and conformance-verified; testbed dormant
- Review status: advisory pre-review (kongming) + code review, 2026-08-25

## Key insights

- **The data is already on stdin and the adapter throws it away.** `tool_response` has zero
  references in `adapters/claude-code/mapper.go`. `hookevent.go:96-104` says content is
  "intentionally not decoded". This is a binding gap, not a surface gap.
- **MCP comes free.** MCP calls are `mcp__<server>__<tool>` and flow through the same
  PreToolUse/PostToolUse hooks (`mapper.go:627 splitMCPName`). Closing tool output closes
  MCP output in the same change.
- **This retires SL3-SEC-3** ("tool commands and file bodies never egress on observe
  events"). C19's observe-path assertions are **updated to assert the gate, not deleted**.
- **Ordering is the control.** detect → redact → attach → cap → sign. A redaction applied
  after attachment passes every unit test and still leaks. Assert on outbound bytes (C26
  pattern), and the conformance case merges **before** the flush code that carries it.
- `status` stays ungated and tool-only — unchanged by this phase.

## Requirements

1. `PostToolUse.tool_response` → `activity_output.output`, content-gated.
2. Tool input on the **observe** path (not only the gated `/evaluate` copy).
3. `PostToolUseFailure.error`, `PermissionDenied.reason`, `StopFailure.error_details` bound
   as free text, content-gated.
4. All four secret-redacted before attach, capped at 64KB before signing.
5. `PermissionDenied.reason` must not become `signal_args` — core reads a `SignalReceived`
   with non-empty `signal_args` as a NEW USER GOAL (`age.go:112-137`).
6. Schema bumps to v1.3, purely additive.

## Architecture

Bind in `hookevent.go`, map in `mapper.go`, gate at `client.stripContent` as the single
choke point. No new plumbing: `Content.Output` and `activity_output` already exist and are
already stripped by default.

`error` is one JSON key on two hooks — a closed enum on `StopFailure`, free text on
`PostToolUseFailure`. The existing `enumOr` allowlist keeps the free text off `error_type`;
this phase routes the same string to a *gated content* field instead of dropping it.

## Related code files

| Path | Change |
|---|---|
| `adapters/claude-code/hookevent.go` | bind `tool_response`, `error` free text, `reason`, `error_details` |
| `adapters/claude-code/mapper.go` | map to `Content.Output` / `activity_output`, observe-path input |
| `client/event.go` | add `Content.ToolOutput` — decided, not conditional: `Content.Output` is already occupied by ADR-0018 turn text (`mapper.go:397`), confirmed in plan review |
| `client/payload.go` | `structuralActivityOutput` gains the gated branch |
| `contracts/dev-event/schema/dev-event.schema.json` | v1.3 |
| `contracts/dev-event/conformance/` | new cases; C19 updated |
| `docs/data-and-privacy.md` | "Tool output: never" → gated row |
| `contracts/dev-event/COVERAGE.md` | §3 item 4 updated |

## Implementation steps

1. Write conformance cases FIRST: gated-on egress, gated-off absence, redact-before-send on
   outbound bytes, 64KB cap, `signal_args` still empty on PermissionDenied.
2. Bind the four fields in `hookevent.go` with capture-off inertness.
3. Map in `mapper.go`; observe-path input mapped separately from `enforcetarget.go`'s copy.
4. Route redaction through `decision.Redactor.RedactText` before attach.
5. Schema v1.3 + `x-schema-version` note.
6. Update C19 to assert the gate rather than the absence.
7. Docs: privacy table, COVERAGE, MAPPING §1/§3.

## Todo

- [x] Conformance cases written FIRST and red before the binding code (C32–C39)
- [x] `tool_response` bound and mapped → `activity_output.output`
- [x] Observe-path tool input (`Mapper.Map` HookPreToolUse, reusing `toolInputExtract`
      so redaction runs BEFORE the cap — see the review fixes below)
- [x] Three failure free-text fields — `error` → `Content.ToolOutput`; `reason` /
      `error_details` → `Content.SignalDetail` → `metadata.denial_reason` /
      `metadata.error_details`
- [x] Redact-before-attach asserted on outbound bytes (C34)
- [x] Schema v1.3 + `client.SchemaVersion`; every fixture bumped, two new gate fixtures
- [x] C19 updated, not deleted — plus the whole capture-off half re-homed onto the
      gate (`TestMap_NoContentLeak`, C33, C36, `TestEscalation…ObserveNeverDoes`)
- [x] Privacy/COVERAGE/MAPPING/README/e2e docs true
- [x] 11 modules green under `-race`, both cross-compiles

Added beyond the original list, and why:

- [x] **C39 — which credential FORMATS the one control actually catches**, which is
      a different question from C34's ordering. C34 uses a pattern-covered AWS key,
      so it says nothing about what the detector recognizes. Tool output egresses at
      tool-call cadence and is where credentials actually surface, so C39 drives a
      dotenv dump and asserts per format. It found TWO limits, both now asserted to
      leak deliberately and both disclosed: (1) for generic secrets **the keyword
      decides, not the charset** — not fixable by lowering the entropy floor, since
      below 4.0 every git SHA and UUID matches and the enforce-path redactor
      REWRITES file bodies; (2) **nested JSON defeats both generic mechanisms**,
      because `tool_response` is JSON so a nested value arrives escaped and the
      backslash breaks `precededByAssignment`. The last leg is the control: the
      identical token, flat, in the same field, IS caught. (2) is fixable — OD-2.
- [x] `TestContentGate` now walks `testdata/content/` instead of a hardcoded list —
      a fixture for a new gated field was otherwise never validated, and "no test
      ran it" is indistinguishable from "it passed". It caught a bad `semantic_type`
      in the new fixture immediately.
- [x] Testbed inverted rather than deleted (`20-capture.sh` asserts the gate OPEN,
      `35-telemetry.sh` the gate CLOSED on its existing capture-off session). "The
      marker is nowhere" and "the runtime emitted nothing at all" are the same
      observation; only the positive form separates them.

## Success criteria

- Tool output present on 100% of PostToolUse events with capture ON; absent with OFF.
- MCP tool output present without MCP-specific code.
- Outbound bytes contain redaction placeholders, never the secret.
- `TestNewSignalsCarryNoSignalArgs` still passes.
- Every 1.2 event remains a valid 1.3 event.

## Risk assessment

| Risk | Mitigation | Signal it broke | Response |
|---|---|---|---|
| Redaction after attach | conformance asserts outbound bytes | a case passes with secret present | stop, fix ordering before merge |
| Free-text `error` leaks onto `error_type` | `enumOr` unchanged; separate field | `error_type` carries non-enum value | revert binding, re-scope |
| Volume rises at tool-call cadence | 64KB cap; measure in testbed | spool growth or core rejects | file backend retention ask (phase 08) |
| `Content.Output` collides with ADR-0018 turn text | decided: distinct `Content.ToolOutput` field (collision confirmed live, `mapper.go:397`) | turn text appears on tool events | the field split is the fix; never overload `Output` |

## Security considerations

- `secret_detection:false` now exposes four more classes. Document the widened consequence
  in `docs/data-and-privacy.md`; do not soften it.
- Tool output is where secrets actually surface — `env` dumps, `cat .env`, tokens in stderr.
  Local pattern/entropy detection is the only in-transit control.
- File bodies on observe events are new egress. Disclose as a widening with bounds.

## Outcome (2026-08-25)

**Status: implemented, unit- and conformance-verified — the testbed has NOT run.**
All 11 modules green under `-race`, gofmt clean, both cross-compiles OK.

What the evidence does and does not cover:

- **Strong.** C32–C39 drive the real `RunHook` observe path against a real
  `/evaluate` stub over HTTP and assert on the bytes actually POSTed. Gate on/off,
  redact-before-send, the 64KB cap, `signal_args` absence, and detector reach are
  all asserted there, not on a `DevEvent`.
- **Unproven without a live stack.** That core stores `activity_output.output` as
  the row's `output` and runs Guardrails stage 1 over it; that
  `metadata.denial_reason` / `metadata.error_details` survive ingest; and the
  VOLUME question — 64KB bodies at tool-call cadence through the realtime flusher.
  `testbed/20-capture.sh` and `35-telemetry.sh` carry the dormant assertions for
  the first two.

Three things worth not re-litigating:

- **SL3-SEC-3 is retired, and what replaces it is weaker in kind.** It was an
  unconditional structural guarantee — tool content had no field to land in. It is
  now a gate plus a redaction plus a cap, each of which can be got wrong. That is
  why all three are asserted on outbound bytes rather than inferred from a missing
  field, and why the capture-OFF half is asserted everywhere the capture-ON half is.
- **`Content.Output` must never carry tool output.** It carries the ADR-0018 turn
  text that feeds core's goal-alignment extractor. The distinct `Content.ToolOutput`
  field is the fix; overloading one would put turn text on tool events and tool
  output into alignment the moment either mapping slipped.
- **`reason` and `error` are each ONE JSON key on TWO hooks**, and only the routing
  keeps them apart: `reason` is a closed enum on SessionEnd and free text on
  PermissionDenied; `error` is a closed provider enum on StopFailure and free text
  on PostToolUseFailure. The ungated enum fields must never receive the free text —
  which is why the wire keys are `denial_reason` / `error_details` rather than the
  provider's own names, and why `TestMap_FreeTextErrorNeverEgresses` now asserts
  both postures instead of only absence.

## Owner decisions from the review — both RESOLVED (2026-08-25)

- **OD-1 — the escalated shell/MCP `/evaluate` copy carries the command verbatim.**
  **Decided: deliberate, document it.** A policy deciding whether a command is
  dangerous must see the command that will actually run; redacting first would have
  the server judge text that differs from what executes, and a rule matching a
  credential-shaped argument would stop firing. Nothing here is written back to the
  machine, so there is no reconstruction to keep faithful. Recorded in ADR-0017
  §Content (amended) and in `docs/data-and-privacy.md`. The asymmetry it creates is
  stated rather than hidden: the OBSERVE copy of that same call IS redacted, so
  ordinary telemetry is better protected than the enforcement copy.
- **OD-2 — nested JSON defeated BOTH generic detection mechanisms.**
  **Decided: fixed.** `secret_assignment` now tolerates quoting/escaping between
  the keyword and the separator, and `precededByAssignment` skips the JSON escape.
  Both widenings can only match MORE, and every extra match is unambiguously a
  secret assignment. Two directions are pinned because both could regress:
  the redaction must not swallow the `\` that terminates a JSON string (or the
  body stops being parseable on the wire), and a backslash-bearing value such as
  `password=C:\Users\…` must still be redacted whole — which is why the value
  pattern refuses a backslash only as the LAST character rather than excluding it
  outright. Fuzzed (246k execs, clean).

## Next steps

Phase 02 reuses this gate plumbing for thinking. One thing this phase changed for
it: `Content` now carries three content fields with three distinct wire slots, so
thinking needs its OWN field and slot decision — not a reuse of `Output`.
