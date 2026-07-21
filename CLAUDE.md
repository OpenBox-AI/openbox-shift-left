# CLAUDE.md

Guidance for Claude Code (and other agents) working in the **openbox-shift-left** repo.

## What this repo is

A **design/specification** repo (no application code yet) for extending OpenBox governance from the agent runtime to the **developer runtime** — the agentic coding tools (Claude Code, OpenAI Codex, Cursor) developers use. See `README.md` for the vision and `docs/diagram/` for the diagrams.

## Core principle: reuse, don't rebuild

Shift-left onboards the developer runtime onto OpenBox's **existing** agent-runtime governance pipeline. Mirror the `openbox-temporal-sdk-python` SDK philosophy (`create_openbox_worker()`) for CLIs/IDEs:

- Register the developer/tool-install as an OpenBox agent (`kind=developer`), session as a child record — reuse `POST agent/create` → `obx_` key + DID.
- Emit session events through the **same** endpoint `/api/v1/governance/evaluate` (openbox-core) with the **same** auth (`Bearer obx_` + AIP signing).
- Store in the **same** tables (`SessionEntity` → `GovernanceEventEntity` → `SpanEntity`; `SessionMerkleLeafEntity` for tamper-evidence) and read via the **same** `GovernanceEventService`.

**Rule:** prefer reusing an existing table/endpoint/service over introducing a new one. A new table/endpoint/service requires an ADR.

## Architecture in one line

Provider-agnostic core + one thin adapter per tool (SPI: `register` / `emit` / `apply` / `capabilities`) behind a normalized event contract. Adding a provider = one surface spike + one adapter, zero core change. Details in `.fab7/sdlc/design/architecture.md` (§1b is the generic adapter model).

## Where things live (fab7-sdlc workflow)

- `.fab7/sdlc/discovery/` — brief + research spikes (S1–S5)
- `.fab7/sdlc/design/` — PRD, architecture, readiness findings
- `.fab7/sdlc/assignments/` — Worker Result Contracts per command
- `docs/` — longer-form narrative docs and diagrams

`.claude/` is local tooling (the fab7-sdlc skill install) and is **git-ignored** — do not commit it.

## Working conventions

- This is a governance product: treat privacy and security as first-class. **Content capture is ON by default as of 2026-07-15** (brian; reverses OD4's original metadata-only-by-default posture) — prompt content is captured and egressed unless an org opts OUT (`content_capture:false` or `OPENBOX_CONTENT_CAPTURE=0`). When on, content is meant to be Guardrail-redacted at source, but that layer is currently inert (`[EXT-guardrail-redaction]`), so prompt content egresses **unredacted**; tool commands and file bodies still never egress on observe events (SL3-SEC-3), and Tier-1 local secret detection (E6-S9) only redacts Write/Edit bodies in enforce mode. See PRD NFR-1 / architecture INV-2.
- Decisions only a human can make (scope, privacy posture, priority) are recorded as `OD*` in the design docs — never infer them; surface them.
- Keep design docs source-cited: cite the repo symbol/path or spike/doc URL behind each claim.
- Sibling repos (reuse targets): **openbox-backend** (NestJS control plane), **openbox-core** (Go data plane), **openbox-temporal-sdk-python** (agent-runtime SDK).

## Status / next step

**Phase-1 (observe) + Phase-2 (enforce) + SDK-unification are SHIPPED** (as of 2026-07-15, all committed to `main`; sibling `openbox-core` work on branch `feat/ext-core-dev-runtime-event-types`):
- **Phase-1** SL-1…SL-16 (+ WIRE/FILEBACKEND) — observe-first, dev-init onboarding, commit→deploy lineage, Advisory tier — DONE.
- **E6 enforcement** (S1–S11) — local decision evaluator, obtain→apply verdict cascade, fail-open/closed policy, redaction apply, REQUIRE_APPROVAL→ask, conformance, native policy evaluator + bundle sync (ADR-0005), Tier-1 secret redaction, Tier-2 sync escalation, Tier-3 findings loop. Enforce is **opt-in** — enable at onboarding via `openbox dev init … --enforce` (persisted to `dev.json`; default observe). Spike S2 / OD6 / OD9 resolved; ADR-0002/0003 (INV-3b carve-out, sidecar module).
- **Onboarding simplification** (2026-07-21/22, ADR-0006, `decision.Decider`/`InProcessDecider`) — the PreToolUse enforce gate decides **in-process**. The socket sidecar is **removed entirely** (brian: "no socket, no sidecar at all"): no `Client`, no listener, no `openbox sidecar serve` command, no `OPENBOX_SIDECAR_SOCKET`; the module was renamed `sidecar/` → `decision/`. `dev init --enforce` persists enforce+tier2+findings to `dev.json` so **no runtime env var is needed**, and `install.sh` downloads a **prebuilt** binary (GoReleaser + `.github/workflows/release.yml`) with a source-build fallback. Net: `curl|bash` → `dev init --enforce` → ambient. See `QUICKSTART.md`.
- **E7 SDK unification** (S0–S8) — dev telemetry re-expressed onto the base SDK (`openbox_core`) Activity/hook + flat `SpanData` wire model; core classifier extended; EXT-core accept-list retired. Per **ADR-0004** (amended for the pivot): the base-SDK `shell`/`mcp`/`tool` hook types are carried as a **Go mirror** in shift-left (`client/hookspan.go`) because upstreaming to `openbox-sdk-python` is push-blocked.

**Next:** SL-7 (Codex) / SL-8 (Cursor) adapters (deferred fast-follows — the next feature increment). Open follow-ups: **push a `vX.Y.Z` tag so the release workflow publishes the first prebuilt assets** (until then `install.sh` uses its source-build fallback); open the `openbox-core` PR; upstream the `shell`/`mcp`/`tool` hook types to `openbox-sdk-python` (needs push access) to complete true unification and retire the Go mirror; full server-side Guardrail redaction (`[EXT-guardrail-redaction]`) for the content-ON posture. See `.fab7/sdlc/retros/E6-E7.md` and the ledger `.fab7/sdlc/status.yaml`.

> **Doc note:** the design docs (`prd.md`, `architecture.md`) scope themselves to "Phase 1 observe-first"; that is their **historical** framing — Phase-2/E6/E7 shipped later (dated supersessions recorded inline, e.g. INV-2/OD4/NFR-1). Treat the ledger + this section as the live status.
