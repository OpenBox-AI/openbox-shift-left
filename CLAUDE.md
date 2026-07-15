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

Design is readiness-checked and ready to plan. Next: slice epics **E1 → E2 → E3** into worker-ready stories (E5 Codex / E4 Cursor fast-follow; E6 Phase-2 enforcement blocked on spike S2).
