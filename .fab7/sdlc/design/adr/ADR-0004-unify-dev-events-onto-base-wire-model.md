# ADR-0004: Unify shift-left telemetry onto the base SDK's Activity/hook wire model

## Status
accepted — G_ADR ratified by brian 2026-07-14 ("§4 do (b) to unify sdk and shift-left"), including both sub-decisions:
- **Sub-decision #1 = B-core (true unification):** extend the base SDK (`openbox-sdk-python`) and Core's semantic classifier (`openbox-core`) to first-class dev-runtime tool calls (`shell`/`mcp`/`tool` hook types), then shift-left mirrors the extended contract. This makes the upstream base-SDK/Core change an **upstream-first, critical-path** dependency (not a trailing step).
- **Sub-decision #2 = SignalReceived:** `CommitCreated`→`SignalReceived(signal_name="commit_created")`, `Deploy`→`SignalReceived(signal_name="deploy")`, lineage keys (`commit_sha`/`deploy_id`/`deploy_did`) in payload metadata. **No residual EXT-core accept-list** — the SL-13 patch is fully retired.

The E7-S0 spike still runs first to **verify** the technical assumptions (Core OPA-bypass, semantic-type derivation, flat-span attribute space) with evidence before the upstream PRs are cut — it validates execution, it no longer gates the decision.

### Amendment 2026-07-14 (brian) — do NOT edit `openbox-sdk-python`; mirror into shift-left instead

Sub-decision #1's phrase "extend the base SDK" is **refined**: shift-left must **not modify the `openbox-sdk-python` repo**. Instead it carries a **hand-maintained Go mirror** of the base SDK's hook-span contract (HookType + family root fields + `assert_hook_wire_shape`), treating `openbox-sdk-python` as a **read-only reference**. This is closer to option (C)/B-shiftleft for the *SDK half*. Practical trigger: that repo is not ours to push to (the session key lacks collaborator access — see memory `sibling-repo-push-access`), and mirroring avoids a cross-team upstream PR.

**What is unchanged:** the **openbox-core classifier edit (E7-S2) still proceeds** — "only sdk-python is off-limits" (brian). So `shell`/`mcp` still become first-class server-side via Core's `ComputeSemanticTypeFromSpan`; the difference is purely *where the SDK-side contract lives* (shift-left `client/hookspan.go`, not `openbox_core`).

**Trade-offs accepted** (the ones this ADR originally weighed for B-core): the base SDK's own `assert_hook_wire_shape` will not know the dev-runtime families — shift-left validates against its **own** mirrored copy (`client.AssertHookWireShape`), which it keeps byte-faithful to the reference by discipline + a drift-guard test. Consequence: E7-S1 is re-scoped from an upstream base-SDK edit to a shift-left-owned mirror; E7-S2 is unaffected.

<!-- G_ADR gate. Decision owner: brian. Supersedes the SL-1 posture (keep a
separate developer-runtime event vocabulary + patch core's accept-list) for the
telemetry wire shape. Triggered by the reference SDK's refactor into the base
package openbox_core (see .fab7/sdlc/design/sdk-python-remap.md §4). -->

## Context

The reference SDK refactored into a **base SDK** (`openbox-sdk-python` → `openbox_core` v1.0.0) plus thin framework adapters; shift-left is conceptually the *developer-runtime* framework adapter (in Go). Full analysis: `.fab7/sdlc/design/sdk-python-remap.md`.

The base SDK's blessed wire `EventType` set is **only** `WorkflowStarted / WorkflowCompleted / WorkflowFailed / SignalReceived / ActivityStarted / ActivityCompleted / Handoff` (`openbox-sdk-python/openbox_core/contracts/events.py:50-59`), and a **tool/operation preflight serializes as `ActivityStarted` + `hook_trigger=true` + non-empty flat `SpanData` + `span_count`** (`events.py:365-401`; guide `instructions.md:273-285`; enforced by `conformance/fake_core.py:132-170 assert_hook_wire_shape`).

Shift-left today emits a **parallel developer-runtime vocabulary** — `SessionStarted / PromptSubmitted / ToolCall / ToolResult / SessionEnded / CommitCreated / Deploy`, `source="developer-runtime"` (`client/event.go:31-41`, `contracts/dev-event/schema/dev-event.schema.json:29-37`, `MAPPING.md §1-2`) — which required patching Core's accept-list (SL-13 `contracts/dev-event/ext-core/`; `MAPPING.md §5`). Consequences of the status quo: shift-left telemetry **fails** the base SDK's `assert_hook_wire_shape`; shift-left carries a Core patch the canonical adapters do not; the shared dashboard shows dev events as `Unknown` with empty Input/Output because its timeline pairs only `ActivityStarted/Completed` (memory `dashboard-devruntime-display-gaps`; `MAPPING.md §5` line 100).

SL-1 deliberately chose the separate vocabulary and **explicitly warned against** mapping developer sessions onto `WorkflowStarted/Completed` (`MAPPING.md §2` note): those strings trigger Core's `Workflow*` OPA-bypass (`openbox-core internal/.../opa.go:205`) and the guardrails-ineligible path (`governance_workflow.go:429`), and "conflate developer sessions with Temporal workflows — a semantic break." **This ADR deliberately reverses that stance:** unification *means* developer sessions become first-class workflows in the one shared model, exactly as every other framework SDK's runs are (guide `instructions.md:180-201` maps a framework run → `workflow_id/run_id/workflow_type`).

## Decision

**Adopt option (B): re-express shift-left telemetry onto the base SDK's Activity/hook + flat `SpanData` wire model**, so shift-left conforms to `openbox_core`'s wire contract, can adopt the base conformance kit, and lets the SL-13 EXT-core dev-type accept-list be retired.

Baseline mapping (detailed, source-cited, in `sdk-python-remap-migration.md`):

| Shift-left dev event | Base wire event | Notes |
|---|---|---|
| `SessionStarted` | `WorkflowStarted` | developer session = workflow; `session_id`→`run_id`, a stable workspace/session id→`workflow_id`, `workflow_type="developer-session"` (or provider). |
| `SessionEnded` | `WorkflowCompleted` | terminal. |
| `ToolCall` (file) | `ActivityStarted`+`hook_trigger`+span `hook_type=file_operation` | root `file_path`+name `file.read\|write\|open\|delete` → Core computes the file semantic type (`session.go:256-268`). Clean. |
| `ToolCall` (mcp) | `ActivityStarted`+`hook_trigger`+span, `attributes["mcp.method"]="callTool"` | Core computes `mcp_tool_call` from the attribute (`session.go:288-296`). |
| `ToolCall` (shell) | `ActivityStarted`+`hook_trigger`+span | no shell hook type; falls to `internal` (as today, `MAPPING.md §2` line 43). See sub-decision #1. |
| `ToolResult` | `ActivityCompleted` | completed-stage span; pairs with the ToolCall → fixes the dashboard display gap. |
| `PromptSubmitted` | `SignalReceived` (`signal_name="prompt_submitted"`) | governance-relevant, not a tool op. |
| `CommitCreated` / `Deploy` | see sub-decision #2 | dev-runtime-only concepts with no agent-runtime analog. |

The enforce path is **unaffected in principle**: the PreToolUse gate already obtains a local decision on a `ToolCall`; under (B) that same call is *also* the `ActivityStarted`+hook event on the observe/egress side. The sidecar `DecisionRequest` stays local and shift-left-owned; only the **egressed** telemetry shape changes.

## Two sub-decisions — RESOLVED (brian 2026-07-14)

**Sub-decision #1 = B-core (true unification).** The base `HookType` families are `http_request / db_query / file_operation / function_call / llm_call` (`contracts/otel_spans.py:36-43`) — **no `shell` or `mcp`**. We extend the base SDK (and Core's semantic classifier) to first-class dev-runtime tool calls (`shell`/`mcp`/`tool` hook types), then shift-left mirrors the extended contract. The guide sanctions this: "add a base-SDK change when the framework exposes a shared gap other SDKs will also need" (`instructions.md:369-371`) — coding-agent tool calls are exactly such a shared gap (Codex/Cursor will hit it too). **Critical-path consequence:** the upstream `openbox-sdk-python` (+ `openbox-core`) hook-type additions must land (locally, per the sibling-repo-changes-stay-local convention) **before** shift-left mirrors them. Rejected: B-shiftleft (map onto the existing shape with shell/mcp second-class) — brian chose the first-class shared model.

**Sub-decision #2 = SignalReceived.** `CommitCreated`→`SignalReceived(signal_name="commit_created")`, `Deploy`→`SignalReceived(signal_name="deploy")`; lineage keys (`commit_sha`/`deploy_id`/`deploy_did`, FR-5/6/7) ride in payload metadata. Stays within the base vocabulary, so **no residual EXT-core accept-list** — the SL-13 patch is fully retired. Rejected: keeping them as dev types (would leave a 2-type Core patch).

## Consequences

**Gains**
- shift-left telemetry conforms to the base wire contract; it can run the base conformance parity behaviors and (under B-core) `assert_hook_wire_shape`.
- The SL-13 EXT-core dev-type accept-list can be **retired** (except possibly a 2-type residual for commit/deploy under sub-decision #2b).
- `ToolCall`→`ActivityStarted` / `ToolResult`→`ActivityCompleted` **pair correctly in the shared dashboard**, fixing the `Unknown`/empty display gap (memory `dashboard-devruntime-display-gaps`).
- Tool calls become **guardrails-eligible** at Core (`ActivityStarted`, not a dev type) (`MAPPING.md §5` line 95) — useful for content-capture redaction (E6-S4).

**Costs / risks**
- **Reverses SL-1's deliberate separation.** Developer sessions become `Workflow*` events → they hit Core's `Workflow*` **OPA-bypass** (`opa.go:205`). That is acceptable for *lifecycle* events (you don't police "a session started") **only because enforcement targets `ToolCall`s, which are `Activity` events and still get OPA**. This must be verified, not assumed (spike).
- shift-left must emit the flat `SpanData` shape (16-hex `span_id`, 32-hex `trace_id`, `parent_span_id`, int-ns `start_time`, `end_time:null`/`duration_ns:null` on started). shift-left has **no OTel dependency** and does not need one — it can construct the flat dict directly — but it needs a span/trace-id generator and correct parent linkage.
- (B-core) is a cross-repo, cross-team effort with upstream PR latency.
- `MAPPING.md`, the dev-event JSON schema, `client/event.go`/`payload.go`, and the conformance suite all change. This is an epic, not a story.

**Invariant impacts**
- **INV-2 (content):** unchanged in principle — content stays gated + carried only in gated span `request_body`/`response_body`; the wire reshape does not loosen the content gate. Re-verify under the new span shape.
- **INV-3 / INV-3b (observe-never-blocks / bounded enforce):** unchanged — the enforce decision stays local (sidecar); only the async egress shape changes.
- **INV-8 (additive to core):** (B-shiftleft) is additive; (B-core) intentionally *adds* to Core's classifier (new hook types) — a sanctioned, coordinated change, but no longer "no core change." Retiring the EXT-core accept-list is a net simplification.

## Alternatives considered
- **(A) Keep the dev vocabulary + EXT-core (status quo).** Lowest churn, preserves dev-native richness, but never conforms and carries the patch forever. Rejected by this decision (brian: unify).
- **(C) Hybrid envelope** — base `ActivityStarted`+hook envelope carrying dev specifics in span attributes/metadata. This is effectively **(B-shiftleft)** done carefully; folded into sub-decision #1 rather than kept as a separate option.

## Follow-ups
- Resolve sub-decisions #1 and #2 (brian) → move this ADR proposed→accepted (G_ADR).
- Spike: confirm (a) Core's `assert`-equivalent acceptance of dev `ActivityStarted`/hook spans + correct `semantic_type` for file/mcp/shell; (b) `Workflow*` OPA-bypass does not disable enforcement (enforcement is on `Activity` tool calls); (c) the flat `SpanData` attribute space holds mcp/shell/commit/deploy specifics.
- Then slice the migration epic (see `sdk-python-remap-migration.md` §"Phased plan").
