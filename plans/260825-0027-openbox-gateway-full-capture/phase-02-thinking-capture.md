# Phase 02 — Thinking capture

## Context links

- Parent: [plan.md](plan.md) · Previous: [phase-01](phase-01-tool-content-capture.md)
- Authority: `docs/adr/ADR-0019-full-content-capture.md` P3 + **ADR-0014 amendment**
- Transcript evidence: [researcher-02](research/researcher-02-claude-code-interception-surfaces.md)
- Depends on: 01 (shares the content-gate plumbing)

## Overview

- Date: 2026-08-25
- Description: lift thinking blocks from the session transcript via the existing byte-offset
  cursor, and amend the ADR-0014 allowlist that currently forbids them.
- Priority: P1
- Implementation status: pending
- Review status: not reviewed

## Key insights

- **The transcript is the only source, and it already works.** `hookflow.TurnCursor`
  (ADR-0014) tails the session JSONL over a byte offset today for token counts. Content
  block types `text`, `thinking`, `tool_use`, `tool_result` confirmed present in 5 of 6 real
  sessions on this machine.
- **OTel can never substitute.** Anthropic redacts extended thinking unconditionally in
  every telemetry path — *"Extended-thinking content is redacted"* — with no flag.
- **The gateway would also get thinking**, from the raw API response. This phase exists so
  thinking does not wait for Track B, and remains the fallback if Track B is descoped.
- **The sentinel is load-bearing and changes direction.** `TestFinops_NoContentOnWire`
  evolves from *content absent* to *content present, redacted, capped*. **A version that
  passes trivially is a defect** — same rule ADR-0014 set, carried forward.
- **INV-2's allowlist is the thing being amended.** `usage.go` binds numeric fields plus
  `message.model`. Thinking is the first genuinely free-form transcript string. The
  amendment must be explicit in the ADR, not implied by the code.
- Format is undocumented and Anthropic-internal — accepted risk, stated in docs.

## Requirements

1. Thinking blocks from the `Stop`/`SubagentStop` transcript window → gated content field.
2. ADR-0014 allowlist amended in writing before the binding merges.
3. Sentinel evolved to assert redacted-and-capped presence, non-trivially.
4. Same ordering: detect → redact → attach → cap → sign.
5. Partition unchanged: `isSidechain` handling must not double-count a subagent's turn.

## Architecture

Extend the existing projection rather than adding a reader. The cursor already defines the
window; this widens what the projection lifts inside it. Spool-then-cursor ordering stays —
a crash over-reports into core's dedupe rather than losing a turn.

Thinking rides the turn's `activity_output`, not a new event type.

## Related code files

| Path | Change |
|---|---|
| `adapters/claude-code/usage.go` | projection widens; INV-2 allowlist comment updated |
| `adapters/common/hookflow/turncursor.go` | window unchanged; verify sidechain partition |
| `adapters/claude-code/usage_test.go` | sentinel evolves direction |
| `client/payload.go` | `turnActivityOutput` gains gated thinking |
| `docs/adr/ADR-0014-turn-as-activity-and-identifier-allowlist.md` | amendment |
| `docs/data-and-privacy.md` | "thinking: never" → gated row |

## Implementation steps

1. Amend ADR-0014 first — the allowlist change is the decision, the code is the consequence.
2. Evolve the sentinel with an adversarial section: capture ON + poisoned transcript ⇒ only
   the permitted fields egress, thinking redacted and capped.
3. Widen the projection to lift `thinking` blocks inside the existing window.
4. Attach gated; redact before attach.
5. Verify the sidechain partition still cannot double-count.
6. Docs.

## Todo

- [ ] ADR-0014 amendment merged first
- [ ] Sentinel evolved, adversarial section added
- [ ] Projection widened
- [ ] Redact-before-attach asserted on bytes
- [ ] Sidechain partition verified
- [ ] Privacy doc row flipped
- [ ] 11 modules green under `-race`

## Success criteria

- ≥1 thinking block captured per turn where extended thinking is active.
- Sentinel fails if the redaction or the cap is removed (prove by deleting each).
- Capture OFF ⇒ no thinking on the wire, byte-identical to pre-phase output.
- No turn counted twice across a main-agent + subagent session.

## Risk assessment

| Risk | Mitigation | Signal it broke | Response |
|---|---|---|---|
| Sentinel weakened into a trivial pass | mutation-test it: delete redaction, delete cap, expect failures | both deletions still green | block merge; the test is the control |
| Transcript format changes on a CC release | version-tolerant projection; absence degrades to no-thinking, never an error | thinking silently stops | accept degradation; alarm in testbed, not in the hook |
| Volume: thinking is large | 64KB cap, same as other classes | spool growth | measure in phase 08 before widening the cap |
| Capturing further than the provider will | owner decision, recorded in ADR | — | do not reverse silently |

## Security considerations

- This captures content **Anthropic's own telemetry refuses to export**. That is the org's
  call to make on its own machine, but it must be a decision someone made and signed, not a
  consequence of "capture everything". Record it in the ADR amendment explicitly.
- Thinking is high-density: it restates prompts, file contents, and credentials seen earlier
  in the turn. Redaction matters more here than anywhere else.

## Next steps

Track A complete. Thinking and tool I/O are captured without any new service. Phase 03
decides whether Track B proceeds.
