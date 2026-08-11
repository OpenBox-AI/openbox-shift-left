# CLAUDE.md

Guidance for Claude Code (and other agents) working in the **openbox-shift-left**
repo. User-facing documentation lives in `README.md` and `docs/`; this file is only
the working context an agent needs.

## What this repo is

The developer-runtime half of OpenBox governance: one static Go binary that governs
the agentic coding tools (Claude Code, Codex) developers use, feeding the same
pipeline the agent runtime already uses. `README.md` for the product,
`docs/architecture.md` for the shape.

## Core principle: reuse, don't rebuild

Shift-left onboards the developer runtime onto OpenBox's **existing** pipeline rather
than a parallel one:

- register the tool install as an OpenBox agent (`kind=developer`), session as a
  child record — `POST agent/create` → runtime key + DID;
- emit events through the **same** `/api/v1/governance/evaluate` with the **same**
  auth (`Bearer obx_` + AIP signing);
- store in the **same** tables (`sessions` → `governance_events`, plus Merkle
  leaves) and read through the **same** services. Dev sessions write no `spans`
  rows — see ADR-0013.

**Rule:** prefer reusing an existing table/endpoint/service over adding one. A new
table, endpoint or service requires an ADR in `docs/adr/`.

## Architecture in one line

Provider-agnostic engine + one thin adapter per tool behind a normalized event
contract. Adding a provider is an adapter, not an engine change.

The SPI (`provider/`) is `Installer` (install time) + `HookEngine` (runtime +
capabilities). The engine is `adapters/common/hookflow`: spool, duration stash,
advisory sink, findings loop, staleness gate, the enforce cascade and its gate
sequence, Tier-2 escalation, approval hold, rewake. An adapter is four things — its
native hook shape, its mapper, an `OutputContract`, its installer.

**The engine used to be copy-pasted per adapter** (~85% of non-test adapter code was
a rename-level fork, on the enforcement path) and the copies drifted. Do not
reintroduce that: if something is provider-agnostic it goes in `hookflow` or
`devconfig`.

## Where things live

| Path | What |
|---|---|
| `provider/` | the SPI |
| `adapters/common/hookflow/` | the engine every adapter runs on |
| `adapters/common/devconfig/`, `adapters/common/git/` | shared config/posture; trailer, notes, attestation |
| `adapters/claude-code/`, `adapters/codex/` | one thin adapter each |
| `client/` | core client: payload, AIP signing, verdicts |
| `decision/` | in-process enforcement: bundle, evaluator, secret detection, redaction |
| `cli/` | the `openbox` CLI, incl. `cli/internal/approver` (ADR-0012) |
| `actions/openbox-git-action/` | commit→deploy lineage for CI |
| `contracts/dev-event/` | event schema, wire mapping, conformance |
| `testbed/` | the mock-free end-to-end suite (`docs/testbed/e2e.md`) |
| `docs/` | user documentation — keep it true, and keep it short |

`.claude/` and `.fab7/` are local tooling and git-ignored — do not commit them.

## Working conventions

- **Privacy and security are first-class.** Content capture is **ON by default**
  (2026-07-15, reversing the original metadata-only posture): prompt text egresses
  unless an org opts out (`content_capture:false` / `OPENBOX_CONTENT_CAPTURE=0`).
  Usage capture is **also ON by default** (2026-08-11, ADR-0014): four token counts
  plus a model id per turn, opt out with `finops:false` / `OPENBOX_FINOPS=0`.
  Guardrail redaction at source is **not wired yet**, so prompt text egresses
  unredacted. Tool commands and file bodies never egress on observe events; an
  approval escalation is the one exception and is content-gated. Tier-1 secret
  detection redacts Write/Edit bodies locally, in enforce mode. Keep
  `docs/data-and-privacy.md` true.
- **`usage.go`'s INV-2 guarantee is an allowlist now, not an impossibility.** It
  used to hold structurally — the transcript projection bound only numeric fields,
  so content had nowhere to land. Binding `message.model` (required: the model id is
  the backend's aggregation key) replaced that with a curated allowlist enforced by
  the sentinel test. **That test is load-bearing.** A change that makes it pass
  trivially is a defect, and a second bound string needs an ADR amendment, not a
  commit.
- **Decisions only a human can make** (scope, privacy posture, priority) are `OD*`
  decisions: surface them, never infer them.
- **Cite sources in docs** — the repo symbol/path or upstream doc URL behind each
  claim. A governance product that overstates itself is the failure it exists to
  prevent, so prefer an honest limit over a confident sentence.
- **Verify against the real thing.** `testbed/run-all.sh` drives real headless
  sessions against a real local stack and asserts what arrived; unit tests are not
  evidence that a hook works.
- Sibling repos: **openbox-backend** (NestJS control plane), **openbox-core** (Go data
  plane), **openbox-temporal-sdk-python** (agent-runtime SDK).

## Build and test

```bash
go build ./cli/...                 # the binary
cd <module> && go test ./...        # per module (go.work lists them all, ADR-0011)
./testbed/run-all.sh               # end to end, needs a local OpenBox stack
```

## Current state

Shipped and verified end to end: observe telemetry, enforce (Tier 1–3), the E9
approval loop with hold + rewake, the autonomous approver (ADR-0012), lineage
(trailer → signed attestation → deploy → queryable links) including its read side,
the managed-config posture layer, and `openbox init` as the single onboarding front
door with `--role approver` and `--base-url`.

Near-real-time delivery (`hookflow.RealtimeTrigger`: debounced detached flusher per
session, default on, `OPENBOX_REALTIME=0` opt-out) is implemented and verified at
the binary level (`TestHookRealtimeDelivery` drives the real binary against a mock
core); its testbed phase (`testbed/25-realtime.sh`) exists but has not yet run
against a live local stack.

**Model turns are Activities too** (ADR-0014, 2026-08-11): `TurnStarted`/
`TurnCompleted` → `ActivityStarted`/`ActivityCompleted` with
`activity_type: "llm_completion"` and `{model, usage{4 counts}}` in
`activity_output` — the AI-Agent `llm_completion` span's `response_body` shape on
the activity carrier, since dev sessions write no spans. Claude Code emits one pair
per turn from new `Stop`/`SubagentStop` hooks over a byte-offset cursor
(`hookflow.TurnCursor`, agent-scoped, spool-then-cursor ordering so a crash
over-reports into core's dedupe rather than losing a turn); Codex emits one
`<session>:usage:rollup` pair at SessionEnd, its `Stop` deliberately unwired.
`client.Tokens` gained both cache counts and **`Input` is now pure input** — the
one non-additive change, which is why the contract is **v1.1**. `Finops` became
`*bool` before the default could flip: as a plain bool an absent config field and
an explicit `false` were indistinguishable, so the flip would have been a silent
no-op. **Status: implemented, unit-verified, reviewed, NOT yet run against a live
stack** — and **write-only until openbox-core ships the activity-usage extractor**
(spec: `plans/260811-1640-coding-agent-token-usage/reports/core-issue-activity-usage-extractor.md`,
not yet filed — this checkout's `gh` cannot see that repo). Until it ships,
`llm_completion` also shows up under core's *tool* metrics, because
`ExtractToolMetric` accepts any non-empty `activity_type`.

**Tool events are Activities** (ADR-0013, 2026-08-11): `ToolCall` →
`ActivityStarted`, `ToolResult` → `ActivityCompleted`, both span-less and
hook-less. `client/hookspan.go` and `client/spanbuilder.go` are deleted, and with
them ADR-0004's standing mirror obligation. The adapter-facing schema did not
change. **Status: implemented and unit-verified, NOT yet run against a live
stack.** `activity_id` is byte-identical to the old shape (pinned in
`client/approval_key_pin_test.go`), the golden fixtures pin the new wire bytes,
and all 11 modules are green — but core's ingest behavior was established by
reading openbox-core, and this repo's own rule is that reading is not evidence.
The load-bearing unverified claim is that core stores the `ActivityCompleted` as
its own row; its dedupe key includes `event_type`
(`activities/governance/validation.go:96`), which says it should. The testbed
assertions are updated and waiting for a run; `MAPPING.md` §7 lists exactly what
that run must confirm.

Known limits, documented in
`docs/architecture.md#assurance--what-the-evidence-proves`: the backend does not sign
policy bundles yet (so `require_verified_bundle` defaults off), Codex's hook cannot be
mandated by `requirements.toml`, Guardrail redaction at source is not wired, the
production-runtime lineage hop is not joined, token usage is write-only until core
ships its extractor, and Codex reports usage per session rather than per turn.

Two things about the turn feature stay empirically open until the testbed runs, and
both are pre-decided either way rather than blocking: whether `Stop` fires on a
tool-only turn (window sums are exact regardless of cadence), and whether
`SubagentStop`'s transcript window carries `isSidechain` lines (the partition cannot
double-count in any case; its worst case is a subagent reporting nothing). The
static measurement behind both is in
`plans/260811-1640-coding-agent-token-usage/reports/measure-260811-transcript-turn-surface.md`.

Next: the Cursor adapter; policy template packs. (Upstreaming the
`shell`/`mcp`/`tool` hook types to `openbox-sdk-python` is no longer needed —
ADR-0013 retired the Go mirror by deleting the span layer it mirrored.)

The epic-by-epic history is in git, not in the tree: read commit messages and the
ADRs for *why*, and the code for *what is true now*.
