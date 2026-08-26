# Competitive landscape — governance for agentic coding tools

Date: 2026-08-26. Branch: `feat/tool-content-capture` (hook governance + ADR-0021 gateway).
Method: web research only. No vendor was run; maturity claims are the vendors' own unless noted.

## Verdict

**Yes — the category exists and is crowded.** No single project matches the whole
stack, but every individual capability has at least one direct competitor, and two
products match large contiguous slices of it:

- **Endor Labs Agent Governance** — near feature-for-feature on the HOOK half, with
  broader provider coverage than this repo. GA, funded, MDM scripts public.
- **Proofpane** — near match on the GATEWAY + signed-evidence half, including the
  same local-daemon-single-binary shape. Real, priced, pre-first-customer.

What is not matched anywhere: **shift-left onto an existing agent-runtime
governance pipeline** (same `/evaluate`, same tables, same DID/AIP signing as the
production agent runtime) and **session → commit → deploy lineage joined and
queryable**. That is the moat, and it is not a feature — it is the topology.

## Tier 1 — closest overall

### Endor Labs — Coding Agent Governance / "AI Audit"

The closest thing to this repo's hook engine that exists as a shipping product.

| Their design | This repo |
|---|---|
| single binary `endorctl ai-audit` wired as handler for every hook event | `openbox` engine, same pattern |
| server-side policy eval; **Block / Alert / Ask Permission** | `/evaluate` only decider; deny / findings / approval (ADR-0017) |
| local redactor masks "secret-named keys and known credential formats" before egress, and states plainly that "a secret in an unknown format can still reach the backend" | keyword-driven `decision/secrets.go` with the identical documented limit (C39) |
| MDM + managed settings, in-browser generator, public `endorlabs/mdm-scripts` | `deploy/managed/`, `--scope global` |
| 29 default policies; shell allow/deny, sensitive-file patterns, MCP allowlists, env-var rules | org policy, no shipped template packs (on the roadmap) |
| Claude Code (app+CLI), Claude Cloud, **Cursor (IDE/Cloud/CLI)**, Codex, **Copilot (CLI + VS Code agent mode)** | Claude Code, Codex. Cursor not built, Copilot not planned |
| macOS / Linux / Windows | same, Windows build-verified only |

Two hard divergences, both in this repo's favour or against it depending on buyer:

- **"Prompt text stays local."** Endor deliberately does not egress prompts. This
  repo egresses prompts, tool I/O, assistant text and extended thinking by default
  (ADR-0019). Strictly more evidence, strictly more privacy surface — and it is the
  axis a security buyer will interrogate first.
- **No model-call capture, no token/cost capture, no attestation/lineage** in their
  product. Those are three of this repo's five differentiators.

Positioning note: they market into ISO 42001 compliance burden. This repo's docs
argue assurance from first principles and refuse to overstate — a real asset, but
the compliance-framework mapping is a sales surface they occupy and this repo does not.

### Proofpane

Same architectural bet as ADR-0021, arrived at independently:

- runtime governance gateway for **Claude Code, Cursor, Codex** (plus Claude Desktop,
  MCP clients, n8n/UiPath/Zapier/Make/Power Automate, OpenAI-compatible agents)
- **allow / deny / human-in-the-loop enforced in the execution path**
- DLP redaction before the model sees a secret
- hash-chained audit exporting an **offline-verifiable Ed25519-signed Evidence Pack**
- mapped to NIST AI RMF, ISO 42001, EU AI Act, GDPR, SOC 2
- **local daemon, 14 MB single binary**, integrates by official hooks, MCP, *or* egress
  gateway — "zero workflow change"
- NZ$15,000/yr / 50 seats founding rate; macOS signed+notarized, Windows unsigned;
  SOC 2, pentest and case studies stage-gated on a first enterprise customer that
  does not exist yet

Read that list against ADR-0021 §§1–7. Local-first daemon, refusal in path, redact
the captured copy, signed offline-verifiable evidence, hooks *and* gateway in one
product. The differences that remain are the pipeline topology, per-turn finops,
and lineage.

## Tier 2 — direct competitors on one slice

**Gateway + cryptographic ledger**
- **KYDE Gateway** — drop-in OpenAI-compatible proxy (OpenAI, Anthropic, Gemini,
  Copilot, local); every agent action into an Ed25519-signed hash-chained ledger;
  DLP + per-MCP-tool policy before upstream; BSL-1.1 source-available. *Only found via
  curated lists — repo not independently located; treat maturity as unverified.*
- **Coder AI Governance Add-On** — LLM gateway auditing prompts/tokens/tool
  invocations, central MCP administration, **process-level agent firewall** limiting
  reachable domains. That firewall is the MDM-tier prevention ADR-0021 §2 defers to an
  org's MDM; Coder ships it.
- **Bifrost, Portkey, LiteLLM, TrueFoundry, Speakeasy, MintMCP** — general AI
  gateways with guardrails/RBAC/budget/audit, all documented for Claude Code via
  `ANTHROPIC_BASE_URL`. Not coding-agent governance, but they own the transport this
  repo's gateway now also occupies, and they got there first.

**Cryptographic evidence — goes further than this repo**
- **Provenrail** — hash-chained Ed25519 records of tool calls, model calls *and
  guardrail decisions*, off-box sink, verifiable without trusting agent or vendor.
  **Two independent verifiers** (Python CLI + in-browser JS) held in lockstep by a
  frozen conformance-vector suite; RFC 3161 timestamps; transparency-log inclusion
  proofs with witness cosignatures; OpenTimestamps Bitcoin anchors. MIT client/SDK/
  verifier/spec, AGPL server. `pip install provenrail`.
  **Provenrail Guard** adds the PreToolUse plugin: deny destructive commands and leaked
  credentials, escalate low-confidence cases to a recorded human prompt, sign every
  allow/deny/approval.
  This is a higher assurance bar than Merkle leaves in core. Worth reading before the
  next attestation change.
- **Microsoft Agent Governance Toolkit** — W3C DID identity verification + Merkle-chained
  audit logs + deterministic policy enforcement. Same primitives (DID + Merkle), aimed at
  agent frameworks not coding agents.
- Smaller, same niche: **HELM AI Kernel** (signed ALLOW/DENY/ESCALATE EvidencePacks),
  **Nobulex** (bilateral pre/post receipts), **Clay Seal**, **world-model-mcp**
  (Ed25519 + Merkle, post-quantum hybrid signing).

**Hook-based gating (OSS/solo)**
- **AxonFlow**, **Prehook.ai** (CC only; SessionStart/PreToolUse/PostToolUse/SessionEnd,
  synchronous deny, 17 rules, SaaS dashboard), **Baseline**, **systemprompt-template**
  (self-hosted Rust binary, auth+audit+policy+cost), **Chock** (governance-as-code
  compiled to pre-tool-use hooks, CC+Cursor, per-agent coverage reports), **ThumbGate**,
  **AgentLock**, **MARGINAL**, **Agentlas OS**, **agent-approval-gate** (blocked call
  becomes a queued approval ticket), **Penholder**.

**Per-turn finops / AI attribution**
- **aGiTrack** — wraps Claude Code / Codex / OpenCode, commits each agent turn to git
  recording prompt, backend, model, and **input/output/cache-read/cache-write token
  counts** in the commit message, sub-agents counted separately. This is ADR-0014's
  four counts plus the git trailer idea, shipped, cross-platform incl. native Windows.
- **ccusage**, **TokenTracker** (31 tools, "never reads prompts"), **Git AI**,
  **Exceeds.ai** (line-level AI attribution), **Crash Override** (commit metadata
  conventions, GPG/Sigstore signing, CI gates on agent-commit metadata), **DX**, **Faros AI**.

**Enterprise DLP for code assistants**
- **Prompt Security (now SentinelOne)**, **Witness AI**, **Aurascape**, **Knostic**,
  **OX Security**. All pitch inspect/redact/block on assistant traffic; the market
  language has moved past binary block to "warn, redact, reroute to an approved model,
  or block" — a policy vocabulary richer than allow/deny/ask.

## Finding that bears on the current branch

**ADR-0021's capture premise is now only partly true, and the doc should say so.**

The ADR states hooks cannot see the model call, so "a gateway is the surface where
[headers and bodies] exist." Claude Code's own OTel export now emits:

- `claude_code.api_request_body` — Messages API request JSON **including system prompt,
  messages and tools**
- `claude_code.api_response_body` — response JSON incl. content blocks, usage, stop reason
- gated by `OTEL_LOG_RAW_API_BODIES` (`1` = inline, truncated at 60 KB; `file:<dir>` =
  untruncated on disk with a `body_ref` pointer)
- plus `OTEL_LOG_USER_PROMPTS`, `OTEL_LOG_ASSISTANT_RESPONSES`, `OTEL_LOG_TOOL_DETAILS`,
  `OTEL_LOG_TOOL_CONTENT`, `claude_code.tool_decision`, and a traces beta linking
  prompt → API request → tool execution
- lockable fleet-wide through managed settings

The ADR's literal sentence — uncapturable *from hooks* — survives. The product framing
does not: **request and response bodies are reachable first-party today, without a
gateway.** What the gateway still uniquely reaches:

1. **request headers** (not logged by OTel)
2. **the credential fingerprint / account binding** (§6) — nothing else sees it
3. **synchronous refusal of the inference** (§7) — OTel is an exporter, not a gate
4. **extended thinking** — OTel redacts it from bodies *unconditionally*; this repo
   captures it from the transcript (ADR-0019 P3), so the full-capture claim relative to
   first-party telemetry still holds, and is now the sharpest version of that claim

Also: OTel is developer-redirectable unless managed settings lock the collector, and
carries no signing or Merkle chain — so §2's tamper-evidence tiering remains a real
distinction, not a restatement.

Consequence: the honest gateway pitch is **enforcement + account binding + signed
evidence**, not capture. Capture is now a commodity on this provider.

## Where this repo is behind

1. **Provider coverage.** Endor: 5 surfaces incl. Cursor and Copilot. Proofpane: 3
   coding agents + desktop + 5 automation platforms. This repo: 2, Cursor unbuilt.
   Provider breadth is the first thing a buyer counts.
2. **Verifiability depth.** Provenrail's two-verifier + conformance-vector + RFC 3161 +
   transparency-log + Bitcoin-anchor stack is a higher bar than Merkle leaves.
3. **Compliance mapping.** Competitors lead with ISO 42001 / EU AI Act / NIST AI RMF /
   SOC 2 mappings. This repo has better-grounded assurance claims and no mapping.
4. **Nothing has run against a real stack** (per `CLAUDE.md`), while Endor is GA.
   Proofpane is equally pre-customer, so the gateway race is open; the hook race is not.
5. **Policy vocabulary.** The DLP vendors offer reroute-to-approved-model and warn tiers
   this repo does not have.

## What stays genuinely distinctive

1. **One pipeline for runtime and dev-time.** Same `/evaluate`, same `sessions` →
   `governance_events`, same DID/AIP signing as the production agent runtime. Every
   competitor is dev-time-only (Endor, Proofpane, Provenrail) or runtime-only (Databricks
   Unity Catalog + AI Gateway, Microsoft AGT). Nobody joins them. **Lead with this.**
2. **session → commit → deploy lineage, joined and queryable, with a signed commit
   attestation.** The attribution cluster tags commits; supply-chain tools attest builds;
   nobody chains agent session → commit → deploy.
3. **Approval hold + rewake, plus a bounded autonomous approver.** Competitors queue a
   ticket or block; waking a paused live session on a late verdict is rarer.
4. **Full content capture incl. extended thinking, on by default** — beyond first-party
   OTel and beyond Endor's deliberate prompt-locality. Differentiator and liability.
5. **Honest assurance framing.** `TestReportNeverClaimsPrevention` as a merge gate is a
   posture no competitor takes, and it is defensible against every "cannot bypass" claim
   in this market.

## Unresolved questions

1. Does the owner want to keep pitching gateway *capture* now that
   `OTEL_LOG_RAW_API_BODIES` exists — or repitch the gateway on refusal + account binding
   alone? ADR-0021 §Context #1 needs an amendment either way.
2. Is Cursor still the right next adapter, given Endor and Proofpane both already cover
   it and neither covers this repo's lineage story? Copilot is the larger unserved seat count.
3. Does the full-content-capture default survive contact with a security buyer who has
   seen Endor's "prompt text stays local"? Worth testing before the Cursor adapter.
4. Is a compliance-framework mapping (ISO 42001 / EU AI Act) worth writing, given every
   Tier-1 competitor leads with one?
5. Should Provenrail's verifier-conformance approach inform the attestation roadmap —
   independent verification without trusting the vendor is a claim this repo cannot
   currently make.
