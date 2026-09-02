---
title: "Agent behaviour assurance: observe a real Mastra agent and produce actionable security feedback"
description: "Roadmap from the working one-scenario conformance slice to observing genuine agent behaviour and emitting security findings and suggestions"
status: superseded
priority: P0
created: 2026-08-25
depends_on: plans/260819-1600-project-security-evaluation
---

# Agent behaviour assurance

> Superseded for execution on 2026-08-26 by the lean local OpenShell plan.
> This file remains historical evidence; its native-runner phases are not
> reachable implementation authority.

## Implementation migration

The remaining intent is implemented only through the active lean plan and its
decision-complete phase documents:

| Historical phase | Active disposition |
|---|---|
| Phase 1 — render static coverage feedback | Replaced by [Phase 4](../260825-1623-lean-openshell-project-assurance/phase-04-report-and-openbox-recommendations.md), which validates issues from actual backend-observed behavior and renders Markdown, JSON, and SARIF. |
| Phase 2 — close the Seatbelt read gap | Retired with the native project-runner path. OpenShell isolation gaps remain development coverage limitations, not an OpenBox security finding. |
| Phase 3 — build a second model-driven fixture | Replaced by [Phase 2](../260825-1623-lean-openshell-project-assurance/phase-02-backend-observation-bundle.md). Mastra remains deterministic conformance evidence; release qualification requires a user-supplied second SDK-integrated image. |
| Phase 4 — qualify more action classes | Replaced by the complete paginated backend event/span snapshot and explicit coverage ledger in active Phase 2. |
| Phase 5 — add fixed scenarios | Deferred. [Phase 1](../260825-1623-lean-openshell-project-assurance/phase-01-developer-image-openshell-execution.md) invokes the image's declared contract once; no scenario engine is part of the lean workflow. |
| Phase 6 — governed rerun | Deferred. Active [Phase 3](../260825-1623-lean-openshell-project-assurance/phase-03-native-host-security-analysis-skill.md) and Phase 4 produce validated inert recommendations only. |

The active parent [delivery ledger](../260825-1623-lean-openshell-project-assurance/plan.md#delivery-ledger)
is the sole status authority. Nothing in this historical document authorizes a
native runner, scenario, control application, approval, or governed rerun.

## Goal

Run the Mastra app end to end in the native host sandbox, observe **what its
agents actually do**, and emit security feedback and suggestions.

## Decisions taken (2026-08-25)

- **The first-party Seatbelt driver is the native host sandbox.** Same kernel
  mechanism Codex and SRT use, with a profile OpenBox owns and can prove. No
  further work on the Codex or SRT drivers; both stay as unsupported
  comparative rows and neither is reachable as a fallback.
- **Retention stays `redacted_digests`.** No prompts, tool arguments or model
  output are persisted. Richer feedback comes from **breadth** — more action
  classes, more scenarios, coverage gaps surfaced as findings — not from
  retaining content. Feedback is structural: *this seam is unguarded*, never
  *the agent said X*.

## Where we are against the goal

| Goal element | Today | Gap |
|---|---|---|
| Run Mastra e2e in a native host sandbox | **Done.** `exploitable` / `runtime_enforceable`, verified 14-role pack, repeatable | — |
| Observe agents' behaviour | **Not started.** The fixture hardcodes `toolName: "recording-tool"`; the model only supplies the payload | Needs an agent that genuinely selects tools |
| Security feedback and suggestions | **One finding, one Rego candidate.** Severity is `unavailable` | Coverage gaps are computed but never rendered |

### The finding that reframes this work

`testbed/.../src/index.mjs:39-46` returns a fixed tool call regardless of the
model response. Even the Ollama variant only maps the model's answer into that
one call's *input*. It is a deterministic conformance harness — exactly right
for proving the SDK seam fires, and unable in principle to show agent behaviour.
**Phase 3 is therefore the load-bearing phase, not Phase 1.**

### The asset we already have and do not use

`sdkdesc` already classifies eight action surfaces — `agent`, `approval`,
`database`, `file`, `http`, `mcp`, `model`, `retrieval` — with a classification
and a reason each (`coverage.go:139`). Only `recordingTool` is a qualified
semantic gate; everything else is recorded as `unsupported`, `disabled` or
`unknown`. **Those gaps are already inside every pack and the report renders
none of them.** They are precisely the structural security feedback the goal
asks for.

## Phases

### Phase 1 — Render the feedback already in the pack

Turn coverage gaps into first-class report output: per-surface findings that say
which seams are guarded, which are unguarded, and why, each bound to its
evidence digests. Extend `propose` to emit a suggestion per actionable gap.

- No new observation, no schema change — the data is already in the pack.
- Exit: `project report` on the existing Mastra pack lists every unguarded
  surface with its reason; `propose` suggests a control per actionable gap.
- Risk: `severity: unavailable` is deliberate in v1. Do not invent severity;
  rank by reachability class instead.

### Phase 2 — Close the sandbox read gap

The profile emits `(allow file-read*)` with no denies. A run can read
`~/.ssh`, `~/.aws/credentials` and shell rc files and reach a declared channel
with them — demonstrated end to end on 2026-08-24. SRT denies exactly those
paths, so this is a regression against the candidate it replaced.

- Add `DeniedReadRoots` to `SeatbeltProfileSpec`, emitted as `(deny file-read* …)`
  after the broad allow (mechanism verified: denies apply, Node still runs).
- Scope `mach-lookup` rather than granting it wholesale.
- Add `DeniedReadPaths` to `HelperChecks` plus a denied-read observation, using
  the same falsifiability rule as the live unapproved listener: a sentinel that
  provably reads **outside** the sandbox and must fail inside.
- Exit: the probe fails a run whose profile allows a sensitive read.
- **Without the probe half this phase is unverified** — `HelperChecks` has no
  denied-read concept today, so the gate cannot see this class at all.

### Phase 3 — A fixture whose agent actually decides

The one phase that makes the goal's middle clause true.

- A second fixture beside the conformance one: a real Mastra `Agent` with
  several tools, where the model's choice determines which tool runs.
- Keep the poisoned-dependency seam so the injection scenario still applies.
- The deterministic harness must stay available: a model-driven fixture is not
  reproducible run to run, so conformance keeps the scripted one.
- Exit: two runs of the same fixture with different model output select
  different tools, and the pack shows which.
- Risk: non-determinism collides with byte-stable evidence. Bound it by
  asserting on the *set* of observed action classes, not an exact transcript.

### Phase 4 — Observe more than one action class

`recordingTool` is the only qualified gate, so an agent's HTTP calls, MCP
traffic and retrieval are invisible to correlation even when the SDK emits them.

- Qualify additional action classes in the descriptor, thread them through
  `sdkdesc` expectations and `evidence.Correlate`.
- Exit: a run reports per-class observed counts, and a class the project used
  but the SDK did not gate appears as a gap rather than silence.

### Phase 5 — More than one scenario

`ScenarioID` is a package constant; `Judge` rejects anything else
(`judge.go:12,137`) and the outcome table is global.

- Make the outcome table scenario-scoped and un-hardcode `Judgment.ScenarioID()`.
- Add a second scenario against a different seam — unguarded MCP or direct
  egress — reusing the `scenario` package added on 2026-08-25.
- Exit: one run evaluates several scenarios and the pack carries a finding each.

### Phase 6 — Governed rerun

Phase 07 of the parent plan. Only after the above does a `blocked` verdict mean
"OpenBox stopped a real agent doing a real thing" rather than "OpenBox stopped
the scripted call the fixture was always going to make".

## Sequencing

Phase 1 first — highest value per unit of work, and it improves the feedback for
every later phase. Phase 2 next: it is a live security defect in the shipped
supported driver, and the product should be able to make this finding about
itself. Phase 3 is the goal's centre of gravity. Phases 4 and 5 widen what a run
can say. Phase 6 last.

Phases 1 and 2 are independent and can run in parallel. Phase 4 should land
before Phase 5, so a new scenario has classes to predicate on.

## Out of scope

- Codex and SRT driver work; both remain unsupported with reasons intact.
- Any retention change. No prompt, tool argument or model output is persisted.
- Severity scoring. v1 reports `unavailable` deliberately.
- New table, endpoint or service — any of those needs its own ADR.

## Standing rules carried from the parent plan

- Missing semantic evidence fails to `inconclusive`/`not_runnable`. It never
  silently downgrades a predicate.
- A denial must be falsifiable: prove the thing is reachable outside the
  sandbox before claiming the sandbox denied it.
- An unread output is an untested one. Every artifact a phase adds must have a
  consumer in the same phase.
