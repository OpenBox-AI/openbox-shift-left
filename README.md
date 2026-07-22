# OpenBox Shift-Left

Extend OpenBox governance from the **agent runtime** to the **developer runtime** — governing the agentic coding tools (Claude Code, OpenAI Codex, Cursor) that developers use to produce code, with the same philosophy OpenBox already applies to running agents.

## Why

OpenBox governs agents at runtime via SDK/code binding: an agent registers, receives an `obx_` key + DID, and its runtime events are evaluated against policy. But there is no visibility or governance over **how the code was produced** — which coding agent, which tools/MCP servers, which prompts, at what token cost. Shift-left closes that gap and establishes a trustworthy lineage:

> **dev session → commit → deploy → runtime agent**

answering, for any commit or deploy: *who/what produced it, with which tools and prompts, and at what cost (finops)?*

## Approach

The guiding principle is **maximal reuse**: shift-left onboards the developer runtime onto OpenBox's existing governance pipeline rather than building a parallel one. A developer's coding tool is registered as an OpenBox agent (`kind=developer`, session as a child record), and its session events flow through the **same** `/api/v1/governance/evaluate` endpoint, are stored in the **same** session/event/span tables, and are read through the **same** service that already serves agent-runtime governance.

The architecture is **provider-agnostic at the core, provider-specific only at the edge**:

- A normalized, tool-agnostic **event contract** is the single interface.
- Each coding tool has a thin **adapter** implementing one SPI (`register` / `emit` / `apply` / `capabilities`).
- Adapters declare a **capability profile**; OpenBox degrades gracefully (telemetry push → poll → hook) and shows each provider's honest governance level (Observe / Advisory / Enforce).
- Adding a new tool = one surface spike + one adapter, **zero core change**.

Developers onboard through a single front door — `openbox dev init --provider <tool>` — after which governance is **ambient** (no new day-to-day UI); admins use the existing OpenBox dashboard.

## Install

```bash
curl -fsSL https://raw.githubusercontent.com/OpenBox-AI/openbox-shift-left/main/install.sh | bash
```

This drops the prebuilt `openbox` engine on your PATH. Then onboard with a single command — see **[QUICKSTART.md](QUICKSTART.md)** for the full walkthrough (onboarding, enforce vs. observe, privacy).

## Provider support

| Provider | Telemetry | Enforce | Egress proxy | Org mandate |
|---|---|---|---|---|
| **Claude Code** | native OTel push | strong | ✅ base-URL | managed settings |
| **Codex** | native OTel + rollout | strong (beta) | ✅ base-URL | requirements.toml / MDM |
| **Cursor** | poll (Admin API) + hooks | beta, fail-open | ❌ Agent egress not interceptable | Team hooks |

Two capabilities are provider-independent and always available — OpenBox registration and git-trailer commit-binding — so session → commit → deploy lineage and finops work for any tool.

## License

[Apache License 2.0](LICENSE).
