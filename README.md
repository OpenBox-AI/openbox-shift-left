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

| Provider | Adapter | Telemetry | Enforce (opt-in) | Telemetry base-URL | Org mandate |
|---|---|---|---|---|---|
| **Claude Code** | shipped | hooks + durable spool | deny / ask | ✅ configurable | managed settings |
| **Codex** | shipped | hooks + durable spool | deny (no `ask` surface) | ✅ configurable | requirements.toml / MDM |
| **Cursor** | not built (SL-8) | hooks available since v3.11 | — | — | Team hooks |

Two capabilities are provider-independent and always available — OpenBox registration and git-trailer commit-binding — so session → commit → deploy lineage and finops work for any tool.

**What the columns do and don't claim.** *Telemetry base-URL* is where OpenBox sends **its own** governance events; it is not egress control over the coding tool. OpenBox does not proxy, intercept, or allow-list the tool's traffic to its model provider — that is the provider's own plane (Claude Code sandbox network allow-lists, Codex network policy) plus your enterprise network controls. OpenBox's job is to **record** that posture as evidence, not to enforce it. *Enforce* is opt-in per install and, until the managed provider config is deployed, is enforced by a user-local hook the developer can remove — prevention without assurance. See the assurance note below.

## Assurance — what the evidence proves today

Being precise about this is part of the product; a governance tool that overstates its own
guarantees is the failure mode it exists to prevent.

- **Commit attribution is an *inferred* claim.** The `OpenBox-Session` git trailer records which
  session was live when a commit was made. It is not proof that the session produced the diff, and
  a trailer can be hand-written. Server-side ownership verification upgrades a claim to
  `attributed`; cryptographic `verified` provenance is E8-S10.
- **Enforcement assurance depends on managed deployment.** The enforce gate runs as a hook in the
  developer's own config. Until the provider's managed configuration is deployed
  (`allowManagedHooksOnly` for Claude Code, `allow_managed_hooks_only` for Codex — E8-S8/S9), a
  developer can remove the hook or flip the local setting, so local enforcement prevents mistakes
  but does not withstand a motivated bypass. **For Codex the hook itself is not yet mandated**: a
  `requirements.toml` cannot define a hook, so the shipped mandate pins approval and sandbox modes
  while the hook stays user-level and removable — see `deploy/managed/README.md` § Remaining gap.
- **Egress is recorded, not controlled** — see the note under the provider table.
- **Policy integrity**: the local policy bundle is currently unsigned, so a user-local edit is not
  yet detectable end-to-end (E8-S6 adds signing, expiry, and rollback protection).

Each session's *effective* posture — enforce on/off, fail-open/closed, bundle state, content
capture, staleness — is emitted as evidence on session start, so the control plane can tell the
tiers apart without trusting the endpoint's word for it.

## License

[Apache License 2.0](LICENSE).
