# Spike S4 — Default privacy/content boundary for session capture

**Question (one sentence):** What session data should leave the developer's machine by default, and what should be opt-in?

**Status:** DONE (2026-07-07) — evidence only; informs decision **OD4** (owner: brian, product/security), does not make it.
**Method:** Read S1 + architecture.md (INV-2, NFR-1); primary-source Claude Code + Cursor docs; two sweeps (dev-governance/DLP/observability defaults; GDPR/trade-secret/monitoring/secrets).
**Convention:** [E]=cited fact, [A]=reasonable application not stated by source.

---

## Design already assumes a boundary; S4 justifies *which*

- INV-2 [E, architecture.md]: absent org config, no prompt/output/file **content** persisted — metadata only; ingestion strips content when disabled.
- NFR-1/R1 [E]: content boundary explicitly gated on S4; blocks freezing the developer event schema + content fields.
- S1 A4 [E]: Claude Code content flags are opt-in; boundary configurable at source.

## 1. Data-class sensitivity (M = needed for a Phase-1 metadata goal; C = only for content inspection, not a Phase-1 goal)

**METADATA (default-egress candidates):** session id (M, opaque), developer/agent DID (M, **personal data under GDPR**), timestamps (M; activity-monitoring signal in aggregate), tool NAMES (M; but see seam below), MCP server names (M), model name (M), token counts (M, finops core), cost (M, finops core), commit hashes (M, lineage core), allow/deny decision (M). → **Every Phase-1 goal is met by metadata alone.** [E: Claude Code emits token.usage/cost.usage/commit.count/code_edit_tool.decision + session.id/user.email with content flags OFF — code.claude.com/docs/en/monitoring-usage.md]

**Two caveats:** developer DID/email is personal data (§3); **tool NAMES vs tool DETAILS is the exact seam** — Claude Code puts some MCP/tool/skill *names* behind `OTEL_LOG_TOOL_DETAILS`, so "tool names as metadata" is a deliberate mapping choice, not free.

**CONTENT (default OFF, opt-in only) — all High sensitivity, all "C":** prompt text; bash commands; file paths (moderate); file contents written; MCP call arguments; command stdout; file contents read; generated code. → **No content class is required for any Phase-1 goal; every one is a plausible carrier of secrets/PII/trade-secret source.** This is the empirical basis for content-OFF default.

## 2. Comparable-tool defaults (cited)

**Tools we integrate:**
- **Claude Code — content OFF by default** [E]: `OTEL_LOG_USER_PROMPTS` (redacted by default), `OTEL_LOG_TOOL_DETAILS` (bash/MCP/tool names + input, disabled), `OTEL_LOG_TOOL_CONTENT` (disabled, 60 KB trunc, tracing beta), `OTEL_LOG_ASSISTANT_RESPONSES` (disabled). Metrics/metadata captured regardless. code.claude.com/docs/en/monitoring-usage.md
- **Cursor — content NOT logged, advises against it** [E]: "We do not log agent responses or generated code content"; hooks guidance: "Be careful logging actual code or prompts… Log metadata (who, when, what file) rather than content when possible." cursor.com/docs/enterprise/compliance-and-monitoring

**Landscape:** GitHub Copilot Enterprise = content OFF (audit log excludes prompts; Metrics API aggregate only) [docs.github.com]. Datadog LLM Obs = content ON, per-span strip + Sensitive Data Scanner [docs.datadoghq.com]. Portkey = ON, `debug:false`→metadata-only [portkey.ai]. LiteLLM = ON, `turn_off_message_logging:True` [docs.litellm.ai].

**Pattern:** *governance*-positioned tools (Copilot + both tools we integrate) default **metadata-only, content OFF**; *observability*-positioned tools default content ON **but ship first-class switches** that reduce to the same metadata-only set our Phase-1 goals need. Metadata-only is a well-understood, industry-supported mode.

## 3. Regulatory notes (cited)

- Prompts/code/tool-I/O = personal-data processing by default; DID/email is personal data (online identifier). [GDPR Art.4 — gdpr-info.eu/art-4-gdpr; ICO]
- Data minimization + purpose limitation (Art.5(1)(b),(c)) → "don't capture content just in case." [gdpr-info.eu/art-5-gdpr]
- **Consent generally NOT valid in employment** (power imbalance); rely on legitimate interest, strictly necessary + proportionate + transparent; continuous ICT monitoring → **DPIA expected**. [WP29 Opinion 2/2017 (EDPB-endorsed); ICO "Monitoring workers" Oct 2023]
- Source code = trade secret; moving it off-machine extends custody chain and can undermine "reasonable steps to keep secret." [EU Dir 2016/943]
- Capturing bash/env/file I/O captures secrets → pipeline becomes a secondary secrets store; redact/scan at source. [OWASP Secrets Mgmt; OWASP MCP Top 10 MCP01:2025]
- [A] metadata-only default is the minimization-aligned, DPIA-friendly baseline; residency (GDPR Ch.V) matters re where the collector lives. **Counsel to confirm** WP29 currency + post-2023 EDPB guidance before this lands in filed policy.

## 4. Guardrail API reuse (repo-grounded)

OpenBox already ships a Guardrail API (PII model, NSFW model, regex, Qwen3-8B) [E, brief.md]; brief explicitly rejects building custom guardrail models. Claude Code hooks support `updatedInput`/`updatedToolOutput` **redaction**; Cursor `afterMCPExecution` redacts output pre-context [E, S1 A1]. → **Any content path (options b/c/d) should reuse the Guardrail API, not build new** (matches reuse-first principle). Placement sub-decision: (i) **at source** (hook redacts before egress — best minimization, but latency tension with NFR-2 <50 ms / INV-3 observe-never-blocks → run async) vs (ii) **at ingestion** (simpler, but raw content already left machine — weaker posture).

## 5. OD4 decision options (owner: brian — NOT chosen here)

| Option | Default egress | Governance value | Privacy/reg risk | Adoption |
|---|---|---|---|---|
| **(a) Metadata-only; content strictly opt-in per-org** | Metadata classes only | Meets **all** Phase-1 goals; no content inspection | **Lowest**; matches CC/Cursor/Copilot; DPIA-light (still processes DID) | **Highest** |
| **(b) Metadata + hashed content fingerprints** | Metadata + one-way hashes | Dedup, change-detection, integrity corroboration (fits Merkle) without storing content | Low–moderate; low-entropy hashes reversible → salt/HMAC, treat as pseudonymous | High |
| **(c) Metadata + Guardrail-redacted content** | Metadata + content surviving redaction | **Highest** — content governance (secret/PII/prompt-injection detection) as product capability | **Highest residual**; imperfect redaction, transit exposure, needs DPIA/access/residency/short retention; latency tension | **Lowest** unless clearly opt-in |
| **(d) Per-data-class configurable** | Org picks each class | **Most flexible**; staged rollout | Variable; misconfig risk → needs safe defaults (=a) + config audit | High if defaults safe + changes logged |

**Cross-cutting:** options compose — likely real answer is "(a) default + (d) control plane, with (c) reusing Guardrail for orgs that opt into content." DID is personal data even under (a) → transparency/lawful-basis/retention still required. Retention (NFR-7) + residency are orthogonal knobs under every option. Content paths must honor INV-3 (async, never block).

## Follow-ups
- **OD4 (owner: brian):** pick default posture (a/b/c/d/composite) + Guardrail placement (at-source vs at-ingestion). This spike is the evidence; decision is human.
- **Legal (brian → counsel):** confirm WP29 currency + newer EDPB monitoring guidance; DPIA trigger; collector residency.
- **Non-blocking evidence gaps:** (1) no single global content-off var confirmed for Datadog LLM Obs; (2) confirm exactly which tool/MCP *name* fields survive Claude Code `OTEL_LOG_TOOL_DETAILS`=OFF before finalizing the "tool names = metadata" mapping.

---

**Bottom line:** every Phase-1 goal is met by metadata alone; all content classes are secret/PII/trade-secret carriers not needed for Phase-1; Claude Code, Cursor, and Copilot all default content-OFF; GDPR minimization + invalid-employee-consent + trade-secret law favor a metadata-only baseline; any content path should reuse the existing Guardrail API.
