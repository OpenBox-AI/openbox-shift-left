# Phase 02 — Thinking capture

## Context links

- Parent: [plan.md](plan.md) · Previous: [phase-01](phase-01-tool-content-capture.md)
- Authority:  P3 + **that decision amendment**
- Transcript evidence: [researcher-02](research/researcher-02-claude-code-interception-surfaces.md)
- Depends on: 01 (shares the content-gate plumbing)

## Overview

- Date: 2026-08-25
- Description: lift thinking blocks from the session transcript via the existing byte-offset
cursor, and amend that decision allowlist that currently forbids them.
- Priority: P1
- Implementation status: **implemented** (testbed dormant)
- Review status: **reviewed 2026-08-25 (code-reviewer) — DONE, no blocking findings.**
  All 6 acceptance criteria verified, incl. independently re-running both sentinel
  mutation drills (cap removed / redaction removed) from scratch — both fail red,
  matching the commit's claims exactly. All 11 modules green under `-race`
  (fresh run), gofmt/vet clean repo-wide, both cross-compiles clean for the two
  touched modules. Three low-severity nits only (doc wording, a comment/code
  strictness mismatch, a missing exact-boundary unit test) — see
  `plans/reports/review-260825-1029-thinking-capture.md`. **All three are fixed**
  (`e6fe191`, `836f297`), along with four gaps a parallel cleanup pass found: a
  missing thinking row in the adapter-local README, a comment in
  `testbed/20-capture.sh` that stated a requirement and asserted nothing (the real
  span non-leak assertion now lives in `35-telemetry.sh`, where a span is expected
  to exist), and three copies of one truncation primitive collapsed into
  `hookflow.TruncateBytes`.

  One finding from that pass was NOT phase-02 scope and was fixed separately
  (`627ce54`): the `secret_assignment` value group had been narrowed in `a391a0e`
  so a value of exactly 8 characters ending in a backslash matched nothing at all.
  The JSON-escape boundary moved into the replacement step; both directions are
  now pinned together.

## Key insights

- **The transcript is the only source, and it already works.** `hookflow.TurnCursor`
tails the session JSONL over a byte offset today for token counts. Content block types
`text`, `thinking`, `tool_use`, `tool_result` confirmed present in 5 of 6 real sessions on
this machine.
- **OTel can never substitute.** Anthropic redacts extended thinking unconditionally in
  every telemetry path — *"Extended-thinking content is redacted"* — with no flag.
- **The gateway would also get thinking**, from the raw API response. This phase exists so
  thinking does not wait for Track B, and remains the fallback if Track B is descoped.
- **The sentinel is load-bearing and changes direction.** `TestFinops_NoContentOnWire`
evolves from *content absent* to *content present, redacted, capped*. **A version that
passes trivially is a defect** — same rule that decision set, carried forward.
- **INV-2's allowlist is the thing being amended.** `usage.go` binds numeric fields plus
`message.model`. Thinking is the first genuinely free-form transcript string. The
amendment must be explicit in that decision, not implied by the code.
- Format is undocumented and Anthropic-internal — accepted risk, stated in docs.

## Requirements

1. Thinking blocks from the `Stop`/`SubagentStop` transcript window → gated content field.
2. That decision allowlist amended in writing before the binding merges.
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
| — | amendment |
| `docs/data-and-privacy.md` | "thinking: never" → gated row |

## Implementation steps

1. Amend that decision first — the allowlist change is the decision, the code is the consequence.
2. Evolve the sentinel with an adversarial section: capture ON + poisoned transcript ⇒ only
   the permitted fields egress, thinking redacted and capped.
3. Widen the projection to lift `thinking` blocks inside the existing window.
4. Attach gated; redact before attach.
5. Verify the sidechain partition still cannot double-count.
6. Docs.

## Todo

- [x] that decision amendment written first — the amendment section at the end of the
decision record carries the mechanism table (gate/redact/cap), the scope limit,
and the cost
- [x] Sentinel evolved: `sentinels` split so `SENTINEL_THINKING` is posture-
  dependent, capture-OFF half tightened to include it, capture-ON half asserts
  presence in the decoded `activity_output.thinking`
- [x] **Both mutation drills performed literally.** Cap removed ⇒ red
  ("70084 runes, want <= 65536" + the tail marker on the wire); redaction removed
  ⇒ red (raw AWS key in the outbound body). Restored, green.
- [x] Projection widened — `message.content` bound as `json.RawMessage`, decoded
  only far enough to find `thinking` blocks
- [x] Redact-before-attach asserted on bytes (sentinel §g, and C40/C41 through
  real `RunHook` over HTTP)
- [x] Sidechain partition verified for thinking, at BOTH levels: the parser
  (`TestTurnWindow_PartitionsSidechainOut` now asserts the subagent lifts its own
  block and the parent lifts none of it) and the wire (`SENTINEL_SIDETHINKING` in
  the always-forbidden list)
- [x] Span non-leak asserted — thinking must not reach the one span core reads as
  the assistant's REPLY (sentinel §f + C40)
- [x] Privacy doc rows flipped (3 sites in `docs/data-and-privacy.md`), plus a
  pre-existing FALSE claim fixed in `README.md` left over from phase 01
- [x] Contract v1.4: schema, `content.thinking` property, changelog entry, all
  conformance testdata, a new gated fixture, the golden wire bytes
- [x] Dormant testbed assertions (`20-capture.sh` gate open, `35-telemetry.sh`
  gate closed) + `MAPPING.md` §7 items 22–24
- [x] 11 modules green under `-race`, both cross-compiles clean, `gofmt` clean

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
| Capturing further than the provider will | owner decision, recorded in decision record | — | do not reverse silently |

## Security considerations

- This captures content **Anthropic's own telemetry refuses to export**. That is the org's
call to make on its own machine, but it must be a decision someone made and signed, not a
consequence of "capture everything". Record it in that decision amendment explicitly.
- Thinking is high-density: it restates prompts, file contents, and credentials seen earlier
  in the turn. Redaction matters more here than anywhere else.

## Next steps

Track A complete. Thinking and tool I/O are captured without any new service.

**What shipped differently from this plan, and why.** Two deliberate narrowings:

- **Intermediate assistant text was NOT bound.** The phase file paired it with
thinking ("thinking blocks and intermediate assistant text"). The final reply
already egresses from the hook field that decision bound, so a second text source
would have widened the allowlist twice for one reader. That decision records it as
still open, needing its own amendment.
- **The lift is ungated; only the attachment is gated.** Gating the parser too was
  considered and cut: it buys nothing on the wire (the chunk the text was read from
  was already resident) and would put a second copy of the posture decision inside
  a pure function. What the parser owes instead is a bound, and the bound has its
  own guard test because the cap's mutation control depends on it.

**Unproven without a live stack** (`MAPPING.md` §7 items 22–24): that core stores
`activity_output.thinking` as its own key on the row, that the sibling key does not
perturb `ExtractModelMetricsFromActivity`, and the volume question — ≤64KB of
thinking per turn through the realtime flusher.

Phase 03 decides whether Track B proceeds.
