# Discovery Brief — openbox-shift-left

**Status:** draft (authored during brainstorm 2026-07-07)
**Owner:** brian@openbox.ai

## Context

OpenBox today governs **agent runtime** via code/SDK binding:

- Agents are registered through the control plane (`openbox-backend/src/modules/agent/agent.controller.ts:187`, `POST agent/create`) and receive a once-shown `obx_live_…/obx_test_…` API key stored as a hash, a DID, and AIVSS/trust scores (`openbox-backend/src/modules/agent/agent.service.ts:652`).
- The data plane authenticates every runtime call by token (`openbox-core/internal/api/main.go:139`, `openbox-core/internal/services/agent.go:260`) and enforces identity headers (`ValidateAgentIdentity`, `openbox-core/internal/services/agent.go:103`).
- Enforcement primitives: OPA app (policy + behavior rules, S3-backed), Guardrail API (PII model, NSFW model, regex, Qwen3-8B), LlamaFirewall goal-drift, KMS attestation (`docs/diagram/openbox.png`).
- Workloads bind via the SDK: `create_openbox_worker()` (`openbox-temporal-sdk-python/openbox/worker.py:43`) with server-side key validation (`openbox-temporal-sdk-python/openbox/config.py:252`).

**Shift-left vision** (`docs/diagram/shift-left.png`, `docs/diagram/diagram.png`): extend governance from agent runtime to **developer runtime** — agentic CLIs (Claude Code) and IDEs (Cursor). Two connected goals:

1. **Developer Governance** — the same policy/guardrail layer applied to coding-agent sessions (what tools, MCP servers, prompts, and commands a coding agent may use on a developer's machine).
2. **Lineage / provenance** — a DID-keyed chain: dev session → plans/brainstorm files → commit (git hash) → deploy (OpenBox git action) → runtime agent version (Local → Staging → Prod), answering: *How did the developer produce this commit? What tools/MCP/prompts were used? How many tokens (finops)?*

---

## Brainstorm 001 — How do we start developing the shift-left idea?

**Topic (one sentence):** What is the first buildable wedge that extends OpenBox governance from agent runtime to developer runtime (Claude Code, Cursor) while establishing session→commit→deploy lineage?

**Evaluation criteria:**
- C1. Reuses existing OpenBox primitives (agent registry/DID, Core policy eval, OPA, Guardrail API) instead of new infrastructure
- C2. Coverage across coding tools (Claude Code first, Cursor and others later)
- C3. Developer friction (install effort, latency, privacy comfort)
- C4. Governance strength (observe-only → advisory → enforce)
- C5. Time to first credible demo of the lineage story in shift-left.png

### Options (converged clusters)

#### A. Observe-first: session telemetry + lineage (recommended entry wedge)

**A1. Dev-session registration + telemetry ingestion.**
Register each developer's coding-agent instance as a first-class OpenBox agent (reusing `POST agent/create` → `obx_` key + DID), and ingest session telemetry: Claude Code natively exports OpenTelemetry metrics/events (tokens, cost, tool decisions, model) and fires lifecycle hooks — point OTLP export at an OpenBox collector endpoint.
- *Evidence:* agent registration/identity already exists (`openbox-backend/src/modules/agent/*`); Claude Code has documented OTel export (`OTEL_METRICS_EXPORTER`, `OTEL_EXPORTER_OTLP_ENDPOINT`) and hooks; the shift-left diagram already models developers (`developer-1/-2`) inside the *Runtime Agents → Local* box, i.e. dev sessions ARE agents in the existing model.
- *Assumption:* Cursor exposes comparable telemetry/hooks (verify — spike S1).
- *Tradeoffs:* no enforcement yet (weak on C4), but maximal on C1/C3/C5; produces the finops answer (token usage per session/commit) almost for free.

**A2. Session→commit binding via git trailers + OpenBox git action.**
A `PostToolUse`/`Stop` hook (or git `prepare-commit-msg` hook) stamps commits with a session ID trailer (e.g. `OpenBox-Session: <uuid>`); the already-envisioned OpenBox git action reads the trailer at push/deploy, registers `DID = git hash + timestamp`, and links deploy DIDs back to the originating sessions.
- *Evidence:* shift-left.png already specifies the git action and DID = (git hash, timestamp) records; Claude Code writes full session transcripts locally (`~/.claude/projects/*.jsonl`) that a trailer can reference.
- *Assumption:* teams accept commit-trailer conventions; monorepo/multi-session commits need a merge rule.
- *Tradeoffs:* the keystone of the lineage story (C5) and cheap; provenance is only as trustworthy as the local hook until attestation (see C2 option below) is added.

#### B. Enforce: policy at the developer runtime

**B1. Hook-based policy enforcement (Claude Code `PreToolUse` → OpenBox policy eval).**
A `PreToolUse` hook calls OpenBox Core's policy evaluation (OPA behavior rules) with the pending tool call (bash command, file write, MCP call) and allows/denies/asks based on the decision — the developer-runtime equivalent of what Core already does for runtime agents. Prompts/outputs can also be run through the existing Guardrail API (PII/secrets/regex).
- *Evidence:* OPA policy + behavior-rule evaluation and Guardrail API already exist (openbox.png; `openbox-core/internal/services/opa*.go`, behavior rules in `openbox-backend/src/modules/agent/utils/behavior-rule-rego.util.ts`); Claude Code hooks support blocking decisions (exit code / JSON `permissionDecision`).
- *Assumption:* policy round-trip latency is acceptable inside interactive sessions (target <200ms; needs local caching or a local OPA sidecar — spike S2); Cursor's hook maturity (spike S1).
- *Tradeoffs:* strongest C4 for Claude Code, moderate friction; per-tool integration work (weaker C2 until Cursor parity).

**B2. MCP gateway: an OpenBox MCP proxy.**
All MCP servers are wired through a local OpenBox MCP proxy that authenticates as the dev-session agent and applies policy/guardrails to MCP traffic uniformly — tool-agnostic for anything speaking MCP (Claude Code, Cursor, others).
- *Assumption:* teams centralize MCP config; non-MCP tool calls (raw bash) are NOT covered — complements, not replaces, B1.
- *Tradeoffs:* best C2 for the MCP slice; misses shell/file operations; new component to build.

**B3. LLM egress proxy (model gateway).**
Route dev tools' model traffic through an OpenBox gateway (`ANTHROPIC_BASE_URL`-style override) so prompts/completions pass the Guardrail API regardless of which coding tool is used.
- *Tradeoffs:* fully tool-agnostic (max C2) and catches prompt-level leaks (PII/secrets); but adds latency to every model call, sees only prompts (not tool execution), and TLS/key handling is sensitive. Heavier lift; candidate for phase 2+, not the wedge.

#### C. Identity & trust hardening

**C1. Developer/agent identity unification.** Model dev sessions as agents with their own DID and trust tier (reuse AIVSS/trust scoring) so "Developer Governance" and "Agent Governance" share one registry, one policy engine, one dashboard.
- *Evidence:* trust lifecycle schema already in place (`openbox-backend/src/migrations/1773076136339-add-trust-lifecycle-schema.ts`).
**C2. Attested provenance.** Sign the session→commit binding with the existing KMS attestation path so lineage is tamper-evident (later phase; depends on A2).

#### D. Packaging & adoption

**D1. Claude Code plugin (hooks + skill + managed settings) as the distribution unit;** enterprise managed-settings can mandate the hooks org-wide. Cursor equivalent once verified.
**D2. CI gate:** the OpenBox git action refuses to register a deploy DID for commits lacking governed-session provenance — the enforcement backstop that makes A2 meaningful without touching the IDE at all.

### Discarded ideas (with reasons)

- **Forking/patching the CLI/IDE binaries** — unmaintainable against fast-moving upstreams; hooks/OTel/proxy surfaces exist for a reason.
- **eBPF/kernel-level interception of dev machines** — disproportionate for v1; privacy and platform-support cost dwarf the benefit over hooks + egress proxy.
- **Full keystroke/screen capture for provenance** — privacy-hostile, low marginal value over transcripts + trailers; would poison adoption (C3).
- **Building custom guardrail models first** — existing PII/NSFW/regex/Qwen3-8B stack already covers the needed checks; model work is not the bottleneck.

### Ranked options against criteria (tradeoffs, no winner declared)

| Option | C1 reuse | C2 coverage | C3 friction | C4 strength | C5 demo speed |
|---|---|---|---|---|---|
| A1 session registration + OTel | high | CC now, Cursor TBD | low | observe | fast |
| A2 git trailers + git action | high | tool-agnostic | low | observe | fast |
| B1 PreToolUse policy eval | high | CC only (now) | medium | enforce | medium |
| D2 CI provenance gate | high | tool-agnostic | low | enforce (at deploy) | medium |
| B2 MCP proxy | medium | MCP-wide | medium | enforce (MCP slice) | medium |
| B3 LLM egress proxy | medium | max | medium-high | enforce (prompt slice) | slow |
| C2 attestation | high | n/a | low | integrity | slow |

**Coherent phasing implied by the table (subject to open decisions):** Phase 1 = A1 + A2 (+C1 modeling choice) — observe-only lineage demo end-to-end matching shift-left.png; Phase 2 = B1 + D2 — advisory→enforcing policy; Phase 3 = B2/B3 + C2 — cross-tool coverage and attested provenance.

### Spikes

- **S1 — DONE (2026-07-07).** Claude Code & Cursor integration surfaces fully mapped in `.fab7/sdlc/discovery/spikes/S1-dev-runtime-surfaces.md`. Verdict below.
- **S2 (open):** Policy-eval latency budget inside interactive sessions — remote Core call vs local OPA sidecar with synced bundles (S3 bundle distribution already exists). Gates the enforcement phase.
- **S3 — DONE (2026-07-07):** Session→commit attribution rules — `.fab7/sdlc/discovery/spikes/S3-commit-session-attribution.md`. Verdict: commit-message trailer as single source of truth (multiple `OpenBox-Session:` lines = fan-in, like Co-Authored-By), written idempotently in `prepare-commit-msg`, **resolved server-side at push against the real pushed SHA** (hooks don't travel; SHAs unstable until push). Only loss modes: fixup / GitHub-squash body-replace → explicit `unattributed`/`inferred` marker. Reuses `didFor`/`createHash`/`SessionMerkleLeafEntity`.
- **S4 — DONE (2026-07-07):** Privacy boundary — `.fab7/sdlc/discovery/spikes/S4-privacy-boundary.md`. Verdict: every Phase-1 goal met by metadata alone; CC/Cursor/Copilot all default content OFF; GDPR minimization + trade-secret law favor metadata-only. **OD4 DECIDED: metadata-only default, content opt-in per org, Guardrail-redacted at-source async when enabled.**
- **S5 — DONE (2026-07-07):** Codex CLI surfaces — `.fab7/sdlc/discovery/spikes/S5-codex-surfaces.md`. Verdict: Codex is a STRONG governable provider (hooks block+rewrite, native OTel, MCP, base-URL egress; best-in-class org mandate via `requirements.toml`/MDM). Filled the generic capability matrix (architecture §1b). Provider adapter model + unifying `openbox` CLI front door (OD12) added.

### S1 verdict — how shift-left integrates with each tool

**Hooks are the cross-tool integration primitive.** Both Claude Code and Cursor expose JSON-over-stdio hooks with allow/deny/ask semantics and per-MCP-call interception, and both vendors officially endorse hooks for governance. An OpenBox hook contract (emit telemetry + call policy) ports to both via thin per-tool adapters, packaged as a Claude Code plugin and a Cursor `hooks.json`/Team-hook bundle.

- **Claude Code = the strong surface.** PreToolUse deny/ask **plus input/output rewrite (redaction)**, native `http` hook type (OpenBox Core can be the endpoint directly), MCP matchers, native OTel (token/cost/commit metrics + tool-decision events — finops nearly free), and hard org mandate via managed settings (`enabledPlugins`, `allowManagedHooksOnly`, `disableSideloadFlags`, fail-closed refresh).
- **Cursor = viable but weaker.** Hooks are **v1.7 beta**, **fail open** on script error, telemetry is poll-based Admin API (≤1/hr, no OTel), and — critically — **Agent-mode LLM egress is NOT interceptable** (routes to Cursor's backend regardless of base-URL). So the gateway/egress-proxy idea (option B3) is Claude-Code-only; hooks are Cursor's only reliable path. Enterprise plan required for org mandate.
- **Consequence for the brainstorm option set:** B3 (LLM egress proxy) drops from "cross-tool" to "Claude Code optional extra." B1/B2 (hook + MCP interception) is confirmed as the portable enforcement path. A1 telemetry is easy on Claude Code (OTel), coarser on Cursor (Admin API). Transcript-parsing (original A1 sub-idea) is rejected on both tools.

### Decisions (elicited 2026-07-07, decided by brian)

- **OD1 — DECIDED: observe-first.** Phase 1 = A1 (session registration + telemetry) + A2 (git-trailer lineage + git action). Enforcement (B1) moves to Phase 2, gated on spike S2.
- **OD2 — DECIDED: Claude Code hooks** as the v1 surface (hooks + OTel + transcripts + managed settings). Proxy approaches (B2/B3) deferred; Cursor follows after spike S1.
- **OD3 — DECIDED: sessions as agents.** Reuse the existing agent registry, `obx_` keys, DID, and trust scoring — one namespace for developer and runtime governance, as shift-left.png draws.

### Decisions (elicited 2026-07-07 post-S1, decided by brian)

- **OD5/OD8 — DECIDED: Claude Code first, Cursor fast-follow.** PRD covers Claude Code end-to-end; the OpenBox hook/ingestion contract is designed tool-agnostic so Cursor ships later as an adapter. Rationale: S1 shows Claude Code is the deeper, non-beta, fail-controllable surface with native finops and hard org mandate.
- **OD9 — DECIDED: fail-closed for deny-class checks.** When a policy call cannot complete, block or downgrade to `ask`. Must be implemented explicitly on Cursor (default there is fail-open). Creates a hard dependency on Core availability/latency → **spike S2 is now on the critical path for the enforcement phase.**

### Open decisions (owners, not decided here)

- **OD4 (owner: brian):** Privacy boundary default — metadata-only (tokens, tool names, hashes) vs content capture, and whether org-configurable. Gate on spike S4. Note: on Cursor, prompt/output content is available ONLY via client-side hooks (no server content logs).
- **OD6 (owner: brian, technical):** Hook handler type — `http` direct to Core (Claude Code native) vs `command`→local OpenBox daemon/OPA sidecar. Gate on S2.
- **OD7 (owner: brian, product):** Phase-1 telemetry content boundary (ties to OD4).
