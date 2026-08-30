# Phase 03 — Contracts, decision record and docs

## Context links

- Parent: [plan.md](plan.md)
- Depends on: [Phase 01](phase-01-wire-activity-lifecycle.md), [Phase 02](phase-02-retire-hook-span-machinery.md)
- Closed by: [Phase 04](phase-04-testbed-verification.md) (claims made here must be
  live-verified before they are asserted as fact)
- Rewrites: `contracts/dev-event/MAPPING.md`,

## Overview

- **Date:** 2026-08-11
- **Description:** Record the new wire mapping, supersede that decision's tool rows with a new
decision record, and correct the docs that assert span-level evidence and semantic
classification.
- **Priority:** P1 — the repo's own rule is that a governance product that overstates
  itself is the failure it exists to prevent. Leaving `MAPPING.md` describing hook spans
  would be exactly that.
- **Implementation status:** complete
- **Review status:** not started

## Key insights

1. **MAPPING.md §2 currently argues the opposite of what we are shipping**, at length and
   with citations ("A `ToolResult` is **not** `ActivityCompleted`", `:90-98`). It is not
   enough to flip the table rows — the reasoning must be replaced, or the next reader
   re-litigates a decided question. State plainly: the base SDK's rule binds *hook*
   events, and shift-left no longer emits any.
2. **A new decision record, not an edit to.** That decision is Accepted and its reversal of the
earlier `ToolResult→ActivityCompleted` draft is part of the record. Overwriting it
erases why the question was settled the first way. The new decision record supersedes
those rows and states what changed in the premise: no OTel in a hook-process runtime
means no span layer to attach to, so the tool call is modelled at the activity layer
instead.
3. **Two doc claims become false and one becomes unverifiable.**
   - Span-level Merkle leaves no longer exist for dev sessions →
     `docs/architecture.md#assurance--what-the-evidence-proves` must say what the
     evidence now proves (event leaves only).
   - MAPPING.md §3's flat-`SpanData` table and §7's dashboard-pairing risk both describe
     a wire that no longer exists.
   - MAPPING.md:110's "E7-S2 dependency (server-side, pending)" is **already
     contradicted by observed data** — a live span carried
     `semantic_type: "shell_command"` while the local openbox-core checkout has no such
     constant. Do not restate it either way; either verify against the deployed core or
     delete the claim as unowned.
4. **`COVERAGE.md` reasons about `SubagentStart`/`SubagentStop` needing no lifecycle type
   "because `agent_id`/`agent_type` ride every payload" (MAPPING.md:86). That argument
   survives — those keys ride `metadata`, not the span — but verify rather than assume.

## Requirements

- MAPPING.md §1-§3, §5, §7 rewritten to the activity mapping; §4 (verdict) and §6
  (transport/signing) unchanged and re-confirmed.
- New decision record in  superseding that decision's `ToolCall`/`ToolResult` rows and its
§Amendment mirror obligation; that decision gains a one-line forward pointer
only.
- `README.md` index updated.
- `docs/architecture.md` assurance section states event-leaf-only evidence for dev
  sessions and drops any span-level claim.
- `docs/data-and-privacy.md` reflects that span `request_body`/`response_body` are gone
  as an egress channel, and that `activity_output` carries counts only.
- Every claim cites a repo symbol/path or an upstream doc URL, per repo convention.
- No claim asserted as verified until Phase 04 has run it.

## Architecture

Documentation ownership stays where it is: the adapter-facing schema is the contract, the
`MAPPING.md` wire layer is the serialization, and decision records carry the *why*. This
phase only moves text between those homes — it introduces no new doc surface.

The accepted trade-off gets one authoritative home (the new decision record) that the
other documents link to, rather than being re-explained in each.

## Related code files

| File | Change |
|---|---|
| `contracts/dev-event/MAPPING.md` | rewrite §1 (envelope), §2 (per-type + drop the "key correction" argument), §3 (replace flat-SpanData with activity_input/activity_output field homes), §5 (consumer behavior), §7 (live verification section) |
| `contracts/dev-event/COVERAGE.md` | re-check the subagent-boundary argument; update any span reference |
| `contracts/dev-event/README.md` | re-check the two-layer description |
| the decision record | **new** |
| — | forward pointer only; do not rewrite history |
| `README.md` | index entry |
| `docs/architecture.md` | assurance section: event leaves only for dev sessions |
| `docs/data-and-privacy.md` | span content channel removed; `activity_output` scope |
| `CLAUDE.md` | "Current state" paragraph, once Phase 04 passes |

## Implementation steps

1. Draft the new decision record first — Context (no OTel in a hook-process runtime; the span was
fabricated by hand), Decision (tool call = Activity; `ToolCall`→`ActivityStarted`,
`ToolResult`→`ActivityCompleted`; span layer retired), Consequences (zero span rows, no
span Merkle leaves, no `semantic_type` for dev sessions, that decision mirror obligation
dissolved, event volume unchanged), and the alternatives rejected with their evidence.
**If Phase 04 triggered the pre-authorized 3-POST fallback** (validation decision 3),
that decision records that shape instead, plus its own consequence: shift-left knowingly
diverges from the base SDK's "hooks are always `ActivityStarted`" rule, which is a local
divergence and not a cross-repo contract change.
2. Add the one-line supersession and the index entry.
3. Rewrite MAPPING.md §2's table and delete the "key correction" subsection, replacing it
with a short statement of the new premise and a link to that
decision.
4. Replace MAPPING.md §3 with a field-home table: every `DevEvent.Span` field → its
   destination (`activity_input`, `activity_output`, `metadata`, or unread). This table is
   the **authority on what the serializer reads** (validation decision 4) and the contract
   Phase 01 steps 6-8 implement; keep them in sync. Record explicitly that the
   adapter-facing schema is unchanged at v1.0 and that `Span.Stage` is retained but read
   by nothing, so a future span-bearing shape needs no adapter change. Record that
   `start_time`/`end_time` are deliberately not sent and `duration_ms` is
   (validation decision 2).
5. Update MAPPING.md §5's consumer table: session store unchanged; OPA now evaluates both
   activity events; guardrails eligible for both; the dashboard-pairing risk in §7 is
   resolved by construction (a real `ActivityStarted`/`ActivityCompleted` pair) — say so
   only after Phase 04 confirms it renders.
6. Resolve MAPPING.md:110: verify `semantic_type` classification against the **deployed**
   core, then either cite it or delete the E7-S2 claim as unowned. Record which.
7. Update `docs/architecture.md` assurance and `docs/data-and-privacy.md`.
8. Hold the `CLAUDE.md` "Current state" edit until Phase 04 is green, then state exactly
   what was verified and how.

## Todo list

- [x] New decision record drafted with alternatives and evidence —
- [x] that decision forward pointer + `README.md` index (that decision was also missing from the index; added)
- [x] MAPPING.md §1/§2/§5/§7 rewritten
- [x] MAPPING.md §3 → field-home table
- [x] MAPPING.md:110 semantic_type claim **deleted as unowned** — see below
- [x] COVERAGE.md / README.md re-checked
- [x] `docs/architecture.md` assurance updated
- [x] `docs/data-and-privacy.md` updated
- [x] `CLAUDE.md` current-state paragraph — written as *implemented, not live-verified*, not held blank

### The finding that reframed the change

Core's idempotency check keys on `(agent_id, workflow_id, run_id, activity_id,
event_type)` (`activities/governance/validation.go:96`). Under the hook shape a
tool call's two halves matched on **all five** — same `activity_id` by design,
both `ActivityStarted` — so the `ToolResult` POST hit the existing-event branch
(`governance_workflow.go:228-231`) and never created a row. The shared `span_id`
that decision chose as the pairing mechanism was also what made the span-dedup
check see nothing new.

So the completed half of every tool call was substantially a no-op: no row, no
independent evaluation. Because `event_type` is in the dedupe key, the new shape
recovers it. This is the strongest source-level argument for the change and it is
now the lead item in that decision's Context and Consequences — but it is still
*reading*, and Phase 04 is what proves it.

### Step 6 resolved: the `semantic_type` claim is deleted

MAPPING.md:110's "E7-S2 dependency (server-side, pending)" is gone rather than
restated. Three reasons, recorded in the doc itself: it was already contradicted
by observed data; the openbox-core checkout defines `mcp_tool_call`
(`session.go:111`) and **no** `shell_command` constant, so it could not be cited
as written; and it is now moot, because with no span there is nothing to
classify. An unowned claim in a governance product is worse than an
acknowledged gap.

### Scope taken beyond the phase file

The phase's related-code table did not list these, but leaving them would have
left false statements behind:

- `client/README.md` — its `semantic_type is set indirectly` section and its
`[EXT-core] dependency` section were both false (the latter already false before
this plan: it claimed dev event types are not accept-listed, which that decision
retired). Rewritten. Its idempotency section also claimed core cannot dedupe dev
events "because they have no activity_id" — tool events do, so it now states
which events dedupe and which do not.
- `CLAUDE.md`, root `README.md`, `docs/architecture.md` diagram — `spans` removed
  from the storage path.
- `contracts/dev-event/ext-core/README.md`, `adapters/codex/README.md`,
  `client/event.go`, `client/acceptancetest/acceptance_test.go` — stale
  `span_id`-pairing claims.
- **Three production adapter files** (`adapters/claude-code/mapper.go`,
  `adapters/codex/mapper.go`, `adapters/common/hookflow/duration.go`) —
  comment-only corrections to `span_id`/`buildHookSpan` references. This
  deliberately breaks Phase 01/02's "no production file under `adapters/`"
  constraint, which existed to prove the engine is not span-shaped. That is
  proven; these are stale sentences, and this repo's rule is that docs must be
  true.

## Success criteria

1. No document describes shift-left as emitting hook spans, `hook_trigger`, or
   `AssertHookWireShape`.
2. The accepted trade-off (no span rows, no span leaves, no `semantic_type`) is stated in
   exactly one authoritative place and linked from the others.
3. Every field a `DevEvent.Span` used to carry has a documented destination or an explicit
   "dropped, because —".
4. That decision still reads as a true historical record, with a pointer forward.
5. No sentence claims live verification that Phase 04 has not produced.

## Risk assessment

| Risk | Mitigation | Signal it broke | Pre-decided response |
|---|---|---|---|
| Docs overstate the new posture (e.g. imply a privacy improvement, or that pairing is fixed) before evidence exists | Step 8 gates the current-state claim on Phase 04; §5/§7 claims likewise | A reviewer cannot trace a sentence to a symbol, test, or run | Downgrade the sentence to a stated limit — the repo prefers an honest limit to a confident one |
| Rewriting that decision instead of superseding it, erasing why the first answer was chosen | Step 2 restricts that decision to a pointer | that decision's diff touches Context/Decision | Revert; supersession only |
| MAPPING.md §3's field-home table drifts from the implementation | It is authored as the contract Phase 01 step 6 implements, and the golden fixtures pin the result | A golden fixture has a field the table does not | Fix whichever is wrong, in the same change |
| The `semantic_type` question stays unresolved and gets restated as fact | Step 6 forces verify-or-delete | The claim survives with no citation | Delete it — an unowned claim in a governance product is worse than a gap |

**Assumption that may break:** that no downstream consumer depends on dev-session span
rows (dashboard span views, analytics, an openbox-backend query). Signal: Phase 04 finds a
dashboard surface that goes blank. Response: record it as a known limit in that decision
and raise it with the dashboard owner — do not reshape the wire to feed a UI.

## Security considerations

- `docs/data-and-privacy.md` must stay true: state that tool commands, file bodies and
  tool output remain absent from observe-path egress, that `activity_output` carries
  counts and an exit code, and that the Tier-2 escalation's content gate is unchanged.
- Do not claim the change reduces egress risk without saying what was measured; the span
  content channel appears to have been unused on the observe path, and "appears" is the
  honest word until Phase 04's capture confirms it.
- Note explicitly that span-level attestation is gone for dev sessions, so an auditor
  reading the Merkle tree sees event leaves only. Understating that would be the failure
  mode this repo exists to prevent.

## Next steps

Phase 04 runs the real thing and converts these claims into evidence — or sends them back.

<!-- Updated: Validation Session 1 - MAPPING.md §3 field-home table is the authority on what the serializer reads, records Span.Stage unread + schema frozen at v1.0 + duration_ms-only timing; decision record gains a conditional
3-POST-fallback branch -->
