# Epic E7 — Unify shift-left telemetry onto the base SDK wire model: story backlog

**Author:** planning (2026-07-14) — sliced after ADR-0004 (accepted, brian) chose §4 option **B-core**.
**Epic:** E7 — re-express shift-left's developer-runtime telemetry onto the base SDK's (`openbox-sdk-python` / `openbox_core`) `Workflow`/`Activity`/`Signal` + flat `SpanData` wire model, extending the base SDK + Core with first-class `shell`/`mcp`/`tool` hook types so dev-runtime tool calls are a *shared* concept. Outcome: shift-left conforms to the base wire contract + conformance kit, the SL-13 EXT-core accept-list patch is retired, and the shared dashboard timeline pairs dev tool calls correctly.
**Design:** `.fab7/sdlc/design/adr/ADR-0004-unify-dev-events-onto-base-wire-model.md` + `.fab7/sdlc/design/sdk-python-remap-migration.md` (event→wire mapping, flat-SpanData construction, phased plan) + `.fab7/sdlc/design/sdk-python-remap.md` (the gap analysis this epic answers).
**Status:** UNBLOCKED to spike. ADR-0004 accepted (both sub-decisions ruled). E7-S0 (spike) runs first; the upstream base-SDK/Core hook-type additions (E7-S1/S2) are the critical path and gate the shift-left work (E7-S3+).

## Decisions baked in (ADR-0004, brian 2026-07-14)
- **Option B-core (true unification):** extend the base SDK + Core classifier with first-class `shell`/`mcp`/`tool` hook types; shift-left mirrors. (Rejected B-shiftleft — shell/mcp second-class.)
- **CommitCreated/Deploy → SignalReceived** (`signal_name`, lineage keys in metadata). No residual EXT-core accept-list — SL-13 patch fully retired.
- **Developer session = workflow** (`SessionStarted`→`WorkflowStarted`). Reverses SL-1's deliberate separation; accepted because enforcement stays on `ToolCall`=`Activity` (OPA still runs on tool calls; only lifecycle `Workflow*` events hit the OPA-bypass, which is fine).

## The one architectural truth
The base SDK blesses only `Workflow*/Activity*/Signal/Handoff` wire types; a tool call is `ActivityStarted` + `hook_trigger=true` + non-empty flat `SpanData` (`events.py:365-401`; `assert_hook_wire_shape` `conformance/fake_core.py:132-170`). Core computes `semantic_type` server-side from span source fields (`session.go:204`). Unification = shift-left emits that shape (no OTel dependency — build the flat `SpanData` dict directly), and the base SDK/Core gain `shell`/`mcp` first-class so those tool kinds aren't lossy.

## Cross-repo note
**E7-S1 is NOT a sibling change** (re-scoped 2026-07-14): `openbox-sdk-python` is off-limits — E7-S1 mirrors the contract into shift-left's own `client/` (ADR-0004 Amendment). Only **E7-S2 (`openbox-core`)** remains a sibling-repo change — committed **locally**, no push/PR until brian asks (`sibling-repo-changes-stay-local`). Note the session key also can't push `openbox-sdk-python` (`sibling-repo-push-access`).

---

## Stories

### E7-S0 — Spike: verify the unified wire shape on live Core  *(gate 0 — blocks the epic)*
- **Goal:** with a live local stack (RUNBOOK / `local-openbox-run`), verify: (a) a dev `ActivityStarted`+hook span classifies to the intended `semantic_type` (file today; confirm the shell/mcp gap E7-S1/S2 must close); (b) the `Workflow*` OPA-bypass (`opa.go:205`) does NOT disable enforcement (enforcement is on `ToolCall`=`Activity`); (c) the flat-`SpanData` attribute space holds shell/mcp specifics; (d) `SignalReceived` carries commit/deploy lineage keys usably (FR-5/6/7); (e) content-capture (INV-2) still gated on the new `ActivityStarted` shape (Activity is guardrails-eligible). **Artifact:** `.fab7/sdlc/discovery/spikes/S8-unified-wire-shape.md`. **Deps:** ADR-0004. **Gates:** — (spike). **Blocks:** all of E7.

### E7-S1 — shift-left MIRROR of the base hook-span contract: `shell`/`mcp`/`tool`  *(critical path)*
- **RE-SCOPED 2026-07-14 (brian):** do **NOT** edit `openbox-sdk-python`. Mirror the contract into shift-left instead (ADR-0004 Amendment 2026-07-14). The upstream base-SDK edit was built then reverted (sdk-python restored untouched).
- **Goal:** a shift-left-owned Go mirror of the base SDK's hook-span wire contract — `HookType` (`file_operation`/`shell`/`mcp`/`tool`) + `CommonRootFields` + `FamilyRootFields` + `DefaultKind` + `AssertHookWireShape` (the Go port of `conformance/fake_core.assert_hook_wire_shape`). Byte-faithful to the READ-ONLY reference `openbox_core/contracts/otel_spans.py` + `fake_core.py`, with the dev-runtime `shell`/`mcp`/`tool` additions. This is the contract+conformance half; the flat-`SpanData` builder that produces conforming payloads is E7-S3.
- **Write scope:** `client/hookspan.go`, `client/hookspan_test.go`. **Deps:** E7-S0. **Gates:** G3 (contract shape already brian-signed; Go mirror independently reviewed). **G_SEC:** n/a here — no egress/content sink in a contract-definition file; the shell_command content-gating/truncation decision lands in the E7-S4 emitter. **Repo:** shift-left (not a sibling).

### E7-S2 — Core: classify `shell`/`mcp` spans first-class + retire EXT-core  *(UPSTREAM, critical path)*
- **Goal:** extend `ComputeSemanticTypeFromSpan` (`openbox-core internal/content/session.go:204`) to classify a `shell` span (e.g. `shell_command`) and an `mcp` span (`mcp_tool_call`) from their root/attribute fields; retire the SL-13 EXT-core dev-type accept-list patch (`contracts/dev-event/ext-core/`) now that every dev event maps to a stock accept-listed wire type. Live-verify the classifier.
- **Write scope:** `openbox-core internal/content/session.go` (+ tests); shift-left `contracts/dev-event/ext-core/` (remove/retire). **Deps:** E7-S0, E7-S1. **Gates:** G3, **G_SEC**. **Invariants:** INV-8 (additive classifier change, not a wire break). **Repo:** sibling — local commit only.

### E7-S3 — shift-left: flat `SpanData` builder (Go, no OTel)
- **Goal:** a Go builder that emits the flat Core `SpanData` (16-hex `span_id`, 32-hex `trace_id`, `parent_span_id`, int-ns `start_time`, started-stage `end_time:null`/`duration_ns:null`, common + family root fields, `attributes`), plus per-session trace-id + parent linkage. Replaces the hand-built `client/event.go Span`. Unit + a wire-shape conformance test mirroring `assert_hook_wire_shape`.
- **Write scope:** `client/event.go`, `client/payload.go`, new span builder + tests. **Deps:** E7-S1. **Gates:** G3. **Invariants:** INV-2 (content only in gated span bodies).

### E7-S4 — shift-left: `ToolCall`/`ToolResult` → `ActivityStarted`+hook / `ActivityCompleted`
- **Goal:** map the tool events (file/mcp/shell) onto the base hook shape using the E7-S1 hook types (§2 rows 3–6 of the migration doc); `ToolResult` pairs with its `ToolCall` (`activity_id`, completed-stage span). The enforce path (`enforce.go`/`sidecar`) stays green (observe/egress-shape change only). Live E2E + enforce regression.
- **Write scope:** `client/payload.go`, `adapters/claude-code/mapper.go`, tests. **Deps:** E7-S3. **Gates:** G3. **Invariants:** INV-3/INV-3b (enforce untouched), INV-2.

### E7-S5 — shift-left: lifecycle → `Workflow*` / `SignalReceived`
- **Goal:** `SessionStarted`→`WorkflowStarted`, `SessionEnded`→`WorkflowCompleted` (session=workflow: `session_id`→`run_id`, stable id→`workflow_id`, `workflow_type`); `PromptSubmitted`/`CommitCreated`/`Deploy`→`SignalReceived(signal_name=…)` with lineage keys in metadata (§2 rows 1,2,7,8,9). Verify commit/deploy lineage (FR-5/6/7) survives the Signal mapping.
- **Write scope:** `client/payload.go`, `adapters/claude-code/mapper.go`, `adapters/common/git` (lineage metadata), tests. **Deps:** E7-S3, E7-S2. **Gates:** G3. **Invariants:** INV-8.

### E7-S6 — shift-left: contract + conformance + dashboard E2E
- **Goal:** update `contracts/dev-event/MAPPING.md` + `contracts/dev-event/schema/dev-event.schema.json` to the unified shape; adopt the base conformance parity matrix (map E6-S7 C1–C9 ↔ base `tests/conformance/test_required_cases.py`); full live E2E in the shared dashboard confirming dev tool calls now pair as `ActivityStarted/Completed` (fixes `dashboard-devruntime-display-gaps`).
- **Write scope:** `contracts/dev-event/`, `adapters/claude-code/*conformance*`, docs. **Deps:** E7-S4, E7-S5. **Gates:** G3. **Invariants:** all INV re-verified end-to-end.

---

## Sequencing (critical path)
```
E7-S0 (spike) ✅ gate 0
   ─► E7-S1 (base SDK: shell/mcp/tool hook types — UPSTREAM)
   ─► E7-S2 (Core: classifier + retire EXT-core — UPSTREAM)
   ─► E7-S3 (Go flat SpanData builder)
   ─► E7-S4 (ToolCall/Result → Activity+hook)
   ─► E7-S5 (lifecycle → Workflow*/Signal)
   ─► E7-S6 (contract + conformance + dashboard E2E)
```
E7-S1/S2 (cross-repo) gate E7-S3+ (shift-left mirrors the extended contract).

## Open items resolved / carried
- **RESOLVED (ADR-0004):** unify scope = B-core; commit/deploy = Signal; session = workflow.
- **CARRIED to E7-S0 for evidence (not decisions):** OPA-bypass harmlessness, semantic-type derivation for shell/mcp, flat-span attribute capacity, content gate on the Activity shape.
- **Independent of E7:** E6-S8 ([EXT-opa-bundle] signed bundle sync, per `sidecar-policy-sync-design`) — orthogonal; does not block E7.
