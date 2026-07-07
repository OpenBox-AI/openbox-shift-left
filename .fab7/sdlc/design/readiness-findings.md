# Design Readiness Findings — Shift-Left Phase 1 (observe-first)

**Author:** John (Solution Architect) — 2026-07-07
**Evaluates:** `.fab7/sdlc/design/prd.md`, `.fab7/sdlc/design/architecture.md`, discovery `brief.md`, spikes S1/S3/S4/S5.
**Checklist:** `design-readiness-checklist`.
**Verdict: READY TO PLAN** (Phase-1 provider-agnostic core + Claude Code adapter). No Phase-1 build blockers. Named non-blocking follow-ups + clearly-deferred items below.

---

## Checklist results

### Traceability
- ✅ **FR → capability.** All 8 FRs (PRD §2) map to a reuse decision + component in architecture §2/§3 (e.g. FR-2→D2 OTLP path, FR-6→D4 Deploy event + `didFor`/`createHash`). *Recommend running `prd-trace` for a formal FR→test map during planning (non-blocking).*
- ✅ **NFR owner/test.** All 7 NFRs (PRD §3) carry an owner + test strategy.
- ⚠️ **UX → AC/non-goal.** No `ux-spec.md`. Phase-1 UX is thin by design: developer surface = `openbox` CLI + ambient governance (OD12); admin surface = **reused** OpenBox dashboard. **Gap (deferred, named):** the FR-7 lineage/finops **read surface** and the `openbox dev init` onboarding flow are user-facing and have no acceptance criteria yet. → capture as ACs in the relevant epics (or a lightweight UX pass) during planning. Not a blocker.
- ✅ **Architecture decisions constrain + cite.** §3 D1–D7 and §5 guardrails cite repo symbols (agent.service.ts:652, governance.go:28/273, hook_governance.py, SessionEntity/SpanEntity/SessionMerkleLeafEntity) or S1/S5 doc URLs. *Recommend promoting D1 (host reuse), D3 (governance_events reuse), and the OD4 privacy posture to ADRs for durability (non-blocking).*
- ✅ **Deferred work labeled.** PRD §5 + architecture §6 label Phase-2 enforcement, Cursor/Codex adapters (fast-follow), egress proxy, KMS attestation (Phase 3) as outside the first build path.

### Consistency
- ⚠️ **PRD narrower than architecture (doc-sync gap, non-blocking).** The PRD predates architecture §1b: it frames "Claude Code first + tool-agnostic contract; Cursor fast-follow" but does not yet carry the **generic Provider Adapter model**, the **`openbox` CLI** front door (OD12), or **Codex** (S5, now a strong provider). Architecture is authoritative and planning slices from the §1b spine, so this does not block — but **route a `create-prd`/`doc-sync` pass** to add the adapter model + OD12 + Codex so PRD and architecture agree.
- ✅ **Sources cited.** Every major requirement/decision cites discovery/spike/repo evidence inline.
- ✅ **External dependencies explicit + owned.** Claude Code OTel/hooks (pin v2.1.200+, R6), Cursor Admin API + beta hooks (S1), Codex `features.hooks` beta + Usage/Cost API (S5, pin version), git (S3). Owner: brian.

### Human-decision items (never inferred)
- ✅ All Phase-1-blocking decisions resolved: OD1 (observe-first), OD3/OD11 (hybrid identity), OD4 (metadata-only + Guardrail at-source), OD5/OD8 (Claude Code first), OD9 (fail-closed, Phase-2), OD10-posture (opt-in), OD12 (`openbox` CLI), OD-A/B/C (reuse). Recorded with owners + dates in the design docs.
- ✅ Remaining open decisions are explicit gates with owners, none Phase-1-build-blocking (see below).

---

## Gap classification

**Planning blockers (must resolve before planning): NONE** for the Phase-1 provider-agnostic core + Claude Code adapter.

**Deferred / plannable-around (named):**
1. **OD6 — Phase-2 enforcement hook handler type** (http vs command→local sidecar). Owner brian. Gated on **spike S2** (policy-eval latency). Blocks only the Phase-2 enforcement epic, not Phase-1.
2. **OD10 — name the pilot team/repo.** Owner brian. Needed before **pilot/rollout** stories and M1/M5 measurement (and to validate S3 U-1 GitHub-squash prevalence). Core + adapter build can be sliced and built without it.
3. **Legal review** — WP29/EDPB currency, DPIA trigger, collector data residency (S4). Owner brian → counsel. A **launch/compliance gate**, not a build blocker.
4. **UX acceptance criteria** for FR-7 read surface + `openbox dev init` flow — capture during epic/story authoring (or a light `create-ux-spec`).
5. **PRD doc-sync** to match architecture §1b (generic adapter model, OD12, Codex) — route to `create-prd`/`doc-sync`.
6. **ADRs (optional)** for D1/D3/OD4 durability.

---

## Recommended epic-slicing spine (for `create-epic`)

From the architecture §1b reuse mirror, a clean split:
- **E1 — Provider-agnostic core:** normalized developer event types (additive to `isValidGovernanceEventType`, INV-8 conformance test), `kind=developer` registration + session-as-child, ingestion onto `/api/v1/governance/evaluate`, lineage read (FR-7) via `GovernanceEventService`, OTLP intake reuse.
- **E2 — `openbox` CLI + Claude Code adapter:** `openbox dev init --provider claude-code` (OD12), CC plugin (hooks + managed settings + OTel config), metadata-only emit, `prepare-commit-msg` trailer (S3), managed-settings mandate support (verify, pilot opt-in).
- **E3 — OpenBox git action:** read `OpenBox-Session` trailer at push, resolve server-side vs real pushed SHA (S3), register Deploy DID, Merkle-anchor.
- **E4 (fast-follow) — Cursor adapter; E5 (fast-follow) — Codex adapter.** Each = adapter + its surface spike already done (S1/S5).
- **E6 (Phase 2, blocked on S2) — enforcement:** flip adapters to honor verdicts, fail-closed.
