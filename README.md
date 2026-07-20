# OpenBox Shift-Left

Extend OpenBox governance from the **agent runtime** to the **developer runtime** — governing the agentic coding tools (Claude Code, OpenAI Codex, Cursor) that developers use to produce code, with the same philosophy OpenBox already applies to running agents.

## Why

OpenBox governs agents at runtime via SDK/code binding: an agent registers, receives an `obx_` key + DID, and its runtime events are evaluated against policy. But there is no visibility or governance over **how the code was produced** — which coding agent, which tools/MCP servers, which prompts, at what token cost. Shift-left closes that gap and establishes a trustworthy lineage:

> **dev session → commit → deploy → runtime agent**

answering, for any commit or deploy: *who/what produced it, with which tools and prompts, and at what cost (finops)?*

See the diagrams in [`docs/diagram/`](docs/diagram/).

## Approach

The guiding principle is **maximal reuse**: shift-left onboards the developer runtime onto OpenBox's existing governance pipeline rather than building a parallel one. A developer's coding tool is registered as an OpenBox agent (`kind=developer`, session as a child record), and its session events flow through the **same** `/api/v1/governance/evaluate` endpoint, are stored in the **same** session/event/span tables, and are read through the **same** service that already serves agent-runtime governance.

The architecture is **provider-agnostic at the core, provider-specific only at the edge**:

- A normalized, tool-agnostic **event contract** is the single interface.
- Each coding tool has a thin **adapter** implementing one SPI (`register` / `emit` / `apply` / `capabilities`).
- Adapters declare a **capability profile**; OpenBox degrades gracefully (telemetry push → poll → hook) and shows each provider's honest governance level (Observe / Advisory / Enforce).
- Adding a new tool = one surface spike + one adapter, **zero core change**.

Developers onboard through a single front door — `openbox dev init --provider <tool>` — after which governance is **ambient** (no new day-to-day UI); admins use the existing OpenBox dashboard.

## Install

Two steps: get the `openbox` engine on your PATH, then wire it into your coding tool.

**1. Install the binary** (builds the single static, no-cgo engine from source):

```bash
curl -fsSL https://raw.githubusercontent.com/OpenBox-AI/openbox-shift-left/main/install.sh | bash
```

The [`install.sh`](install.sh) bootstrap checks for a Go 1.23+ toolchain and `git`, clones the source, builds `openbox` from `cli/cmd/openbox`, and installs it to `~/.local/bin` (override with `OPENBOX_INSTALL_DIR`). Building from source is required because the `cli` module uses relative `replace` directives that `go install …@latest` cannot resolve. It does **not** touch your OpenBox account. Tunables: `OPENBOX_INSTALL_DIR`, `OPENBOX_REF`, `OPENBOX_REPO_URL`, `OPENBOX_SRC` (build from an existing checkout instead of cloning).

> Prefer not to pipe to a shell? Clone the repo and run `bash install.sh`, or build directly with `cd cli && go build -o openbox ./cmd/openbox`.

**2. Wire OpenBox into Claude Code** (the interactive step — mints credentials and installs the plugin):

```bash
export OPENBOX_BACKEND_URL=https://<your-openbox-backend>
export OPENBOX_CONTROL_TOKEN=<keycloak-jwt-or-obx_key_…>   # never a flag (INV-1)
openbox dev init --provider claude-code
```

This registers a `developer` agent, stores its `obx_` key + Ed25519 seed in your OS secret store, and materializes the Claude Code plugin into `~/.claude/plugins/openbox-observe` (copying this same engine into its `bin/`). Governance is ambient thereafter. Preview it first with `--dry-run`; confirm the data-plane round-trip with `openbox dev verify`.

### Status: observe-first shipped; enforcement (Phase 2) shipped as opt-in

**Content capture is ON by default** (as of 2026-07-15): a session's **prompt** text is captured onto the emitted event and egressed so governance can act on it. **Opt out** with `content_capture:false` in `~/.config/openbox/dev.json`, or `OPENBOX_CONTENT_CAPTURE=0` — which restores the **metadata-only** projection (tokens, cost, tool/MCP names, session/commit ids, model, decisions).

> ⚠️ **Privacy note:** when capture is on, redaction-at-source (the Guardrail API) is **not yet wired** (`[EXT-guardrail-redaction]`), so **prompt content egresses unredacted**. Opt out if that is not acceptable for your org. Regardless of the toggle, tool **commands**, file **bodies**, and tool **output** are **never** egressed on observe events (SL3-SEC-3, asserted by `TestMap_NoContentLeak`) — only the prompt is gated by content-capture.

Claude Code is the first adapter; Codex and Cursor are fast-follows. Policy **enforcement** (deny/ask/rewrite, fail-closed) is built as opt-in Phase-2 (Epic E6; `OPENBOX_ENFORCE=1`, default observe).

## Provider support

| Provider | Telemetry | Enforce (Phase 2) | Egress proxy | Org mandate |
|---|---|---|---|---|
| **Claude Code** | native OTel push | strong | ✅ base-URL | managed settings |
| **Codex** | native OTel + rollout | strong (beta) | ✅ base-URL | requirements.toml / MDM (best-in-class) |
| **Cursor** | poll (Admin API) + hooks | beta, fail-open | ❌ Agent egress not interceptable | Team hooks |

Two capabilities are provider-independent and always available — OpenBox registration and git-trailer commit-binding — so session→commit→deploy lineage and finops work for any tool.

## Design artifacts

This project uses the **fab7-sdlc** workflow. Durable design lives under [`.fab7/sdlc/`](.fab7/sdlc/):

- `discovery/brief.md` — problem framing, option set, decisions
- `discovery/spikes/` — S1 (Claude Code/Cursor surfaces), S3 (commit→session attribution), S4 (privacy boundary), S5 (Codex surfaces)
- `design/prd.md` — Phase-1 PRD (requirements, NFRs, epics)
- `design/architecture.md` — architecture of record, incl. the generic provider-adapter model (§1b)
- `design/readiness-findings.md` — design-readiness verdict

Longer-form narrative docs are in [`docs/`](docs/).

## Status

Design complete and readiness-checked; planning (story slicing) is next. Epic spine: **E1** provider-agnostic core → **E2** `openbox` CLI + Claude Code adapter → **E3** git action; **E5** Codex and **E4** Cursor fast-follow; **E6** Phase-2 enforcement.

## Related repositories

Shift-left reuses and mirrors these OpenBox repos:

- **openbox-backend** — NestJS control plane (agent registry, governance events, dashboard)
- **openbox-core** — Go data plane (`/api/v1/governance/evaluate`, token auth, policy)
- **openbox-temporal-sdk-python** — the agent-runtime SDK whose onboarding philosophy shift-left mirrors for the developer runtime
