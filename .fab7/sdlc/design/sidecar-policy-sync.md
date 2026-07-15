# Design — Sidecar Policy Sync (closing `[EXT-opa-bundle]`)

**Status:** draft
**Author:** Claude (design synthesis for brian) — 2026-07-14
**Scope:** How the local decision sidecar obtains org policy from OpenBox and evaluates a developer tool call with **verdict parity to openbox-core**. Closes the `[EXT-opa-bundle]` seam left by E6-S5.
**Sources (code-cited):** `openbox-backend` (NestJS control plane), `openbox-core` (Go data plane), `sidecar/` + `adapters/claude-code/` in this repo. Every claim below cites a symbol/path.
**Decisions ratified (brian, 2026-07-14):** evaluator strategy = **embed OPA rego** (regoEvaluator, rego primary); distribution = **pull-at-init + session-start staleness prompt (no background poll)**; staleness detection = **client-side version compare**; stale + fail-closed org = **block session until refresh**.

---

## 0. Problem

E6-S5 built the sidecar with two pluggable seams — `BundleSource` (`sidecar/sync.go:35-37`) and `Evaluator` (`sidecar/evaluator.go:18-20`) — but shipped only a **local-file** source (`FileBundleSource`, `sidecar/sync.go:109-133`) and a **metadata-only JSON** evaluator (`bundleEvaluator`, `sidecar/evaluator.go:33-70`). There is **no sync of policy from OpenBox**; policy arrives only when an operator writes `~/.config/openbox/policy-bundle.json`. This is the `[EXT-opa-bundle]` gap (`sidecar/bundle.go:33-36`, `sidecar/sync.go:27-30`).

This design specifies the real sync + a faithful evaluator so the sidecar's verdict on a tool call matches what openbox-core would return for the equivalent event.

---

## 1. How OpenBox authors + verifies policy today (the mechanism to replicate)

### 1a. Rule authoring & compilation (openbox-backend)

- A "rule" is one **active `PolicyEntity` per agent** (`src/modules/policy/entities/policy.entity.ts:16-68`). Create deactivates all others for that agent (`policy.service.ts:160-164`) → an agent has exactly one active policy.
- A policy is **either raw rego** (`rego_code`) **or** a structured **`policy_builder`** config in `config.policy_builder` — `{version, rules:[{decision, matchMode:'all'|'any', conditions:[{field, operator, value, valueType, transform}]}]}`, decisions `ALLOW | REQUIRE_APPROVAL | BLOCK | HALT`, operators `equals/not_equals/contains/greater_than.../exists...` (`src/modules/policy/utils/policy-builder.util.ts:4-45`). The builder compiles to rego at save via `getEffectiveRegoCode` (`policy.service.ts:82-102`).
- **Package namespacing:** `formatRegoCode` force-wraps every policy into package `org.<orgId>.policy_<policyId>` (`src/common/utils/format-rego-code.ts`), and stores the query path in `config.path = org/<orgId>/policy_<id>`.
- **Bundle build/distribution:** `deployAllPolicies` (`policy.service.ts:492-559`) writes per-org `.rego` files + a `.manifest` (`roots:["org/<orgId>"]`), runs `opa build -b … -o bundle.tar.gz`, and uploads to S3 at a **fixed, unversioned key `bundle.tar.gz`** (`src/modules/s3/s3.service.ts:37-50`) — **one global bundle for all orgs**. There is **no bundle/version/manifest endpoint**; the server-side OPA polls S3.
- **Behavior rules** (`agent_behavior_rules`, multi-span sequences) compile to `org.<orgId>.behavior.agent_<agentId>` — **out of scope here** (§4).
- **Auth to read policy:** org **`X-API-Key`** (org-scoped) + `ReadAgentPolicy` permission (`src/modules/auth/guards/jwt-auth.guard.ts:39-61`). `GET /agent/:agentId/policies/current` returns the full `PolicyEntity` including **`rego_code` + `config`** (`agent.controller.ts:661`, DTO `agent-response.dto.ts:314-360`).

### 1b. Verification at evaluate time (openbox-core)

Core is a **pure OPA query client** — embeds no rego, pushes none:

1. `/governance/evaluate` (`internal/api/governance.go:28`) receives `GovernanceEventPayload` (`internal/content/governance.go:198-248`); agent/org/trust derived from the **signed identity**, not the payload.
2. `buildOPAInput` (`internal/services/opa.go:459-477`) shapes the `input` doc: event fields + `addAgentInfo` (`trust_tier`) + `addActivityFields` + prompt/messages hoisting + `addSpansToInput` → per-span `buildSpanMap` (`opa.go:619-719`) with `semantic_type`, `http_*`, `db_*`, `file_*`, `function`, `args`, `request_body`…
3. POST `{"input":…}` to external OPA (`OPA_URL`, default `localhost:8181`) at `/v1/data/{config.path}`, reading `result.result.decision` (`opa.go:316-369`).
4. **decision → action** (`opa.go:257-270`): `continue|allow→ALLOW`, `block→BLOCK`, `stop|halt→HALT`, `require_approval|require-approval→REQUIRE_APPROVAL`, default→ALLOW. Undefined result → ALLOW (`opa.go:240-255`). Note `constrain` is **not** a policy decision string (CONSTRAIN comes from guardrails/AGE).
5. **Verdict enum** `ALLOW=0 CONSTRAIN=1 REQUIRE_APPROVAL=2 BLOCK=3 HALT=4` (`internal/content/governance.go:34-44`), aggregated by **max-severity** `HighestPriorityVerdict` (`governance.go:117-131`) across policy + guardrails + AGE.
6. **Fail semantics:** policy-OPA unavailable → **BLOCK** (fail-closed, `activities/governance/policy.go:46-64`); no policy → ALLOW (`policy.go:29-37`).

### 1c. What the sidecar already ports faithfully (E6-S5)

`decisionToVerdict` (`sidecar/bundle.go:165-183`) mirrors `opa.go:257-270`; `verdictPriority` (`sidecar/evaluator.go:75-90`) mirrors `HighestPriorityVerdict`; `BuildOPAInput` (`sidecar/input.go`) mirrors `buildOPAInput` but is **currently unused** on the decision path. The plumbing is in place; only the rule bodies and the fetch are missing.

---

## 2. Fidelity boundary — what the sidecar can/can't replicate

| Layer | Local? | Rationale |
|---|---|---|
| Input shaping, semantic_type, verdict enum + max-severity | ✅ | Pure transforms, already ported (§1c) |
| **Per-request POLICY rule bodies** | ✅ **via embedded rego** | Rule predicates live only in `rego_code` — a JSON evaluator can express only the builder subset; evaluating the real rego is the only faithful path (**this is the ratified choice**) |
| Behavior rules (multi-span) | ❌ v1 | Needs hydrated span history + `AGEResult` contract (`opa.go:428-438`) |
| Guardrails redaction | ❌ | Separate external mechanism → `[EXT-guardrail-redaction]` seam (E6-S4) |
| AGE / goal-alignment | ❌ | External LlamaFirewall client (`age.go:167+`) |

**Scope of this design: per-request policy verdict parity.** Behavior/guardrails/AGE remain server-side and are reached via the async `/evaluate` telemetry channel, not the local gate.

### 2a. OPA bundle is enough for POLICY, not for FULL verdict (confirmed by `docs/diagram/openbox.png`)

The architecture diagram shows core fanning out to **three independent evaluators**, combined by max-severity (`HighestPriorityVerdict`, `governance.go:117-131`):
- **OPA app (Policy + Behavior rule)** ← S3 bundle — the only one the sidecar embeds.
- **Guardrail API** — NSFW/PII/Regex, backed by two FastAPI **model** servers + **Qwen3-8B** — not shippable to a dev laptop.
- **LLamaFirewall (Goal Drift)** — backed by **Qwen3-8B** — same.

**Consequence (state as accepted posture, not accident):** because the final verdict is **max-severity** and the sidecar computes **only the OPA/policy dimension**, the local verdict is a **floor** — it never over-blocks relative to policy, but it **can under-block relative to core**. A tool call that core would BLOCK/CONSTRAIN via Guardrail (PII/NSFW) or LLamaFirewall (drift) the OPA-only sidecar will ALLOW. This is acceptable **only** under the posture: *policy is the local blocking gate; content-guardrail + goal-drift are server-side/advisory via async `/evaluate`* (the Phase-1 metadata-only default). → **OD-SYNC-7.**

Two nuances from the diagram: (1) behavior rules **are** in the OPA bundle (`org.<org>.behavior.agent_<id>`); what blocks local behavior eval is the hydrated **span-history input**, not the rego — so behavior is v1-out-of-scope on input-state grounds, not bundle-availability. (2) The **Regex** leg of Guardrail is deterministic and *would* be locally replicable; the NSFW/PII/drift **model** legs are not — a possible future local-content optimization, distinct from the model paths which stay behind `[EXT-guardrail-redaction]` (E6-S4).

### 2b. What each server-side check actually needs (from core code recon)

Cross-repo recon (`openbox-core` guardrails/AGE) refines §2a — most of these are **not** LLMs:

| Check | Real compute | Blocks in core today? |
|---|---|---|
| PII (Guardrail) | Presidio **NER classifier** (small) + **deterministic span masking** (`ValidatedOutput`/`error_spans`, not generative) | yes (on_fail=block) |
| NSFW / Toxicity | **threshold classifiers** (small, score-based) | yes |
| Regex | pure code — **not wired in core's Go orchestrator** (`guardTypeToName` has no regex entry) | n/a |
| Behavior rules (AGE) | **OPA/rego** — deterministic, already in the bundle | yes |
| Goal drift (LlamaFirewall `/scan_replay`) | **reasoning LLM judge**, separate external service | **no — advisory** (`newGoalDriftResult` → `VerdictAllow`; "logged, not blocked") |

Implications: (a) the *blocking* model-backed gap is **PII/NSFW, which are small classifiers, not LLMs** — locally replicable without a big model if a real requirement appears (bundle a classifier process, or a scoped synchronous call to the existing Guardrail API via `[EXT-guardrail-redaction]`); (b) **goal-drift is the only reasoning-LLM component and it does not block**, so it is the lowest-value thing to localize.

### 2c. Considered & rejected: sidecar-as-MCP-server asking the agent's LLM to judge

Idea: make the sidecar an MCP server so it can ask the developer's Claude Code LLM to perform the guardrail/drift judgment (reuse an LLM already present, avoid running model infra). **Rejected for enforcement**, three reasons in increasing severity:
1. **MCP direction** — MCP = the *agent calls* server tools; it gives the server no way to invoke the agent's LLM. The mechanism doesn't exist.
2. **Reentrancy** — the enforce decision runs inside a PreToolUse hook while Claude Code is *blocked waiting* for the hook; there is no primitive to make the suspended agent perform a sub-evaluation mid-gate. (`ask` asks the *human*, not the LLM.)
3. **Trust inversion (fatal)** — drift/guardrail judgment must be independent of the governed agent; a misaligned agent asked to judge itself will self-clear. LlamaFirewall deliberately uses a **separate** model instance replaying recorded outputs. The governed cannot be its own judge.

**Salvageable:** the cost insight (reuse an LLM you have) is valid only if the judge stays *independent* — i.e. the sidecar calls an independent model (cheap API model or a separate local model). That is a network/inference hop → fits only the **advisory/async** or a scoped `ask`-tier path, never the single-digit-ms local blocking path (S2/OD6). A cooperative MCP "check-before-you-act" tool is a *nudge*, not enforcement (skippable). → folds into OD-SYNC-7.

---

## 3. Design

**Distribution model = pull-at-init + session-start staleness prompt (no background poll).** The policy is fetched once by the CLI (`openbox dev init` / `openbox dev sync`), written to the local bundle file, and loaded by the **existing** `FileBundleSource`. The resident daemon does **no network I/O at all** — it is a pure local file-loaded evaluator, which strengthens INV-3b. Freshness is a **client-side check at session start**, not a background loop.

```
                    ┌─────────────────────────────── one-shot, out-of-band (CLI, not the daemon)
backend policies    │  openbox dev init | dev sync
   │  signed GET /agent/:agentId/policies/current  (org X-API-Key)
   ▼                │  → write rego_code + config.path + PIN (policy.id, updated_at) to bundle file
FileBundleSource ───┘──SetBundle──►  Server (in-memory, NO network)
                                        │  each hook (local, no I/O)
regoEvaluator (new Evaluator impl)  ◄───┘
   │  OPA rego eval of data.<config.path>.decision  over BuildOPAInput(DecisionRequest)
   ▼
decision string → decisionToVerdict → verdict → CC permissionDecision (E6-S2 apply, unchanged)

session start (adapter, once per session — NOT per tool call):
   signed GET /agent/:agentId/policies/current → (id, updated_at)
   == local PIN ?  ── yes ─► proceed (all decisions local for the session)
                  └─ no  ─► fail-open org: warn "run openbox dev sync" + proceed stale
                            fail-closed org: BLOCK session start until refreshed
```

**Why the staleness signal rides on session start, not a tool-call 403:** the decision path never calls the backend (S2: synchronous `/evaluate` is a ~1.6 s NO-GO — the reason the sidecar exists). The only server-bound calls are session register/preflight (backend) and the async `emit` spool (core, fire-and-forget). So staleness is caught **once per session**, at start. A session runs under the policy pinned when it started; a mid-session policy change is picked up at the next session or an explicit `dev sync`. Acceptable for short-lived dev sessions; named as the trade vs. a poll.

### 3a. `regoEvaluator` (new `Evaluator` impl) — **rego primary**

- Embed the OPA Go `github.com/open-policy-agent/opa/rego` package. On `SetBundle`, **compile once** (`rego.New(rego.Query("data."+path+".decision"), rego.Module(...))` → `PrepareForEval`) and hold the prepared query + the bundle's `config.path`.
- On each `DecisionRequest`, build the OPA `input` via the **existing `BuildOPAInput`** (`sidecar/input.go`) — finally wiring it onto the decision path — and `Eval`. In-process, no network → INV-3b holds.
- Map the resulting `decision` via the **existing `decisionToVerdict`** (`sidecar/bundle.go:165-183`); undefined/empty → ALLOW (matches `opa.go:240-255`).
- Drops in behind `Evaluator` with **zero change** to `Server.decide` (`sidecar/server.go:179-215`) or the E6-S2/S3/S4/S6 apply cascade — exactly as the seam comment anticipated (`evaluator.go:9-14`).
- **`bundleEvaluator` (JSON) is retained** as the offline/air-gapped fallback for builder-only policies and hand-authored bundles.

**Input-shaping parity is the correctness crux.** The sidecar's `DecisionRequest` is metadata-only (tool name/kind, permission_mode, file_path/operation, mcp_function, local-only command). `BuildOPAInput` must map a CC hook event → a `spans[]` entry with the same `semantic_type` + `file_*`/`function`/`http_*` keys core produces, or a rule that keys on `input.spans[_].semantic_type` won't fire. **This mapping must be conformance-tested against core's `buildSpanMap` field names** (see §5).

### 3b. `dev init` / `dev sync` fetch (CLI-side, one-shot) — **reuse, no new surface**

- The **CLI** (not the daemon) calls **existing** `GET /agent/:agentId/policies/current` with the org `X-API-Key`, **reusing the SDK client signer** and the SL-11 `dev verify` / SL-15 `OwnershipVerifier` signed-GET pattern (`E6-backlog.md:22`).
- Parse `{rego_code, config.path, id, updated_at}` from the `data.data` envelope (same envelope gotcha as [[sl15-ownership-verifier]]).
- Write `rego_code` + `config.path` **plus a PIN `(policy.id, updated_at)`** into the local bundle file. The daemon loads it via the **unchanged `FileBundleSource`** — no new `BundleSource` impl, no HTTP client in the daemon.
- **`openbox dev sync`** is the lightweight refresh verb: re-fetch + rewrite the bundle only. Full `openbox dev init` (plugin + hooks + register) is reserved for install; the staleness prompt points at `dev sync`, not `dev init` (full re-init to refresh a bundle is too heavy). `dev init` calls the same fetch as its last step.
- **No new backend endpoint** (CLAUDE.md reuse rule; a new endpoint would need an ADR), **no S3 credentials**, **org-scoped by the API key** (no multi-tenant leak, unlike the global `bundle.tar.gz`).
- **The 60s ticker (`syncLoop`/`syncOnce`, `sidecar/sync.go:44-84`) is removed** for the online path (or left inert). Rationale: with no in-daemon fetch there is nothing to poll; freshness is the session-start check in §3c.

### 3c. Session-start staleness check (adapter-side, client-side compare)

- **Once per session** (session-start hook / register step — NOT per tool call), the adapter does a signed `GET /agent/:agentId/policies/current` and compares the returned `(id, updated_at)` to the local PIN.
- **Match** → proceed; all decisions are local for the session.
- **Mismatch, fail-open org (OD9 default)** → surface a warning ("policy changed — run `openbox dev sync`") and **proceed on the stale bundle** (staleness never denies).
- **Mismatch, fail-closed org** → **block session start** (refuse/hard-degrade the governed session) until `openbox dev sync` runs — no silent under-enforcement window.
- **Detection is client-side** (the adapter decides) → **no server 403/409, no core change, no ADR**. Note: the correct server-driven code, if ever added as defense-in-depth, is **409 Conflict / 426 Upgrade Required — never 403** (403 collides with real auth failures and the client couldn't distinguish stale-policy from bad-key). Deferred (OD-SYNC-5).
- Reuses the SL-15 signed-GET; a single GET at session start is well under any budget (it is not on the tool-call path).

### 3d. Fail semantics (deliberate, documented deviation)

Core is fail-**closed** when policy-OPA is down (`policy.go:46-64`); the sidecar stays fail-**open** on daemon/infra faults (absent bundle → ALLOW) per **OD9** — same deviation ratified in [[e6-s3-fail-closed-policy]], reconciled in [[e6-s7-conformance-info1]]. Two distinct axes now:
- **Daemon unreachable/unbundled at decision time** → existing E6-S3/S7 `applyFailurePolicy` (fail-open proceeds; fail-closed org denies). Unchanged.
- **Policy stale at session start** → §3c: fail-open warns+proceeds-stale, fail-closed blocks-until-refresh. **New; the enforcement-window closer.**
- **`dev sync` fetch failure** (offline at refresh) → keep the last-good pinned bundle; never denies at fetch time.

---

## 4. Explicitly out of scope (v1)

- **Behavior rules** — multi-span; needs span-history hydration + the `AGEResult` rego contract. Reachable via async `/evaluate`, not the local gate.
- **Guardrails redaction** — `[EXT-guardrail-redaction]` (E6-S4); the regoEvaluator produces no `redacted_input`.
- **AGE / goal-alignment** — external client; cannot be offline.
- **Pulling the S3 `bundle.tar.gz`** — rejected: global multi-org bundle + requires KMS/S3 creds + no per-org isolation. The per-agent signed GET is strictly better for a dev's machine.

---

## 5. Verification plan (conformance parity)

Extend the E6-S7 conformance suite (`adapters/claude-code/enforce_conformance_test.go`):

1. **Verdict parity harness** — for a fixture rego + a fixture event, assert `sidecar regoEvaluator` verdict == core's `decisionToVerdict(opa result)`. Golden fixtures shared with core's `opa_test.go` decision strings.
2. **Input-shape parity** — assert `sidecar/BuildOPAInput` emits the same field names core's `buildSpanMap` does for the tool kinds a CC hook produces (file read/write, mcp_tool_call, http, command). This is the load-bearing test — a name mismatch silently no-fires a rule.
3. **Staleness detection** — a new policy version at the backend makes the session-start check report mismatch; an unchanged version reports match. `dev sync` rewrites the bundle + PIN so the next session matches.
4. **Stale + fail-open** → session start warns and proceeds on the stale bundle; no deny attributable to staleness. **Stale + fail-closed** → session start blocks until `dev sync` runs (load-bearing regression guard for the enforcement window).
5. **Offline at refresh** — `dev sync` fetch failure keeps the last-good pinned bundle; never denies at fetch time. **Decision-time daemon faults** still follow E6-S3/S7 (unchanged).
6. **Org isolation** — org-A key never yields org-B rego.

---

## 6. Open decisions (human-only; surface, don't infer)

- **OD-SYNC-1 — evaluator strategy.** ⛔️ **SUPERSEDED by ADR-0005** (accepted brian 2026-07-15, built E6-S8). The 2026-07-14 "embed OPA rego" resolution is REVERSED: embedding OPA's `rego` package is the only pure-Go/no-cgo option and it pulls a large dependency tree into `bin/openbox` (violates OD-SYNC-2/OD17); the lightweight interpreters (Regorus/rego-cpp) are cgo-only. E6-S8 instead evaluates the `config.policy_builder` structured config **natively in pure Go** (`sidecar/builder.go`, FIRST-MATCH, faithful to `policy-builder.util.ts`), with a documented fail-open-local residual for hand-written raw rego (covered by T2/T3). The `Evaluator` seam is unchanged, so a rego/Regorus evaluator can drop in later. **Result-rule correction (ADR-0005 §Decision-4):** the generated policy's output rule is `result` (`{decision, reason}`), so the OPA query is `data.<pkg>.result` then read `.decision`/`.reason` — NOT `data.<path>.decision` as §2/§3 drafts stated; the native evaluator sidesteps the query but `BuildOPAInput` must still match core's `buildSpanMap` key names (the load-bearing E6-S8 parity fix).
- **OD-SYNC-2 — binary-size / dependency posture.** ✅ **RESOLVED by ADR-0005** (brian 2026-07-15): NO embedded rego engine — native pure-Go builder eval keeps `bin/openbox` small and cgo-free (OD17 intact); `go list -m all | grep open-policy-agent` finds nothing (regression-checked in E6-S8 validation).
- **OD-SYNC-3 — legacy raw-rego trust.** Raw-rego policies can express predicates the builder can't; the sidecar will evaluate them faithfully but they may reference `input` fields a dev-runtime event never carries (e.g. Temporal `workflow_type`). Such rules simply don't fire locally — acceptable, or should the sidecar warn on unmatched-required-field rego?
- **OD-SYNC-4 — sync auth artifact.** ✅ **RESOLVED by ADR-0005** (brian 2026-07-15): the org control-plane credential (`OPENBOX_CONTROL_TOKEN`: an `obx_key_…` org key on `X-API-Key`, or a Keycloak JWT) — NOT the per-session `obx_` agent key — since `read:agent_policy` is org-scoped. Reuses the `cli/internal/backend` control-plane client + the SL-15 org-key auto-classification.
- **OD-SYNC-5 — server-side staleness backstop.** ✅ Detection is client-side (brian 2026-07-14) → no server change. A **409/426** server backstop (defense-in-depth so a tampered client can't skip the check) is **deferred**; if adopted it is a core `/evaluate` change + ADR, and **never 403**.
- **OD-SYNC-6 — session-start hook cost.** The staleness check adds one signed GET per session start. Confirm this is acceptable latency at session open (it is off the tool-call path). *(recommend: accept; cache the result for the session.)*
- **OD-SYNC-7 — policy-only enforcement posture (from `openbox.png`).** ✅ implied by scope, but ratify explicitly: the local sidecar enforces **only the OPA/policy dimension**; Guardrail (PII/NSFW/regex) and LLamaFirewall (goal drift) verdicts are **not** enforced locally (model-backed, server-side). Local verdict is a **floor** — it can be more permissive than core. **Resolved by §7 tiering:** T2 sync `/evaluate` closes the floor for high-risk classes (Bash/MCP); edits accept the floor + T3 audit.
- **OD-SYNC-8 — hook timeout ownership + fail-behavior (corrects E6-S3 framing).** The "5 s kill" is **not a CC floor** — it is **our own `timeout: 5`** in `plugin/hooks/hooks.json` (PreToolUse matcher `*`). CC's *default* is **600 s**, configurable per-matcher. So we already own the timeout; the E6-S3 number is right but was mis-attributed to CC.
  - **✅ VERIFIED EMPIRICALLY (2026-07-14, CC v2.1.210): CC FAILS OPEN on a PreToolUse hook timeout.** Controlled nested-`claude` experiment (`scratchpad/hooktest/`): a hook `sleep 30` under `timeout: 2` → **the tool RAN**. Controls: `exit 0`→ran, `exit 2`→blocked (deny works and "most-restrictive wins" held alongside the observe hook), so the timeout→ran result is unambiguous. Undocumented, version-specific — do not depend on it.
  - **LOAD-BEARING CONSEQUENCE:** because CC fails open on timeout, a fail-closed gate must **never let CC's timeout fire.** Our binary MUST enforce a **tighter internal budget** and emit the explicit verdict itself on expiry (deny for fail-closed / proceed for fail-open — E6-S3 `applyFailurePolicy` unchanged), returning **before** the configured hook `timeout`. Invariant: **internal budget < configured hook `timeout`, with margin.** This re-justifies the E6-S3 clamp (the real reason is CC's fail-open-on-timeout, not a "5 s kill").
  - **Per-tier budgets map to per-matcher `timeout`:** split the single `*` PreToolUse block into a **Bash/MCP block** (T2: hook `timeout` e.g. 10 s > internal budget e.g. 8 s > ~1.6 s `/evaluate`+drift) and a **tight block for the rest** (T1: small hook `timeout`, <50 ms internal). Raise the E6-S3 internal 2 s clamp for T2 classes only.
- **OD-SYNC-9 — T2 drift-wait latency.** ✅ RESOLVED (brian 2026-07-14): **accept the full sync `/evaluate` latency for v1** (drift is advisory; Bash/MCP are rare). No core fast-verdict mode for now; revisit only if measured Bash latency hurts adoption.
- **OD-SYNC-12 — T2 fidelity vs content posture.** ⏳ surfaced by E6-S10 (brian to confirm). The T2 `/evaluate` event reuses the observe Mapper → metadata-only unless content capture is on (INV-2). So with content OFF (default) the server's Guardrail/LLamaFirewall classifiers (the §2a floor T2 exists to close, OD-SYNC-7) have no content to inspect, and T2 authoritatively closes only the POLICY dimension (server-side policy divergence from the local bundle). Full floor-closing needs content ON. **Question:** should high-risk T2 escalation egress the command/body EVEN under content-capture-off — a narrow, high-risk-only egress decoupled from the general opt-in (à la E6-S9's on-by-default LOCAL secret detection, but this one EGRESSES, so a distinct posture) — or keep strict INV-2? **Recommend: keep strict INV-2 for v1** (T2 value scales with the existing content opt-in; a decoupled high-risk egress is a real privacy change only brian can make).
- **OD-SYNC-11 — T2 delivery vs the observe spool (double-send).** ⏳ surfaced by E6-S10 (brian to confirm). The Tier-2 escalation sends the tool-call event synchronously to `/evaluate` with the SAME deterministic `event_id` the observe path spools; the SessionEnd flush re-sends the spooled copy. Two sends, one Idempotency-Key — designed to collapse once server-side dedupe lands (SL3-IDEMPOTENCY / EXT-core), a transient duplicate identical in kind to the accepted "no dev-event dedupe" gotcha. **Recommend: ACCEPT** (reuse-faithful — it exercises the SL-14 idempotency machinery; the alternative, suppressing the spooled copy, needs cross-process coordination and would drop the E7-S8 duration stash). Revisit only if the duplicate pollutes dashboards before dedupe lands.
- **OD-SYNC-10 — local secret detection posture.** ✅ RESOLVED (brian 2026-07-14): Tier-1 secret/entropy detection runs **on by default, local-only** — inspects Edit/Write content locally, never egresses (finding/redaction stays on `sidecar.Decision`, never `client.Evaluation`), decoupled from the content-capture-for-egress opt-in. Honors INV-2 (egress-only). `updatedInput` rewrites **content-only fields**, never structural locators (E6-S4/S7 carry-forward).

---

## 7. Verdict tiering & latency model (design rule)

**Ratified (brian 2026-07-14): full three-tier model, reusing `/evaluate` — T1 local, T2 sync `/evaluate` (high-risk classes only), T3 async `/evaluate` (audit/findings). Secret detection on by default, local-only.**

**Why this is a design rule, not a tuning knob:** a gating hook runs on *every* matched tool call. If the developer feels it, they remove it — and every gate upstream of CI evaporates. So tier by **risk class**, paying network cost only where the §2a fidelity floor actually bites. Practitioner budget: <500 ms to be tolerable, <50 ms to be imperceptible on edits.

### The three tiers

**Tier 1 — local, in-process, <50 ms. The only tier allowed to block Edit/Write.**
- Cached OPA policy (`regoEvaluator`, §3a) **+ NEW local deterministic secret/entropy detectors** (regex + high-entropy string detection, gitleaks/trufflehog-style).
- Blocks all tools incl. frequent low-risk edits; never touches the network.
- **Local content inspection ≠ egress:** secret detection reads Edit/Write content locally; the finding/redaction lives on the local `sidecar.Decision`, never `client.Evaluation` — so it honors INV-2 and runs **on by default** (OD-SYNC-10), independent of content-capture opt-in.
- Produces the **redact-and-continue** rewrite (secret → env-var ref) → feeds the **already-built E6-S4 `updatedInput` path**. This "fourth verdict" is ~half-built already; it needs a local redaction *source*, not new apply plumbing. Rewrite content-only fields, never `file_path`/`command`.

**Tier 2 — synchronous `/evaluate`, high-risk classes ONLY (Bash, MCP execution). Full server verdict.**
- Escalation: T1 local runs first (fast floor); if T1 allows, T2 calls **sync `/evaluate`** for the authoritative full verdict (policy + guardrail classifiers; drift advisory) before the tool runs. **This is how the §2a floor is closed — exactly where arbitrary execution makes it dangerous, and nowhere else.**
- Latency: `/evaluate` ≈ 1.6 s (S2, full pipeline). Tolerable for *rare* high-risk calls; unacceptable on edits (→ T1-only there). **Reuses `/evaluate` — no new core surface.**
- Timeout owned by our binary (OD-SYNC-8), not CC: explicit hook `timeout` backstop + tighter internal budget → E6-S3 fail-open/closed on *our* expiry.

**Tier 3 — asynchronous `/evaluate`, never blocks. Full model eval (guardrails + drift) → audit + findings.**
- Fire-and-forget spool (existing telemetry path) for classes that don't get T2 (edits, reads).
- **Findings-back-to-dev is the gap:** async results land in the dashboard today; surfacing them into the session (PostToolUse warning / SessionEnd summary) is the "findings" half — product follow-up.

### Per-tier budget (replaces the single E6-S3 clamp)
| Tier | Budget | Blocks? | Classes | Fail behavior |
|---|---|---|---|---|
| T1 | <50 ms local | yes | all (only tier for Edit/Write) | fail-open (absent bundle→allow), OD9 |
| T2 | ~1.6 s sync, bounded by explicit hook timeout | yes | Bash, MCP exec only | E6-S3 fail-open/closed on our-timeout |
| T3 | async, non-blocking | no | edits/reads (non-T2) | n/a (audit) |

### Feedback-option mapping
Option A ≈ this model. **Option B** (sync-everywhere, failClosed) = our **fail-closed strict-mode org** (E6-S3 primitive) — opt-in only, never default. **Option C** (local-block + remote-audit) = **v1 minus T2** = our prior floor; still the honest fallback if T2 slips.

---

## 8. Next step

Slice as **E6-S8 — Sidecar rego evaluator + pull-at-init policy distribution (`[EXT-opa-bundle]` close)** into `.fab7/sdlc/stories/`, dependency: E6-S5..S7 done (they are). Write scope:
- `sidecar/evaluator.go` (`regoEvaluator`, embed OPA rego), `sidecar/input.go` (wire `BuildOPAInput` onto the decision path), `sidecar/bundle.go` (carry the PIN `(policy.id, updated_at)`).
- CLI `dev init` / new `dev sync` verb — the signed `GET /policies/current` fetch + write bundle + PIN (reuse SL-15 signed-GET).
- Adapter session-start staleness check (client-side compare; fail-open warn+proceed, fail-closed block-until-refresh).
- Remove/inert the 60s `syncLoop` (`sidecar/sync.go`); `FileBundleSource` stays as the loader.
- Conformance tests (§5).
No change to core/backend surface. Consider **ADR-0005** to record embed-rego (OD-SYNC-1/2) + the pull-at-init distribution choice (parity-with-deviation posture, no background poll).

**Tiering follow-on stories (from §7):**
- **E6-S9 — Tier-1 local secret/entropy detection + redact-and-continue.** New deterministic secret detector in the sidecar evaluator; wire its redaction into the built E6-S4 `updatedInput` path (content-only fields). On by default, local-only (OD-SYNC-10). Highest adoption value; no core surface.
- **E6-S10 — Tier-2 sync `/evaluate` escalation for high-risk classes. ✅ BUILT 2026-07-15 (status=review; PENDING brian G3 + Sam G_SEC).** After a T1-allow on Bash/MCP, call sync `/evaluate` for the full server verdict; own the timeout in-binary (OD-SYNC-8); E6-S3 fail-open/closed on expiry. Closes the §2a floor for execution. Reuses `/evaluate` (no core surface). Default OFF (opt-in `OPENBOX_TIER2`). **In-binary budget** `defaultTier2Timeout` 3.5 s clamped to `maxTier2Timeout` 4 s < the shipped 5 s PreToolUse hook timeout (CC fails OPEN on a hook timeout, verified) — so `hooks.json` is UNCHANGED and the fail-closed deny lands with margin (conformance C15 + live E2E). **OD-SYNC-8 realization:** the literal "split the `*` matcher into a Bash/MCP block + a tight block" is infeasible — CC runs ALL matching hook blocks and its matcher is RE2 (no negative lookahead), so a `*` catch-all cannot EXCLUDE Bash/MCP (Bash would double-fire). The invariant it mandates (binary-owned budget < hook timeout, margin) is met more robustly by a SINGLE `*` matcher + a per-tier in-binary budget (T1 ≤ ~50 ms, T2 ≤ 4 s). **Cross-repo recon:** the SDK has NO risk-class tiering (every activity gets the full evaluate — `activity_interceptor.py`), so T2's tiering is a deliberate shift-left addition (the §7 rationale); the SDK fail behavior (`_handle_api_error`) is the reused `applyFailurePolicy`. Files: `adapters/claude-code/{enforce_tier2.go,creds.go,hookrun.go}` + tests (conformance C12-C17); ADR-0006 candidate (below). See `.fab7/sdlc/stories/STORY-E6-S10.md`.
- **E6-S11 — Tier-3 findings loop.** Surface async guardrail/drift results back into the session (PostToolUse warning / SessionEnd summary), not just the dashboard.
- **Recalibrate E6-S3 clamp** per OD-SYNC-8: replace the single 2 s "under 5 s kill" clamp with explicit per-tier budgets + an explicitly-configured hook `timeout`.

This tiering may warrant its own **ADR-0006 (verdict tiering & latency model)** since it's a load-bearing enforcement-architecture rule, not just a story detail.
