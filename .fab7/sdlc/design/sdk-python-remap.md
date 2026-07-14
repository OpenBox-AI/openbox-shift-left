# Shift-left ↔ `openbox-sdk-python` (base SDK) re-mapping & gap analysis

**Author:** design (2026-07-14). **Trigger:** the reference SDK moved from `openbox-temporal-sdk-python` (the Temporal-coupled agent-runtime SDK shift-left originally ported from) to **`openbox-sdk-python`** — the standalone base SDK `openbox_core` (v1.0.0). Shift-left's whole philosophy is "mirror the SDK" (CLAUDE.md: *reuse, don't rebuild*), so this doc re-grounds every shift-left parity point against the new base and flags drift.

**Method:** three read-only cross-repo surveys (per the standing "use Explore for cross-repo" directive): a deep-dive of the new `openbox_core`, an inventory of every shift-left SDK-mirroring surface, and an old→new SDK diff. All claims are file:line-cited to one of:
- NEW base SDK: `/run/media/brian/DATA/works/openboxai/openbox-sdk-python` (`openbox_core/`)
- OLD reference: `/run/media/brian/DATA/works/openboxai/openbox-temporal-sdk-python` (`openbox/`)
- shift-left: this repo.

---

## 0. The one architectural shift (read this first)

The SDK **refactored into a base + thin framework adapters**. `openbox_core` now owns contracts, config, identity/signing, the evaluate client, the strict gate, context correlation, generic instrumentation, approval parsing, and a **reusable conformance kit**. Framework SDKs (Temporal, LangGraph, LangChain, DeepAgent, CrewAI) are *thin adapters* that implement one `FrameworkAdapter` protocol and bind framework lifecycle into `OpenBoxRuntime` (`.github/instructions/openbox-sdk-python.instructions.md:7-17`; README:1-30).

**Shift-left is conceptually one more framework adapter — the *developer-runtime* adapter** (Claude Code / Codex / Cursor). But it is written in **Go**, so it cannot `import openbox_core`; it must re-implement the base SDK's contracts/client/signing/gate in Go (exactly what it did against the old Temporal internals). So the guide's "Do Not Reimplement" list (`instructions.md:254-268` — signing, EvaluationClient, fail-open/closed policy, EvaluationResult/ApprovalResult parsing, evaluate payload assembly, redaction, conformance fake-Core) is *precisely the set shift-left had to reimplement in Go*. The question this doc answers is therefore **not** "can shift-left import the base SDK" (it can't) but **"does shift-left's Go re-implementation still match the new base SDK's process, request/response, and functional model?"**

**Headline verdict:** the load-bearing *wire contracts* (AIP signing, verdict enum values, evaluate request/response, backend `agent/create`) are **stable** — shift-left keeps working. The drift is in (1) **idioms** shift-left ports by name (fail-policy shape, verdict-cascade home, redaction location), which are functionally correct but now cite dead symbols; (2) a genuine **structural divergence** (shift-left's dev-event vocabulary vs the base SDK's Activity/hook wire model); and (3) **new capabilities** shift-left lacks (outbound key-redaction, the canonical conformance kit, the `fallback_used` marker on the observe path). Only the structural divergence is an OD-class decision; the rest is re-grounding + small hardening.

The new mental model (map each shift-left stage to it):
```
framework lifecycle → ActivityContext → EventEnvelope factories → OpenBoxRuntime
  → GovernanceGate → EvaluationClient → OpenBox Core → EvaluationResult → FrameworkAdapter
hook path: HTTP/DB/file/function wrapper → HookRuntime → GovernanceGate.preflight/completed → FrameworkAdapter
```
(`instructions.md:19-42`)

---

## 1. Process mapping — shift-left stage → `openbox_core` concept

| Shift-left (Go) | New `openbox_core` | Fidelity |
|---|---|---|
| `openbox dev init` → backend `POST /agent/create` (`cli/internal/backend/client.go:132-151`), secret store, `DevConfig` (`adapters/claude-code/creds.go`) | Config/identity composition root: `OpenBoxConfig.resolve` + `AgentIdentity.from_private_key` + `OpenBoxRuntime(config, adapter=…)` (`runtime.py:40-69`, `identity.py:131-152`) | ✅ **equivalent**. `create_openbox_worker()` is **gone**; the runtime factory + `FrameworkAdapter` replace it (`instructions.md:156-165`). `agent/create` is a *backend* contract, untouched by the SDK refactor. Re-ground the doc comment `adapters/claude-code/doc.go:8-10`. |
| adapter SPI `register/emit/apply/capabilities` (`adapters/claude-code/doc.go:12-21`, `provider/provider.go`) | `FrameworkAdapter` protocol (`adapters/base.py:31-68`) + runtime lifecycle | ✅ **conceptual 1:1** (see §3). `register`↔config/runtime factory; `emit`↔`runtime.evaluate_lifecycle`; `apply`↔the four adapter callbacks; `capabilities`↔(no base analog — shift-left-owned). |
| `Adapter.Observe` → spool → `Flush` (`adapters/claude-code/adapter.go:47-85`) | `runtime.evaluate_lifecycle(event)` / `aevaluate_lifecycle` (`runtime.py:91-109`) | ✅ same role. Shift-left spools + flushes async (INV-3 observe-never-blocks); the base SDK evaluates inline. Different transport discipline, same purpose. |
| PreToolUse enforce gate (`adapters/claude-code/enforce.go`, `sidecar/`) | `HookRuntime.preflight` (started stage) → `GovernanceGate.preflight` → adapter (`hooks/preflight.py:57-152`, `gate.py:104-116`) | ✅ **direct analog**. "preflight blocks the real op before it runs" = shift-left's pre-execution `permissionDecision`. See §4. |
| PostToolUse observe (`hookrun.go`) | `completed` stage — "never undo, only mark future work" (`hooks/preflight.py:216-293`, `adapters/base.py:58-68`) | ✅ same "post-op can't undo" invariant. |

---

## 2. Request/response mapping — wire contracts

### 2a. AIP signing — ✅ STABLE (byte-compatible)
Canonical string `UPPER(METHOD)\nPATH\nTIMESTAMP\nNONCE\nBODY_SHA256_HEX`, five headers `X-OpenBox-Agent-{DID,Timestamp,Nonce,Signature}` + `X-OpenBox-Body-SHA256`, Ed25519 raw-32-byte-seed, **std** (padded) base64 signature, compact JSON body serialized once and sent via `content=` (never `json=`), signing timestamp `datetime.now(UTC).isoformat()` = **`+00:00` (never `Z`)**.
- NEW: `identity.py:66-71,147-159,182-231`; `serialization.py:75-85`. The base SDK asserts byte-compat with the Temporal SDK via a **golden fixture** (`tests/signing/test_golden_signing.py:44-82`).
- shift-left: `client/signing.go:14-118` — matches exactly (already cites `request_signing.py` / core `agent.go:93`). **No change needed.**
- ⚠️ **Minor:** the `X-OpenBox-SDK-Version` *value* format is now `openbox-{engine}-{language}-v{version}` (`sdk_version.py:55-79`, e.g. `openbox-base-python-v1.0.0`); shift-left sends `openbox-shift-left/0.1.0` (`client/signing.go:43`). The header is **not** in the canonical string, so signing is unaffected — but if Core ever parses the format, align to e.g. `openbox-shiftleft-go-v0.1.0`. Low risk.

### 2b. Verdict enum — ✅ STABLE (wire), internal extension OK
NEW `Verdict` (`results.py:25-73`) = OLD values, unchanged: `allow / constrain / require_approval / block / halt` (lowercase), `from_string` unknown→ALLOW, `should_stop()`=BLOCK|HALT, priority HALT=5…ALLOW=1.
- shift-left `client/verdict.go:16-45` parses lowercase wire → UPPERCASE internal constants (`"ALLOW"`…) and adds `VerdictUnknown=""` as a fail-open marker. Since shift-left **receives** verdicts (never sends them on the wire), the internal case is harmless; `WouldBlock()` ≡ `should_stop()`. ✅ **No wire change needed.** `VerdictUnknown` is a documented shift-left extension (the base SDK instead uses `Verdict.ALLOW` + `fallback_used=True`; see §2d).

### 2c. Evaluate request/response — ✅ STABLE
Endpoint `POST /api/v1/governance/evaluate` (NEW `client.py:46`; shift-left `client/client.go:17`). Response parsed by `EvaluationResult.from_dict` (`results.py:150-183`) reads: `verdict`/`action`, `reason`, `policy_id`, `risk_score`, `metadata`, `governance_event_id`, `guardrails_result`|`guardrails`, `approval_id`, `approval_expiration_time`, `trust_tier`, `alignment_score`, `behavioral_violations`, `constraints`, `fallback_used`, `diagnostics`. shift-left's `verdictResponse`/`parseEvaluation` (`client/verdict.go:138-212`) parses the same set **except** `fallback_used`, `diagnostics`, `metadata`. ✅ core-compatible.
- ⚠️ **Small gaps to consider:** parse `fallback_used` on the observe path so shift-left can tell a real ALLOW from an unreachable-Core ALLOW in advisory telemetry (mirrors `Decision.FailOpen` at the enforce layer). `metadata`/`diagnostics` optional.

### 2d. Fail-open / fail-closed — ⚠️ IDIOM DRIFT (functionally correct)
| | OLD (shift-left ported this) | NEW `openbox_core` |
|---|---|---|
| helper | `_handle_api_error` (`client.py:204-208`) | `_network_failure` (`client.py:214-219`) |
| fail-open | returns `None` (→ proceed) | returns `EvaluationResult.fallback_allow(reason)` = **ALLOW + `fallback_used=True`** (`results.py:185-193`) |
| fail-closed | synthesizes `Verdict.HALT` response | **raises `GovernanceAPIError`**; the *adapter* maps it to native halt/block |
| scope | — | **network errors only**; contract violations raise `ContractError` before send and are NEVER fail-open'd (`gate.py:1-8`) |

- shift-left `applyFailurePolicy` (`enforce.go:154-164`) still synthesizes `VerdictHalt` on `FailOpen && FailClosed` (OLD idiom). **Outcome is identical** (fail-closed→deny; fail-open→proceed), so this is not a bug — but it cites a dead symbol. **`Decision.FailOpen` ≡ `EvaluationResult.fallback_used`** and **`allowFailOpen` ≡ `fallback_allow`** — a clean 1:1 that *validates E6-S7's whole reconciliation*. **Recommendation:** re-ground the comments (`enforce.go:76-96,127,145-153`, `creds.go:98-99`) to `_network_failure`/`on_api_error`/`fallback_used`; optionally rename the marker to reduce future confusion. Code behavior can stay.

### 2e. Guardrails — ✅ field-compatible
NEW `GuardrailsResult` (`results.py:76-102`): `redacted_input:Any`, `input_type` (`"activity_input"|"activity_output"`), `raw_logs`, `validation_passed:bool` (False→stop), `reasons:[{type,field,reason}]`. shift-left `GuardrailResult`/`guardrailsWire` (`client/verdict.go:59-159`) parses `validation_passed` + `reasons` and deliberately drops `redacted_input`/`raw_logs` (INV-2, SL-9). ✅ compatible; category-only projection preserved.

### 2f. Approval / HITL — ⚠️ RESHAPED (N/A today, matters for the deferred story)
Endpoint `POST /api/v1/governance/approval`, body `{workflow_id, run_id, activity_id}` (`client.py:226`) — unchanged. NEW `ApprovalResult` (`results.py:196-288`) is **strict**: `action` beats `verdict`; unknown/empty → `None` (pending), **not** ALLOW; expired → blocking unless explicitly allow-shaped; `id`→`approval_id` normalized. The poll loop moved into `ApprovalPoller` (`approvals.py`).
- shift-left **does not poll** (OD-HITL → CC native `ask`; `enforce.go:507-530`), so the strict parsing is **not exercised today**. ✅ no current bug. **But** the deferred "approval-outcome capture" story (noted in E6-S6) MUST adopt the new strict `ApprovalResult` semantics — a port of the OLD lenient dict would be unsafe.

### 2g. Registration — ✅ STABLE (backend contract)
`agent/create` response `{data:{token, identity:{did, privateKey}}}` (`cli/internal/backend/client.go:96-128`) is a **control-plane** contract owned by openbox-backend, not the SDK — untouched by the refactor. The new SDK's config-side identity model (`AgentIdentity`, `OpenBoxConfig.resolve`, `instructions.md:106-111`) is the analog of shift-left's `DevConfig`/`creds.go`. No change.

---

## 3. Functionality mapping — the `FrameworkAdapter` seam

The base SDK funnels **all** enforcement through one adapter (`adapters/base.py:31-68`). Shift-left's `mapVerdict`/`applyDecision` **are** a `FrameworkAdapter` implemented in Go against Claude Code's `permissionDecision`:

| `FrameworkAdapter` (base.py) | Shift-left equivalent | Notes |
|---|---|---|
| `raise_hook_blocked(result)` — BLOCK/HALT, op NOT run | `mapVerdict`→`deny` on HALT/BLOCK/guardrail-fail (`enforce.go:465-479`) → `permissionDecision:"deny"` | ✅ pre-execution semantics identical; `_raise_stop` HALT→GovernanceHaltError, else Blocked (`base.py:115-123`) ≡ shift-left's single `deny`. |
| `handle_approval(result)` — REQUIRE_APPROVAL, before op | `mapVerdict`→`ask` + `approvalReason` (`enforce.go:475-476,524-530`) | ✅ **adapter-native** — exactly the new model (adapter drives approval). CC's `ask` is the interactive resolve (OD-HITL) vs the base `CoreAdapter`'s poller (`base.py:85-100`). Both fail *safe*. |
| `on_completed_hook_result(result, ctx)` — op ran, mark future only | PostToolUse observe (no enforce) | ✅ "never undo" upheld. |
| `raise_lifecycle_blocked(result)` — lifecycle BLOCK/HALT | (shift-left enforces only at the tool/PreToolUse hook, not session lifecycle) | ⚠️ shift-left has no lifecycle-level block (a session-scoped HALT). Likely fine for the dev runtime; note as a scope choice. |

**Verdict cascade home moved:** OLD `verdict_handler.enforce_verdict:50-103` → NEW `gate.raise_for_verdict` (`gate.py:166-185`), **same priority** HALT>BLOCK>guardrails>REQUIRE_APPROVAL>CONSTRAIN>ALLOW, same error types, but the `VerdictEnforcementResult`/`requires_hitl` struct is **gone** (approval is adapter-driven). shift-left's `mapVerdict` (`enforce.go:465-479`) and `sidecar/evaluator.go` max-severity aggregation still match the priority; **re-cite** `gate.raise_for_verdict` + `FrameworkAdapter` instead of `verdict_handler.enforce_verdict`.

**Redaction moved to adapter territory:** OLD `_apply_input_redaction` (`activity_interceptor.py:441-478`) has **no core equivalent** now — re-applying `redacted_input` onto args is a framework-adapter responsibility. shift-left's E6-S4 `applyInputRedaction` (`enforce.go:419-441`) is therefore **already aligned** with the new model (adapter applies it). Re-cite. **New gap:** the base SDK added an *outbound* request-side key redaction (`PrivacyConfig.redact_keys` → `serialization.apply_redaction`, `gate.py:159-162`) applied before signing — shift-left has **no equivalent**; it relies on content-gating + content-stripping (`client.Emit`). Consider adopting `redact_keys` for defense-in-depth on the observe egress path (INV-2-relevant).

---

## 4. The structural divergence (OD-class decision) — dev-event vocabulary vs the Activity/hook wire model

**This is the one big fork.** The base SDK's blessed wire `EventType` set is **only** `WorkflowStarted / WorkflowCompleted / WorkflowFailed / SignalReceived / ActivityStarted / ActivityCompleted / Handoff` (`events.py:50-59`), `source="workflow-telemetry"` (`events.py:47`), and a tool/operation **preflight serializes as `ActivityStarted` + `hook_trigger=true` + non-empty flat `SpanData` + `span_count`** (`events.py:365-401`; guide "Wire Contract Requirements" `instructions.md:273-285`; enforced by `assert_hook_wire_shape` `conformance/fake_core.py:132-170`).

Shift-left instead emits **7 developer-runtime event types** — `SessionStarted / PromptSubmitted / ToolCall / ToolResult / SessionEnded / CommitCreated / Deploy` with `source="developer-runtime"` (`client/event.go:31-41`, `contracts/dev-event/schema/dev-event.schema.json:29-37`, `MAPPING.md §1`), which required **patching Core's accept-list** (SL-13 `contracts/dev-event/ext-core/`; memory: *dev types not accept-listed*).

Consequences:
- Shift-left telemetry **would fail** the base SDK's `assert_hook_wire_shape` (wrong `event_type`, no `hook_trigger`, dev-shaped spans vs flat `SpanData`).
- Shift-left carries a maintained Core patch (EXT-core) the base SDK's canonical adapters do not need.
- Shift-left keeps dev-native richness (`semantic_type` = `file_read`/`mcp_tool_call`/…, session/prompt/commit/deploy lifecycle) the Activity/hook model does not express directly.

**The decision (OD — owner: brian; recommend an ADR):**
- **(A) Keep the dev-runtime vocabulary** as a sanctioned divergence (status quo + EXT-core). Lowest churn; preserves dev-native semantics; but shift-left never conforms to the base wire contract and carries the Core patch indefinitely.
- **(B) Re-express onto the base model**: developer session → a `workflow`; tool call → `ActivityStarted` + `hook_trigger=true` + flat `SpanData`; prompt/commit/deploy → activities/signals. Drops EXT-core, passes the canonical conformance kit, unifies dashboards with framework SDKs — but is a large migration and must find a home for dev-native `semantic_type`/commit/deploy metadata (likely span attributes).
- **(C) Hybrid**: emit the base wire shape (ActivityStarted+hook spans) as the *envelope*, carrying dev-runtime specifics inside `SpanData` attributes + `metadata`. Best of both if the flat-span attribute space can hold `semantic_type`/session/commit/deploy — needs a spike.

This is the highest-leverage question in the migration and blocks whether shift-left can adopt the base conformance kit (§5).

---

## 5. Conformance kit — align to the canonical parity matrix

The base SDK ships a reusable kit (`conformance/fake_core.py`, `hook_preflight.py`, `instrumentation.py`) and a **required-cases matrix** (`tests/conformance/test_required_cases.py`) every framework SDK runs: started-BLOCK-not-sent, HALT halt-requested, REQUIRE_APPROVAL rejected/allowed, DB/file/function preflight-block, completed-block-marks-future-only, context bind/reset, and `assert_hook_wire_shape` (`instructions.md:299-341`).

Shift-left built its own E6-S7 suite (`adapters/claude-code/enforce_conformance_test.go`, C1–C9) that already covers the **behavioral** half (BLOCK denies, fail-open/closed, HALT, staleness, observe-never-blocks). Being Go, it **cannot run the Python fixtures**, but it should **mirror the required-cases behaviors by name** so parity is legible. The **wire-shape** half (`assert_hook_wire_shape`) is only achievable under §4 option (B)/(C).
- **Recommendation:** map E6-S7 C1–C9 onto the base required-cases matrix (a small table in the E6-S7 doc), and gate "wire-shape conformance" on the §4 decision.

---

## 6. Findings, prioritized

**P0 — decision required**
1. **§4 dev-event vocabulary vs base Activity/hook wire model.** OD-class; recommend an ADR (A/B/C). Blocks base-conformance-kit wire parity and determines the long-term fate of the EXT-core patch.

**P1 — correctness-adjacent / hardening**
2. **§2f approval strictness** — safe today (no polling), but the deferred approval-outcome story MUST use the new strict `ApprovalResult` (`action`-beats-`verdict`, unknown→pending, expired→blocking). Record now so the port isn't stale.
3. **§3 outbound `redact_keys`** — the base SDK gained request-side key redaction before signing; shift-left has none. Evaluate adopting it for the observe egress path (INV-2 defense-in-depth).
4. **§2c parse `fallback_used`** on the observe path so advisory telemetry can distinguish a real ALLOW from a fail-open ALLOW (mirrors `Decision.FailOpen`).

**P2 — re-grounding (doc/comment, no behavior change)**
5. **§2d fail-policy idiom** — comments cite the dead `_handle_api_error`; re-ground to `_network_failure`/`on_api_error`/`fallback_used`. Note `Decision.FailOpen ≡ fallback_used`.
6. **§3 cascade + redaction homes** — re-cite `gate.raise_for_verdict` (was `verdict_handler.enforce_verdict`) and adapter-owned redaction (was `activity_interceptor._apply_input_redaction`).
7. **§1 entry-point** — re-ground `create_openbox_worker` references to `OpenBoxRuntime` + `FrameworkAdapter` (`doc.go:8-10`, `creds.go:98-99`, and the SDK-parity comments enumerated in the inventory).
8. **§2a SDK-Version value format** — align `X-OpenBox-SDK-Version` to `openbox-{engine}-{language}-v{version}`.
9. **Verify** the `DevEvent.timestamp` format (payload convention is RFC3339 `Z`; signing stays `+00:00`) — confirm shift-left doesn't reuse one for both.

**Non-issues (confirmed stable — do not touch)**
- AIP signing (§2a), verdict wire values (§2b), evaluate endpoint + response keys (§2c), guardrail fields (§2e), `agent/create` (§2g). The `FrameworkAdapter` seam (§3) is a clean conceptual match to shift-left's enforce path.

---

## 7. Suggested next steps
- **Write ADR-0004** for §4 (dev-event wire model): A/B/C, with a spike for (C)'s flat-span attribute capacity. *(This is the gate for everything wire-related.)*
- **Batch the P2 re-groundings** into one low-risk "SDK reference refresh" story (comments + the `X-OpenBox-SDK-Version` value + the `fallback_used` parse) — no behavior change, keeps the parity claims honest.
- **Record P1 items** as backlog: approval-outcome strictness (folds into the deferred E6-S6 outcome-capture story), `redact_keys` adoption (new NFR-1/INV-2 story).
- **Map E6-S7 C1–C9 → the base required-cases matrix** in the E6-S7 doc so behavioral parity is legible even though the Python fixtures can't run in Go.

**Bottom line:** shift-left is *not broken* by the SDK refactor — its wire contracts hold and its enforce path is already shaped like a `FrameworkAdapter`. The work is one strategic wire-model decision (§4) plus a batch of honest re-grounding and two small hardening adoptions.
