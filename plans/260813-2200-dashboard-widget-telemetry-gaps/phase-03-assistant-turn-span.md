# Phase 03 — the assistant-turn span, end to end

## Context links

- Plan: [plan.md](plan.md) · Blocked on: [phase 01 (ADR-0018)](phase-01-adr-0018-dev-turn-content-carrier.md),
  [phase 02](phase-02-activity-status-field.md) (same struct, same golden dir)
- Evidence: [scout-02 §Widgets 1 & 2](scout/scout-02-write-side-core-sdk-shiftleft.md),
  [scout-01 §1-2](scout/scout-01-read-side-fe-backend.md),
  [research 260813-2215 §2 + P2](../reports/research-260813-2215-session-content-capture-gaps.md)
- Core-side targets (read-only): `openbox-core internal/services/goal_alignment_session.go:64-88`,
  `internal/services/age.go:109-172`, `internal/services/governance_workflow.go:302-304`,
  `internal/content/session.go:236-256,326-333,451-476`

## Overview

- **Date:** 2026-08-13
- **Description:** Attach ONE minimal wire span to `TurnCompleted` carrying the assistant turn's
  text as `response_body` in the exact shape core's AGE extractor demands — content_capture-gated,
  secret-redacted before attach, `capBody`-capped. Unblocks Goal Alignment Trend and Recent Drift
  Events. Claude Code only; Codex deferred.
- **Priority:** P2 — the largest change and the one that widens egress.
- **Implementation status:** pending
- **Review status:** pending
- **Effort:** 4h

## Key Insights

1. **The carrier is dictated three times over.** Core's AGE takes assistant content ONLY from
   `payload.Spans`, only from the LAST span, only when
   `Stage=="completed" && SemanticType==llm_completion && ResponseBody!=nil`
   (`goal_alignment_session.go:64-72`), and unmarshals it as
   `{"choices":[{"message":{"content":"…"}}]}`, returning `choices[0].message.content`
   (`goal_alignment_session.go:73-88`). Any other shape logs "response_body was not valid JSON"
   and yields "". No freedom here — match it exactly.
2. **`semantic_type` cannot be asserted.** The workflow overwrites it for every span before policy
   evaluation: `Spans[i].SemanticType = ComputeSemanticTypeFromSpan(&Spans[i])`
   (`governance_workflow.go:302-304`). The only classifier paths returning `llm_completion` are
   `isLLMCall` — attributes `http.method=="POST"` **and** `http.url` containing an LLM domain
   (`session.go:451-476`, domains at `session.go:137-149`) — and its root-field twin
   (`session.go:236-256`). `gen_ai.system` yields `llm_gen_ai`, which the AGE check rejects. So the
   span carries **synthesized classification keys**; that is OD-0018-1, decided in phase 1.
3. **Span ids must be deterministic.** Core's span dedupe key is `(span_id, stage)` scoped by
   session (`internal/content/governance.go:255-262`), and `hasNewSpan` drives extra workflow
   branches (`governance_workflow.go:230,499,753`). A random span id on a re-reported turn (the
   crash path `emitTurn` deliberately allows — `adapters/claude-code/hookrun.go:286-291`) would
   look like a NEW span. Derive `span_id`/`trace_id` by hash so a re-report is byte-identical and
   absorbed exactly as today.
4. **`hook_trigger` stays unset.** With it true and spans present, the payload enters the
   approval-bypass fingerprint path (`governance_workflow.go:310-330`). A turn is never approved;
   keep it out.
5. **The text source is the hook payload, not the transcript.** `Stop`/`SubagentStop` carry
   `last_assistant_message` (`adapters/claude-code/hookevent.go:119-124` documents it as present
   and deliberately unbound); the provider's own docs call it the recommended source because the
   transcript lags. Using it leaves `usage.go`'s INV-2 allowlist and the load-bearing sentinel
   `TestFinops_NoContentOnWire` (`adapters/claude-code/usage_test.go:485`) **untouched** — the
   sentinel keeps proving transcript content cannot reach the wire, and gains one assertion.
   `stop_reason` stays unbound (not needed; would be a second content-ish field for nothing).
6. **The gates compose, in both directions.** Turn events exist only under
   `ResolveFinops()` (`hookrun.go:174-183`) and only when the window had usage
   (`mapper.go:262-264`). The span exists only when `Content` survived — i.e. content capture on
   (`client/client.go:180-183` `stripContent`). And core's Redis goal session is CREATED by
   `SignalReceived` with non-empty `signal_args` (`age.go:113-137`), which shift-left sends only
   under content capture (`client/payload.go:641-650`); the append Lua **returns 0 when no session
   exists** (`goal_alignment_session.go:33-41`). So: capture off ⇒ no session, no span, nothing
   accumulates, nothing triggers. Capture on ⇒ prompt-then-turn, which is the natural order.
7. **Redaction must be structural, not remembered.** Inject a redactor into `Mapper` (the repo's
   existing collaborator idiom: `Now`, `NewID`, `Finops`, `Posture`, `Evidence`, all "RunHook
   resolves it and passes it in, so Map stays I/O-free" — `mapper.go:37-77`), so every path that
   produces a turn event redacts by construction rather than by the author remembering to.

## Requirements

- R1: Exactly one span, on `TurnCompleted` only, and only when the assistant text is non-empty
  after gating and redaction. No span on `TurnStarted`, tool events, or lifecycle events.
- R2: `stage:"completed"`; attributes carry the classification keys; `response_body` carries the
  OpenAI-chat wrapper; `semantic_type` may be sent for readability but MUST NOT be relied on.
- R3: `hook_trigger` absent/false. `span_count` = 1.
- R4: `span_id`/`trace_id` deterministic and stable across a re-report of the same turn.
- R5: Text is redacted by local secret detection **before** attach, then `capBody`-capped
  (`client/payload.go:694-715`), cap applied to the TEXT before JSON-wrapping.
- R6: With `content_capture:false`: no `spans`, no `span_count`, no `response_body` — nothing new
  on the wire at all.
- R7: `activity_id`, `event_id`, approval keys byte-unchanged (`deriveID` hashes an explicit field
  list that excludes `Content`, `mapper.go:641-690`).
- R8: Codex unchanged in behaviour; its non-support documented, not silently absent.
- R9: All 11 modules green under `-race`; both cross-compiles build.

## Architecture

```
Stop / SubagentStop
  └─ hookrun.go emitTurn (finops-gated, window must have usage)
       └─ Mapper.MapTurn(e, window, index)
            ├─ CaptureContent? ── no ─▶ no Content, no span   (R6)
            └─ yes ─▶ m.RedactContent(e.LastAssistantMessage)
                       └─ completed.Content = &client.Content{Output: redacted}
  └─ spool ─▶ flush ─▶ client.Emit
       ├─ !contentOn ⇒ stripContent nils Content ⇒ no span (second, independent gate)
       └─ buildPayload: case EventTurnCompleted ⇒ p.Spans = turnAssistantSpan(ev); p.SpanCount = 1
```

New file `client/turnspan.go`:

```go
// wireSpan is the MINIMAL subset of core's SpanData (internal/content/governance.go:275-330)
// this client sends. One carrier, one purpose: feed AGE's assistant-content accumulator.
type wireSpan struct {
    SpanID       string         `json:"span_id"`
    TraceID      string         `json:"trace_id"`
    Name         string         `json:"name"`
    Kind         string         `json:"kind,omitempty"`
    Stage        string         `json:"stage"`
    StartTime    int64          `json:"start_time"`
    EndTime      int64          `json:"end_time"`
    Attributes   map[string]any `json:"attributes"`
    ResponseBody string         `json:"response_body"`
    SemanticType string         `json:"semantic_type,omitempty"`
}
```

- `Name`: `"llm_completion"` — must NOT uppercase-contain `EMBED` or `TOOL`, which would classify
  as `llm_embedding` / `llm_tool_call` (`session.go:327-332`). `"llm_completion"` is safe.
- `Attributes`: `{"http.method":"POST","http.url":"https://api.anthropic.com/v1/messages",
  "openbox.span_synthetic":true}` — the first two are classification keys for
  `isLLMCall` (`session.go:451-476`); the third states on the record that this span is derived from
  a hook, not an observed request. Never content.
- `ResponseBody`: `{"choices":[{"message":{"content":<capped redacted text>}}]}`, produced with
  `json.Marshal` so escaping is correct, then carried as a string (core's field is `*string`).
- `SpanID = hex(sha256("turnspan\x1f" + activity_id))[:16]`,
  `TraceID = hex(sha256("turntrace\x1f" + session_id))[:32]` — 16/32 lowercase hex, deterministic.
- `StartTime`/`EndTime`: epoch **nanoseconds** from `ev.StartedAt`/`ev.EndedAt` via the existing
  `rfc3339Nanos` (`client/payload.go:726-735`); core's SpanData uses nanos (`duration_ns` sibling).

`governanceEventPayload` (`client/payload.go:28-78`) gains, appended after phase 02's `Status`:

```go
    Spans     []wireSpan `json:"spans,omitempty"`
    SpanCount int        `json:"span_count,omitempty"`
```

Redaction seam — `adapters/common/hookflow` (provider-agnostic, next to `NewDecider()` at
`enforce.go:155`):

```go
// RedactText redacts secrets in a free-text content body before it is attached
// to an event. Bounded by MaxRedactBody: an oversized body is returned unchanged
// (fail-open, same skip-not-truncate rule as the file-body path, enforce.go:125-134).
func RedactText(s string) (string, []string)
```

It needs an exported entry point on `decision.Redactor` (today only `Decide` is exported and it
reads `Content.FileText`, `decision/redact.go:75-87`). Add `func (r *Redactor) RedactText(s string)
(string, []string, bool)` wrapping the same `r.scanner.Redact` — no second detector (DRY).
Rejected alternative: call `Decide` with the text stuffed into `Content.FileText` — works, but
mislabels assistant text as a file body in every log and category record.

`Mapper` (`adapters/claude-code/mapper.go:37-77`) gains
`RedactContent func(string) string` (nil ⇒ identity), wired in `hookrun.go` beside
`ad.Mapper.CaptureContent = ResolveContentCapture()` (`hookrun.go:109`), gated on
`ResolveSecretDetection()` (`adapters/common/devconfig/devconfig.go:367`).

**Codex, deferred — reasons (record in `capabilities.go`, not just here):** its `HookEvent` binds no
assistant-text field and its `Stop` hook is deliberately unwired
(`adapters/codex/capabilities.go:15`), so there is no per-turn boundary to attach to; its only turn
activity is the SessionEnd `<session>:usage:rollup` (`adapters/codex/mapper.go:273-275`), emitted in
the same flush as `SessionEnded`→`WorkflowCompleted` — and that branch DELETES the goal session
(`age.go:158-160`), so ordering decides whether the append lands at all, and one message is a
degenerate alignment trace. Sourcing text from the rollout JSONL would widen a numbers-only
projection, which is the exact cost this phase avoids on Claude Code.

## Related code files

| Path | Change |
|---|---|
| `client/turnspan.go` | **new** — `wireSpan`, `turnAssistantSpan(ev)`, the two id derivations |
| `client/payload.go:28-78` | add `Spans`, `SpanCount` (after `Status`) |
| `client/payload.go:154-160` | `case EventTurnCompleted:` set `p.Spans`/`p.SpanCount` |
| `client/payload.go:319-370` | amend `turnActivityOutput`'s INV-2 doc block — the turn now MAY carry gated text, in the span, not here |
| `client/payload.go:677-690` | `stripContent` — verify it nils `Content` (it does); no change |
| `decision/redact.go:60-90` | add exported `RedactText` on `Redactor` |
| `adapters/common/hookflow/enforce.go:125-155` | add `RedactText` helper next to `NewDecider` |
| `adapters/claude-code/hookevent.go:119-136` | bind `LastAssistantMessage string \`json:"last_assistant_message"\``; rewrite the "absence is the safeguard" comment to say what changed and what did NOT (`stop_reason` stays unbound) |
| `adapters/claude-code/mapper.go:37-77` | `RedactContent` field + doc |
| `adapters/claude-code/mapper.go:262-318` | `MapTurn`: attach gated+redacted `Content.Output` to the completed half only |
| `adapters/claude-code/hookrun.go:109` | wire `RedactContent` from `ResolveSecretDetection()` |
| `adapters/codex/capabilities.go` | one entry: alignment feed unsupported + why |
| `client/testdata/golden/activity_turn_completed.json`, `activity_turn_subagent_completed.json` | regenerate; add a content-on turn fixture (e.g. `activity_turn_completed_content.json`) |
| `client/golden_test.go` (`goldenCases()`) | add the content-on turn case |
| `client/leakscan_test.go` | add a `TurnCompleted` + `Content.Output` case (capture off ⇒ canary absent) |
| `client/payload_test.go` | span shape/id determinism/cap/absence unit tests |
| `adapters/claude-code/enforce_conformance_test.go` | new cases C23-C25 (see step 8) |
| `adapters/claude-code/usage_test.go:485` | EXTEND `TestFinops_NoContentOnWire` (never weaken): transcript-sourced text still absent; hook-sourced text present only under capture |
| `adapters/claude-code/mapper_test.go`, `turn_runhook_test.go` | gating + redaction-ordering tests |

`MapTurn` callers to keep compiling (all 3, no signature change under the injected-field design):
`adapters/claude-code/hookrun.go:324`, `adapters/claude-code/usage_test.go:530`, `:619`.

Do **not** touch: `adapters/claude-code/usage.go` (the projection), `client/hookspan.go` /
`client/spanbuilder.go` (stay deleted — this is not a revival of the span layer),
`client/approval_key_pin_test.go`, `client/turn_key_pin_test.go`, `go.mod` files.

## Implementation Steps

1. Recover the retired wire-span shape for reference only:
   `git show 01166bd^:client/spanbuilder.go` and `git show 01166bd^:client/hookspan.go`
   (commit `01166bd` "feat(client)!: emit tool calls as activity events, retire the hook span").
   Take the id formats and the flat-SpanData layout; do **not** restore the files, the 14-field
   family tuples, or `AssertHookWireShape` — this span is minimal by design.
2. `decision/redact.go`: add `RedactText`; unit-test that it redacts the same corpus `Decide` does
   and returns `changed=false` untouched input.
3. `adapters/common/hookflow`: add `RedactText(s string) (string, []string)` — bounded by
   `MaxRedactBody`, oversized returns input unchanged (fail-open, documented).
4. `adapters/claude-code/hookevent.go`: bind `LastAssistantMessage`. Update the doc comment
   honestly: which field is now bound, under which gate, and that `stop_reason` remains unbound.
5. `adapters/claude-code/mapper.go`: add `RedactContent`; in `MapTurn`, on the **completed** half
   only, `if m.CaptureContent && e.LastAssistantMessage != ""` set
   `Content{Output: m.redactContent(e.LastAssistantMessage)}`. Do not touch the started half, the
   metadata, or `Model`/`Tokens`.
6. `adapters/claude-code/hookrun.go:109`: wire the redactor (nil when `ResolveSecretDetection()`
   is false — the honest degradation, documented).
7. `client/turnspan.go` + `client/payload.go`: build and attach the span for
   `EventTurnCompleted` when `ev.Content != nil && ev.Content.Output != ""`; cap the text with
   `capBody` BEFORE wrapping; return nil so both keys stay omitted otherwise.
8. Tests:
   - unit: span present/absent per gate; `response_body` parses to the exact wrapper; ids are
     16/32 hex and identical across two builds of the same event; cap applied at 64K runes;
     `SpanCount==len(Spans)==1`; no `hook_trigger` key on the wire; name is not EMBED/TOOL-tainted.
   - conformance (`adapters/claude-code/enforce_conformance_test.go`, naming `t.Run("C<NN> …")`,
     next free number after phase 02's):
     `C23 a turn span carries the assistant text only with content capture on`,
     `C24 content_capture:false emits no span and no span_count`,
     `C25 a secret in the assistant text never reaches /evaluate` (mirrors C18's
     assert-on-outbound-bytes discipline, `enforce_conformance_test.go:256`).
   - extend `TestFinops_NoContentOnWire` with the two new assertions; leave every existing
     assertion in place. If the change makes it pass trivially, it is a defect (CLAUDE.md).
9. Regenerate goldens (`go test ./client -run Golden -update`) and READ the diff: turn-completed
   fixtures gain `spans`/`span_count` only in the content-on case; tool fixtures unchanged;
   `activity_id`/`event_id` unchanged anywhere.
10. `go test ./... -race` per module; both cross-compiles.
11. Commits: `feat(client): carry the assistant turn text on one wire span`,
    `feat(claude-code): bind the assistant turn message under the content gate`,
    `feat(decision): expose text redaction for content bodies`.

## Todo list

- [ ] `decision.Redactor.RedactText` + tests
- [ ] `hookflow.RedactText` (bounded, fail-open)
- [ ] `HookEvent.LastAssistantMessage` + honest doc comment rewrite
- [ ] `Mapper.RedactContent` + `MapTurn` attach (completed half only, gated)
- [ ] `hookrun.go` wiring from `ResolveSecretDetection()`
- [ ] `client/turnspan.go`: `wireSpan`, deterministic ids, OpenAI-chat wrapper, cap-before-wrap
- [ ] `payload.go`: `Spans`/`SpanCount` fields + `EventTurnCompleted` arm + amended INV-2 doc
- [ ] Codex non-support entry in `capabilities.go`
- [ ] Unit + C23/C24/C25 conformance + leakscan turn case
- [ ] `TestFinops_NoContentOnWire` extended, never weakened
- [ ] Goldens regenerated, diff read
- [ ] 11 modules `-race` green; both cross-compiles

## Success Criteria

- A `TurnCompleted` built with content capture ON carries exactly one span whose
  `stage=="completed"`, whose attributes satisfy core's `isLLMCall`, and whose `response_body`
  unmarshals to `choices[0].message.content == <the assistant text>`.
- The same event with capture OFF carries no `spans`, no `span_count`, no assistant text anywhere
  in the outbound bytes (asserted on bytes, C24 + leakscan).
- A secret placed in the assistant text does not appear in the outbound bytes (C25).
- Two independent builds of the same turn event produce byte-identical `span_id`/`trace_id`.
- `client/approval_key_pin_test.go`, `client/turn_key_pin_test.go` green with zero edits.
- `TestFinops_NoContentOnWire` green, strictly stronger than before (diff shows added assertions,
  no removed or relaxed ones).
- No `spans` key on any tool, signal or lifecycle payload.
- Codex behaviour byte-identical to before this phase (its golden fixtures unchanged).

## Risk Assessment

| Risk | L×I | Mitigation / signal & pre-decided response |
|---|---|---|
| `last_assistant_message` absent on `SubagentStop` (or renamed) | M×M | Gate is `!= ""`, so absence ⇒ no span, no error. **Signal:** phase 5 live check shows alignment rows for main-thread turns only. **Response:** adjust in-plan — document subagent turns as unfed; do NOT reach into the transcript to compensate |
| Field absent on BOTH hooks (provider changed) | L×H | **Signal:** step 8 unit test against a real captured `Stop` payload finds nothing to bind. **Response:** stop-and-replan onto the brief's original source (transcript projection + INV-2 allowlist amendment), which then needs an ADR-0018 amendment before coding |
| Core classifies the span as something other than `llm_completion` ⇒ AGE still silent | M×H | Classification traced through `ComputeSemanticTypeFromSpan` → `ComputeSemanticType` → `classifyMCPType`(skip) → `classifyLLMType`(hit). **Signal:** `spans.span_type != 'llm_completion'` in the live check. **Response:** adjust in-plan — add the root `http_method`/`http_url` twin (`session.go:236-256`), the documented second path |
| Synthesized `http.url` triggers an org policy keyed on HTTP egress | M×M | Named in ADR-0018; turn events are never gated (`Stop` writes no stdout, `hookrun.go:176-184`), so a verdict on them cannot block. **Signal:** noisy violations on turn events. **Response:** adjust in-plan — drop `http.url` to the root-field variant or revisit OD-0018-1 |
| Span rows + Merkle span leaves + `age_span_evaluations` reappear unexpectedly for dev sessions | H×L | Accepted and pre-recorded in ADR-0018 consequences (`openbox-core .../storage_event.go:146,189-197`). Tool Health is unaffected: its `span_tools` CTE selects `span_type='mcp_tool_call'` only (scout-01:269-284), so no double counting |
| Re-reported turn stores a second span row | M×M | Deterministic ids + core's `(span_id, stage)` dedupe. **Signal:** duplicate spans for one `activity_id`. **Response:** adjust in-plan; the irreducible lost-200 window stays a documented server-side ask |
| Redaction forgotten on a future path | M×H | Structural: redaction lives on the `Mapper` collaborator, so every `MapTurn` output is redacted by construction; C25 asserts on outbound bytes |
| Alignment still silent because LlamaFirewall is unset | M×M | `performTraceCheck` returns nil when `LlamaFirewallHost==""` (`llama_firewall.go:31-34`). Documented in phase 5 as a precondition, not a defect of this phase |

## Security Considerations

- **This phase is the egress change.** Assistant turn text leaves the machine when content capture
  is on. It is redacted (better than the prompt, which egresses unredacted today — a known repo
  limit, out of scope here), capped at 64KB, and absent entirely when capture is off.
- Redact **before** attach — the repo's only in-transit control (`adapters/*/enforcetarget.go`,
  conformance C18). C25 must assert on the outbound bytes, not on an intermediate value.
- Bind exactly one new string on `HookEvent`. Do NOT bind `stop_reason`, `tool_response`, or
  anything from the transcript in this phase; each would be its own decision.
- Thinking blocks are never captured — `last_assistant_message` is the final text only, and the
  provider's own OTel export redacts thinking unconditionally. Keep that stance in code comments.
- With `secret_detection:false` the text egresses unredacted: state it in the ADR and in
  `docs/data-and-privacy.md` (phase 4), never imply otherwise.
- Server-side, the assistant text becomes visible to Guardrails/OPA and is stored in `spans` +
  hashed into Merkle leaves. That is a real retention increase, named in ADR-0018 consequences and
  out of shift-left's control (backend question).

## Next steps

Phase 4 — reconcile every contract and doc surface with what phases 2 and 3 actually shipped.
