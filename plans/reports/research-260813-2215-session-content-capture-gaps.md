# Research Report: Enriching dev-session telemetry — trace, input, output, thoughts

Date: 2026-08-13 22:15 (+07) · Repos: openbox-shift-left @ `main` 7cfb50e · Claude Code 2.1.229 installed
Related: `plans/260813-2200-dashboard-widget-telemetry-gaps/scout/` (widget root causes, same session family)

## Executive Summary

The events look thin **by design, not by accident**. Shift-left's posture is INV-2 metadata-only: every field you
miss in the OpenBox UI — tool output, assistant response text, thinking, subagent prompts, success/failure — is
*available* at the Claude Code hook boundary or in the transcript, and the adapter **deliberately refuses to bind
it** ([hookevent.go:119-128](../../adapters/claude-code/hookevent.go),
[mapper.go:429-431](../../adapters/claude-code/mapper.go), [usage.go:30-54](../../adapters/claude-code/usage.go)).
Enriching is therefore a **privacy-posture decision first, code second** — and the repo's own rules route that
through a decision record + OD decision, not a commit.

Three tiers, by cost:

1. **Structural wins — no privacy change, no decision record** (recommend now): `status` on `ActivityCompleted`
   (this alone fixes the Tool Health Matrix 0% widget), wire `PostToolUseFailure`/`SubagentStart`/
   `PermissionDenied` hooks, map the `Task` tool's `subagent_type`. All identifiers/enums, INV-2-clean.
2. **Content-class extensions — decision record + OD decision required**: tool output (`tool_response`) and
   assistant response text (`last_assistant_message`) as content-gated, redacted, capped fields.
   Assistant text is also the **prerequisite for the Goal Alignment / Drift widgets** — but needs a
   matching openbox-core change (AGE reads assistant content only from spans, which dev sessions never write).
3. **Thinking — recommend against**: Anthropic's own OTel telemetry redacts thinking blocks
   unconditionally, even with every content flag enabled. Capturing it would exceed what the
   provider itself is willing to export.

## Research Methodology

- Sources: 9 repo files read (mapper, hookevent, usage, enforcetarget, payload, event, MAPPING.md,
  data-and-privacy.md, capabilities.go), 2 prior scout reports, 3 web lookups (Claude Code hooks
  reference, Claude Code OTel monitoring doc, OTel GenAI semconv status).
- Date range of external material: current as of 2026-08 (code.claude.com live docs).
- Search terms: Claude Code hooks tool_response last_assistant_message; OTel GenAI capture message content opt-in.

## Key Findings

### 1. What OpenBox receives today (per event, verified against code + your API sample)

| Wire event | Carries | Missing (and where it exists) |
|---|---|---|
| `WorkflowStarted` (SessionStarted) | cwd, posture, provider version, source | — (complete) |
| `SignalReceived` (PromptSubmitted) | **full prompt text** in `signal_args` (content-gated, 64KB cap) | — (complete) |
| `ActivityStarted` (ToolCall) | tool identity, `activity_input` locators; **command/file body only via the gated /evaluate copy** | Task/Agent prompt+description (in `tool_input`, dropped); with enforce OFF the command disappears too — see §3 note |
| `ActivityCompleted` (ToolResult) | `duration_ms`, tool identity | **`tool_response` text + `tool_output` JSON** (in PostToolUse stdin, unbound); **success/failure** (no `status` field sent — root cause of Tool Health 0%); `exit_code` (core promotion live, "no adapter supplies one today" — MAPPING.md:229) |
| `ActivityStarted/Completed` `llm_completion` (turn) | `turn_index`, model, 4 token counts, duration | **assistant response text** (`last_assistant_message` on Stop payload, deliberately unbound); **thinking** (transcript-only); turn's prompt (deliberate: rides PromptSubmitted instead, MAPPING.md:63) |
| `WorkflowCompleted` (SessionEnded) | reason, token/cost rollup, evidence state | — |

Sample confirmation: in your 161-event capture, every `ActivityCompleted` has `output: null` and no
status; `Agent` tool shows `input: {kind: "shell", tool_name: "Agent"}` — prompt/description dropped,
`kind: shell` is the documented catch-all fallback (mapper.go:487-492), not a bug.

### 2. Available-but-dropped inventory (the enrichment menu)

From the **hook stdin payloads** (verified against [code.claude.com/docs/en/hooks](https://code.claude.com/docs/en/hooks)):

| Data | Hook / field | Today |
|---|---|---|
| Tool output (text) | `PostToolUse.tool_response` | unbound — hookevent.go binds no field, "intentionally not decoded" |
| Tool output (structured) | `PostToolUse.tool_output` | unbound |
| Tool failure + reason | `PostToolUseFailure` event | **hook not wired** (adapter wires 7 of ~25 events) |
| Assistant final text | `Stop`/`SubagentStop.last_assistant_message` | payload field deliberately unbound (hookevent.go:119-128) |
| Stop reason | `Stop.stop_reason` | unbound |
| Subagent spawn + type | `SubagentStart` event | not wired (subagent tree today reconstructed only from `agent_id` on tool events) |
| Subagent prompt/description/type | `Task` `tool_input` | dropped (only `file_path`/`command` extracted from tool_input) |
| Permission denials | `PermissionDenied` (denial_reason, classifier_verdict) | not wired |
| API failures | `StopFailure` (error_type: rate_limit, billing…) | not wired |
| Model | `SessionStart.model` | already captured |

From the **transcript JSONL** (allowlist projection currently binds numbers + `model` + `isSidechain` + timestamp only):
full per-message trace — `message.content[]` with `text`, `thinking`, `tool_use`, `tool_result` blocks.
Confirmed present in real transcripts (usage.go:43-48, measurement report 260811). This is the only
source for thinking and for per-model-call (sub-turn) granularity; hooks deliver neither
(hooks carry **no token usage at all** — docs "Key Observations").

Notably: **hook payloads carry token usage nowhere** — the existing transcript projection stays the
only usage source. And `Stop`'s `last_assistant_message` is the *docs-recommended* source for final
text ("transcript is written asynchronously and may lag" — hooks reference), which means assistant-text
capture does **not** require widening the transcript allowlist. That materially lowers its cost.

### 3. Why it is this way — constraints any enrichment must respect

- **INV-2 / SL3-SEC-3** ("commands, file bodies, tool output never egress on observe events") holds
*by construction*: fields are unbound in `HookEvent`, `stripContent` is the client choke point,
`TestFinops_NoContentOnWire` is a load-bearing sentinel. `docs/data-and-privacy.md:15` promises
"Tool output: **never**" — enrichment changes a published promise, hence decision record
territory.
- **The `input` you see today is the gated /evaluate copy.** With `enforce: true`, the inline
  evaluation delivers the event (with content) and suppresses the duplicate observe copy
  (evaluate.go:167-172). An observe-only org gets NO command text at all. Any "input looks fine"
  conclusion from your sample only holds while enforce is on.
- **New transcript-bound string = decision record amendment**, not a commit (that decision rule; the allowlist "IS the
  struct"). Corollary: prefer hook-payload fields (last_assistant_message) over transcript binding.
- **Redact-before-attach must extend to outputs** (conformance C18 pins ordering for inputs). Outputs
  are where secrets actually surface — `env` dumps, `cat .env`, tokens in stderr. Local secret
  detection (`decision/`) must run over any new content class before it is attached.
- **64KB `capBody` cap** on signed bytes already exists and matches the provider's own 60KB default.
- **Event identity untouched**: enrichment adds fields to existing events; `activity_id`/`event_id`
  derivation must not change (byte-pinned, load-bearing for core dedupe).

### 4. Provider + industry precedent (external)

Claude Code's **own OTel export** ([monitoring docs](https://code.claude.com/docs/en/monitoring-usage))
is the strongest precedent — Anthropic already made these privacy calls:

| Content class | Provider's flag | Default |
|---|---|---|
| Prompt text | `OTEL_LOG_USER_PROMPTS` | off (redacted) |
| Assistant response text | `OTEL_LOG_ASSISTANT_RESPONSES` (separate flag, falls back to prompts flag) | off |
| Tool commands/params | `OTEL_LOG_TOOL_DETAILS` | off |
| Tool input/output content | `OTEL_LOG_TOOL_CONTENT` | off |
| Raw API bodies | `OTEL_LOG_RAW_API_BODIES` | off; 60KB truncation |
| **Thinking blocks** | **no flag — always redacted** | never exported |

Its `tool_result` OTel event carries `success`, `error_type`, `duration_ms` — exactly the trio the
Tool Health Matrix needs; treat that as the field-naming reference. OTel GenAI semconv agrees
directionally: prompt/completion content events are **Opt-In requirement level**, still
"Development" stability as of mid-2026 ([gen-ai events](https://github.com/open-telemetry/semantic-conventions-genai/blob/main/docs/gen-ai/gen-ai-events.md),
[status roundup](https://dev.to/azena-ai/opentelemetrys-genai-semantic-conventions-are-not-stable-yet-heres-what-actually-shipped-in-2026-3mke)).
Per-class flags + off-ish defaults + hard truncation + thinking-never is the industry answer.

### 5. Widget dependency chain (connects to plan 260813-2200)

| Empty widget | Root cause (scout 02) | Which enrichment unblocks it |
|---|---|---|
| Tool Health SUCCESS 0% | core `ExtractToolMetric` needs top-level `Status=="completed"` on ActivityCompleted; no producer sends one → `.failed` incremented every call | **P0 `status` field** (client-only fix; core already reads it) |
| Goal Alignment Trend | core AGE accumulates assistant content **only from `payload.Spans[]`** (`Stage=="completed" && SemanticType==llm_completion && ResponseBody!=nil`); dev sessions are span-less and never send text (INV-2) → `GoalAlignmentChecked` never true | **P2 assistant text** on the turn's `activity_output` **+ a core-side change** to read it from the llm_completion Activity, not only spans |
| Recent Drift Events | same accumulator, further gated on LlamaFirewall replay | same as above |

Client-side capture alone fixes Tool Health; alignment/drift additionally needs the openbox-core ask.

## Comparative Analysis — enrichment options

| Option | Value | Privacy delta | Effort | Verdict |
|---|---|---|---|---|
| A. `status`/`error_type` on ToolResult + wire `PostToolUseFailure` | Tool Health widget; failure visibility | none (enums) | S | **do first** |
| B. Task/subagent structural mapping (`subagent_type`, wire `SubagentStart`) | legible session tree | none | S | do with A |
| C. `tool_response` → `activity_output.output` (gated, redacted, capped) | "what did the tool return" in UI; Guardrails stage-1 gets real content | new content class — reverses a published "never" | M (decision record + conformance + testbed) | recommend, behind flag |
| D. `last_assistant_message` + `stop_reason` → turn `activity_output` | assistant text in UI; **unblocks AGE**/alignment | new content class (provider ships a separate flag for it) | M (decision record; no transcript change needed) | recommend, behind flag |
| E. Thinking capture (transcript) | "thoughts" in UI | exceeds provider's own export; widens transcript allowlist | L | **don't** |
| F. Full per-message trace (parse transcript content[]) | sub-turn fidelity, tool_use/result pairing | largest egress ever; storage cost; duplicates A–D | L | defer; A–D covers 90% of the visible gap |

## Implementation Recommendations (phased)

### P0 — structural, ship without decision record
- `hookevent.go`: bind nothing new for status — success is implied by which hook fired once
  `PostToolUseFailure` is wired; map PostToolUse → `status:"completed"`, PostToolUseFailure →
  `status:"failed"` + `error_type` enum. Emit as top-level payload `status` (core reads it,
  scout-02 errors.go:332) + `metadata`.
- Wire `PostToolUseFailure`, `SubagentStart` (structural metadata only), optionally
  `PermissionDenied`/`StopFailure` (governance-relevant enums).
- `mapper.go`: extract `subagent_type` from Task `tool_input` (identifier-class, like file_path).
- Caveat to verify in testbed: `Status` field is documented core-side for *Workflow* events;
  scout-02 flagged the reuse. Confirm no side effect on workflow status handling.
- Installer/plugin `hooks.json` + capabilities.go + MAPPING.md rows + conformance cases.

### P1 — tool output (decision record: new content class)
- Bind `tool_response` (capped at parse boundary), attach via existing unused plumbing:
  `Content.Output` → `activity_output.output` (payload.go `structuralActivityOutput` gains one
  content-gated key, mirroring `activity_input.command`).
- Run `decision` secret detection over it **before** attach (extend C18-style conformance case to
  outputs: assert redaction on outbound bytes).
- `stripContent` already nils `Content` — the choke point needs no change.
- Update `docs/data-and-privacy.md` "Tool output: never" row honestly.

### P2 — assistant text + stop_reason (decision record, same or sibling)
- Bind `last_assistant_message`/`stop_reason` in `HookEvent` **under the content gate** (pattern:
  existing `Prompt` field). Attach on `TurnCompleted` → `activity_output.message`. Transcript
  projection and its sentinel test stay untouched.
- Pair with openbox-core ask: AGE assistant-content accumulator reads llm_completion
  `activity_output.message` for span-less (developer) sessions.

### Config surface (OD decision, but recommendation)
Follow the provider: per-class `*bool` keys — `capture_tool_output`, `capture_assistant_text` —
**defaulting to follow `content_capture`** so one master switch still works. Remember the two
hard-won lessons: `*bool` (omitempty drops explicit false) and test the second `init` invocation.

### Common pitfalls (from this repo's own history)
- Don't bind new strings in the transcript projection to "save a hook field" — that's an
  allowlist widening with a load-bearing sentinel test.
- Don't let a new content field bypass `stripContent`/`capBody` by riding metadata — metadata is
  never content (payload.go comment at 411 exists because two fields once tried).
- Don't touch `activity_id`/event-id derivation while adding fields.

## Resources & References

- Repo: `adapters/claude-code/{mapper,hookevent,usage,enforcetarget}.go`, `client/{event,payload}.go`,
  `contracts/dev-event/MAPPING.md` (§2 turn pair, §3 field map, line 365), `docs/data-and-privacy.md`,
  `plans/260813-2200-dashboard-widget-telemetry-gaps/scout/scout-0{1,2}-*.md`,
  `plans/260811-1640-coding-agent-token-usage/reports/measure-260811-transcript-turn-surface.md`.
- [Claude Code hooks reference](https://code.claude.com/docs/en/hooks) — per-hook stdin fields.
- [Claude Code monitoring/OTel](https://code.claude.com/docs/en/monitoring-usage) — provider's own content flags, thinking always redacted.
- [OTel GenAI events semconv](https://github.com/open-telemetry/semantic-conventions-genai/blob/main/docs/gen-ai/gen-ai-events.md) · [OTel GenAI observability blog](https://opentelemetry.io/blog/2026/genai-observability/) · [2026 stability status](https://dev.to/azena-ai/opentelemetrys-genai-semantic-conventions-are-not-stable-yet-heres-what-actually-shipped-in-2026-3mke) — content capture is Opt-In, conventions still Development.

## Next steps

1. Decide the OD questions below (privacy posture — human call).
2. `/ak:plan` P0 (status + failure hooks + subagent mapping) — client-only, fixes Tool Health.
3. Draft decision record for P1/P2 content classes; file the openbox-core AGE ask (read assistant content from
   llm_completion activities) alongside the existing server-side-dedupe ask.
4. Codex parity check when P1/P2 land (Codex adapter: what's its output/assistant-text surface?).

## Unresolved Questions (OD — need your decision)

> **RESOLVED 2026-08-13 22:34 — see "Decision update" section below.** Owner set the posture:
> full capture. Q1–Q3 decided; Q4–Q6 remain open as engineering follow-ups.

1. **Defaults**: tool output + assistant text ON by default (like content_capture's 2026-07-15 flip)
   or OFF (provider's own default)? Provider precedent says off; your product's capture-first posture says on.
2. **One gate or per-class flags**: single `content_capture` governs all, or
   `capture_tool_output`/`capture_assistant_text` sub-flags (recommended above)?
3. **Thinking**: accept "never captured" as product stance (recommended), or fight for it against
provider precedent in a dedicated
decision record?
4. **Core asks priority**: AGE activity_output read path (needed for alignment widgets) — file now or
   after P2 ships client-side?
5. `Status`-field reuse semantics on activities (workflow-documented field) — confirm with core team
   or via testbed before P0 merges.
6. Retention/PII handling server-side for new content classes (64KB/event × every tool call is real
   storage) — backend question, out of shift-left's control.

---

## Decision update (2026-08-13 22:34) — posture: FULL CAPTURE

Owner decision (vu@krnl.xyz): OpenBox is runtime governance an **organization** deploys over its own
developers — company machine, company code. Collect **full data, or as much as the surfaces allow**,
so OpenBox has complete input for evaluation (policy /evaluate, Guardrails stage 0/1, AGE goal
alignment, finops). This resolves the OD questions:

1. **Defaults: ON.** All content classes captured by default, consistent with the 2026-07-15
content_capture flip and that decision finops flip ("a default-off headline feature stays off").
Org opt-out remains `content_capture:false` / `OPENBOX_CONTENT_CAPTURE=0`.
2. **Gate: single master gate.** Reuse the existing `content_capture` key for every content class —
   no new config keys (KISS; per-class narrowing flags only if a customer asks).
3. **Thinking: IN SCOPE.** Org-owned trust model makes it the org's call; the transcript on the
developer machine holds thinking in plaintext already (same trust boundary as the signing key).
Requires that decision allowlist amendment — staged last because the sentinel test is
load-bearing.

### Full-capture target state (all fields content_capture-gated, secret-redacted BEFORE attach, 64KB-capped)

| Event | Add | Source |
|---|---|---|
| `ActivityStarted` — every tool, **observe path too** | `activity_input` command / MCP args / file body; Task `prompt`+`description`+`subagent_type` | `PreToolUse.tool_input` (already parsed; today attached only on the gated /evaluate copy) |
| `ActivityCompleted` — every tool | `status: completed\|failed`, `error_type`, `activity_output.output` (tool_response text), structured `tool_output` passthrough | `PostToolUse` / `PostToolUseFailure` |
| `TurnCompleted` (`llm_completion`) | `activity_output.message` (final assistant text), `stop_reason`; `activity_output.thinking[]` + intermediate assistant text blocks for the turn window | `Stop/SubagentStop.last_assistant_message` (final text); transcript window via existing TurnCursor (thinking + intermediates — decision record amendment) |
| `PromptSubmitted` | already full | — |
| New wire events | failed ToolResult (PostToolUseFailure), SubagentStart (spawn marker), PermissionDenied (`denial_reason`, `classifier_verdict`), StopFailure (api error taxonomy: rate_limit/billing/…) | unwired hooks |

Consequences owned by this posture:
- **SL3-SEC-3 is retired by decision record** ("commands/file bodies never egress on observe events") — replaced
  by: *content egresses on every path under the org gate, always redacted-first, always capped*.
  `docs/data-and-privacy.md` "never" rows rewritten honestly. INV-2 transforms from "metadata-only"
  to "gate + redact + cap at the choke points" (stripContent/capBody already are those choke points).
- **Redaction becomes the load-bearing control**: extend `decision` secret detection over
  tool_response and assistant/thinking text before attach; new conformance cases pin
  redact-before-send on outbound bytes for each class (C18 pattern).
- **Volume**: up to 64KB × every tool call × every turn. Realtime flush already exists; server
  truncates at 64KB. Storage/retention is a backend concern to file alongside the dedupe ask.
- **Codex parity gap disclosed**, not hidden: Codex adapter's equivalent surfaces need their own
  mapping pass (its hook cannot be mandated; per-session usage only).

### What even full capture cannot get (be honest in docs)

1. **System prompts / full API request-response bodies** — not in hooks, not in the transcript.
   Only Claude Code's own OTel channel (`OTEL_LOG_RAW_API_BODIES`) exports them; if the org wants
   this, run CC's OTel export into an org collector as a complementary channel (different pipeline,
   out of the dev-event contract).
2. **Per-model-call granularity** — hooks fire per turn; token numbers stay window sums (measured
   ~52 model calls per Stop window).
3. **Provider-side redactions** — anything Claude Code never writes to disk.

### Revised sequencing (same phases, all now approved direction)

P0 structural (status/failures/subagent — fixes Tool Health) → P1 tool output → P2 assistant text +
stop_reason (+ core AGE ask: read assistant content from llm_completion `activity_output` for
span-less sessions) → P3 thinking + observe-path input attach + that decision allowlist amendment.
One decision record can cover the whole content-posture change (P1–P3) with the retirement of
SL3-SEC-3 argued against its original rationale, that decision-style; P0 needs no decision record.

Remaining open (engineering, not posture): Q4 core AGE ask timing, Q5 `Status`-field reuse semantics
(verify in testbed), Q6 server-side retention/storage for 64KB-class events.
