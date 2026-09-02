# Shift Left opportunities across agentic ecosystems

Status: discussion draft, not an accepted ADR and not implementation approval.
Date: 2026-08-17.

## Executive position

Shift Left should integrate at each host's strongest native enforcement point
while keeping one provider-neutral OpenBox decision and evidence engine. The
near-term order should be:

1. add versioned capability probes and correct Codex managed-hook drift;
2. strengthen the existing Claude Code managed posture;
3. build separate local and cloud capability profiles for a Cursor adapter;
4. prototype a native DeepSeek Harness policy plugin as an experimental runtime
   integration; and
5. let `openbox project` inspect these ecosystems without confusing authoring
   posture with deployed-runtime security.

The opportunity is not to replace each host's sandbox or permission system.
OpenBox adds organization-specific behavioral decisions, cross-host evidence,
approval workflow, and runtime policy continuity on top of the host's actual
control surface.

This discussion uses the current repository implementation plus the research
notes for [Codex](../kb/codex.md), [Claude Code](../kb/claude-code.md),
[Cursor](../kb/cursor.md), and [DeepSeek Harness](../kb/deepseek-harness.md).

## Current Shift Left baseline

The repository already has a sound separation:

- one `openbox` CLI with `auth`, `init`, `dev verify`, `hook`, `rewake`,
  `approve`, `managed`, and `doctor` commands;
- a provider SPI in [`provider/`](../provider/) for installation and native
  hook execution;
- a shared engine in
  [`adapters/common/hookflow/`](../adapters/common/hookflow/) for redaction,
  synchronous evaluation, failure policy, approvals, findings, and telemetry;
- thin real adapters for Claude Code and Codex; and
- a Cursor stub in
  [`cli/internal/providers/providers.go`](../cli/internal/providers/providers.go).

Every mapped gating event currently receives a synchronous decision from
`/api/v1/governance/evaluate`. Secret redaction remains local; ordinary
telemetry uses the spool. A no-verdict failure proceeds by default or becomes a
synthetic deny when the effective organization posture is fail-closed. There is
no resident daemon.

Important current limits should shape all integrations:

- a host hook can only govern events the host emits through that hook path;
- user-installed hooks can usually be removed without managed configuration;
- the control-plane round trip is on the blocking path;
- content-based policy sees capped content, while local redaction has its own
  coverage limits;
- the developer principal, and therefore the coding agent in sufficiently broad
  modes, can read the plaintext OpenBox credentials stored for that principal;
- absence of telemetry is not evidence of inactivity; and
- the live control-plane behavior called out in the architecture remains less
  proven than local stub and conformance tests.

The architecture's governance-level table also labels both Observe and Enforce
as defaults in different rows. New posture surfaces should report the resolved
effective value and provenance rather than inherit an unqualified default label.

## Host capability comparison

This table is a documented/source snapshot, not a compatibility promise.

| Host | Pre-action control | Input/result transformation | Managed authority | Native confinement | Key limitation for OpenBox |
|---|---|---|---|---|---|
| Codex | Synchronous `PreToolUse` deny on surfaced tools | Supported inputs can be rewritten; hook-level `ask` is not a generally supported result | Current upstream supports managed requirement hooks and `allow_managed_hooks_only` | Permission profiles, approvals, OS sandbox, network proxy, exec policy | Specialized tool paths can omit hooks; installed-version support must be probed |
| Claude Code | `PreToolUse` and `PermissionRequest` can block or ask | Supported tool input/permission decisions can be changed; post hooks observe later state | Managed settings, `allowManagedHooksOnly`, managed permissions/sandbox/MCP | Seatbelt/bubblewrap, permissions, network proxy | Sandbox can degrade to unsandboxed unless configured to fail when unavailable |
| Cursor local | Before-shell, before-MCP, before-read, and generic hook decisions | Event-specific decisions/modification; do not assume universal input rewrite | Project/team/enterprise hooks and controls | Platform agent sandbox plus file/shell permissions | Hook process failures are open by default unless `failClosed` is set |
| Cursor cloud | Managed hook surface | Event-specific | Team/enterprise managed hooks | Cloud-hosted boundary | Current docs omit MCP before/after hooks and some early read-only turns do not run hooks |
| DeepSeek Harness | In-process `tools/pre-execute`, approval, and monotonic guards | Post-execute can replace/enrich/block model-visible results; call identity is protected | Deployment-owned plugin composition; no documented vendor fleet mandate equivalent | File-effect sandbox with full/partial reporting | Developer preview; native sandbox explicitly excludes network/process visibility |

## Cross-host integration contract

The current `Capability{Key, Supported, How}` is useful but too coarse for the
next set of claims. `Supported: true` conflates upstream documentation,
installed configuration, and observed behavior. Evolve posture reporting toward
a versioned capability record such as:

```text
capability: pre_tool_control
surface: cursor-local
state: documented | configured | observed | degraded | unsupported
host_version: ...
mechanism: ...
failure_mode: fail_open | fail_closed | host_defined
managed: true | false | unknown
coverage_exceptions: [...]
evidence: [...]
checked_at: ...
```

At minimum, every adapter should report:

- pre-tool observation and blocking;
- native ask support;
- input rewrite support;
- post-tool success/failure visibility;
- prompt and assistant-response visibility;
- subagent identity and lineage;
- MCP identity and pre/post coverage;
- hook timeout and process-failure behavior;
- installation scope and whether it is managed;
- sandbox filesystem and network posture;
- content capture/redaction posture; and
- the exact host version and surface validated.

This is not a demand for one lowest-common-denominator adapter. The normalized
engine should use the strongest supported behavior and declare the gaps.

## Codex opportunities

### 1. Adopt managed requirement hooks, version-gated

The shipped installer writes a user-level `hooks.json`. Repository documentation
is transitional: the architecture and managed-deploy guide say the hook is not
yet mandated, while the adapter README already identifies managed hooks as the
hardening path. Current upstream documentation and source support managed
requirement hooks plus `allow_managed_hooks_only`. That is a material capability
change.

Recommended work:

- add an installed-version and feature probe rather than infer support from a
  version string alone;
- add a managed deployment generator for required OpenBox hooks;
- preserve the user-hook installer as a compatibility path for older versions;
- make `doctor` identify which source supplied the effective hook and whether
  unmanaged hooks are excluded;
- update assurance documentation only after a controlled installed-tool test;
  and
- test deny, rewrite, timeout, invalid output, disabled feature, untrusted
  workspace, and managed-only discovery.

Managed delivery strengthens non-removability. It does not close upstream's
documented hook-coverage exceptions, so posture must continue to name them.

### 2. Treat native permission and sandbox posture as evidence

OpenBox should not implement another filesystem sandbox. It should record the
effective Codex permission profile, approval mode, network proxy posture, and
MCP tool restrictions beside the OpenBox gate. Policies can then require a
minimum native posture before allowing particularly sensitive actions.

Avoid circular control: a model-callable MCP tool that asks OpenBox whether it
may act is not a reliable gate because the model can choose another path or omit
the call. Enforcement belongs in synchronous host hooks or a lower capability
boundary; an OpenBox MCP tool may expose read-only diagnostics, not authority.

### 3. Improve lifecycle and success evidence

The current adapter intentionally reports some Codex facts at session granularity
and cannot infer every tool outcome. New upstream events should be adopted only
after conformance tests demonstrate stable call/result correlation. Unknown
success should remain unknown rather than derived from absence.

## Claude Code opportunities

### 1. Make managed posture the enterprise path

The existing plugin integration is a strong local packaging path. For enforced
fleets, generate and verify managed settings that:

- install or permit only the intended OpenBox hooks;
- enable `allowManagedHooksOnly` where the organization wants to exclude local
  replacements;
- pin permission rules and allowed permission modes;
- disable bypass modes when required;
- configure sandbox failure and unsandboxed-command behavior explicitly; and
- constrain MCP servers through managed controls.

`openbox doctor` should report all of these as independent facts. A plugin
present in the marketplace or project is not sufficient evidence.

### 2. Use rich lifecycle and subagent events

Claude Code exposes more lifecycle and subagent surface than the current common
denominator needs. Capture parent/child session identity, delegated tool scope,
and completion status without forcing Codex or Cursor to fabricate equivalent
events. This strengthens lineage and lets OpenBox policy distinguish a direct
developer action from a delegated subagent action.

### 3. Couple OpenBox policy to native boundaries, not duplicate them

Examples of complementary policy include "this repository may not invoke this
MCP server," "this tool/data combination requires independent approval," or
"a sensitive egress needs a fail-closed OpenBox verdict." File paths and command
effects still belong primarily to Claude Code's permission and sandbox layers.

## Cursor opportunities

### 1. Replace the stub with two capability profiles

Current Cursor documentation makes an adapter viable, but local and cloud
execution must be modeled separately. Build the local adapter first because its
pre-shell, pre-MCP, and pre-read hooks provide concrete gating points. Do not
claim the same MCP coverage for cloud agents while the official hook reference
documents those gaps.

The first adapter should:

- set `failClosed: true` for every hook used as an enforcement gate;
- use event-specific output contracts rather than assume Claude/Codex response
  shapes;
- preserve full file-content privacy expectations for `beforeReadFile`, whose
  payload itself is sensitive;
- distinguish editor, CLI, and cloud sessions in normalized events;
- capture subagent and workspace identity where exposed; and
- publish explicit unsupported events instead of a generic "Cursor supported."

### 2. Establish black-box conformance before broad rollout

Closed-source hook behavior needs controlled tests for:

- allow, deny, ask, crash, timeout, invalid JSON, and `failClosed`;
- shell matcher edge cases and interpreter indirection;
- before-read timing and payload content;
- MCP local versus cloud coverage;
- early read-only turns;
- project versus team/enterprise precedence; and
- non-interactive CLI behavior.

Documentation establishes what to probe. Only repeated installed-host behavior
qualifies a release.

### 3. Use enterprise distribution where authority matters

Project-local Cursor hooks are useful for adoption but remain developer-owned.
Team or enterprise managed hooks are the stronger enforcement path. The
installer should describe the difference rather than presenting both as the
same scope.

## DeepSeek Harness opportunities

### 1. Build a native Cordis plugin, not a shell-hook adapter

A thin OpenBox plugin can integrate directly with the shared tool pipeline:

- observe `tools/pre-execute` and build the normalized OpenBox event;
- obtain the bounded OpenBox verdict;
- express a final deny through a monotonic guard so later listener ordering
  cannot re-allow it;
- map `REQUIRE_APPROVAL` through an explicit authority contract: retain
  OpenBox's independent approval path by default, and use the harness's native
  one-call approval only when policy explicitly permits developer self-approval;
- observe or filter `tools/post-execute` before untrusted tool content returns
  to the model; and
- record the harness session, agent, parent, and call identifiers.

This is a genuine runtime integration when the plugin is composed into the
deployed application. If it is used only in the interactive coding profile, it
is workstation governance. The artifact and posture must say which.

### 2. Preserve harness-native security semantics

- Do not replace the harness's filesystem sandbox; record its mode and
  `full`/`partial` result.
- Do not claim its sandbox controls network or process visibility.
- Treat missing approval as denial, matching the native service.
- Avoid exporting raw session telemetry without an explicit redaction listener.
- Keep call identity immutable and do not make an OpenBox listener capable of
  widening another policy's decision.

### 3. Keep it experimental

The harness is in developer preview. Pin an exact commit or release, run the
upstream and OpenBox conformance suites together, and label the adapter
experimental until the same behavior is repeated on a supported release. Its
clean architecture makes it an excellent design partner, but not yet a reason
to promise fleet stability.

## Project-assurance opportunities

The proposed [`openbox project`](security-evaluate.md) lane can recognize
ecosystem material as discovery evidence:

| Ecosystem | Project-scoped evidence worth inspecting | What it does not prove |
|---|---|---|
| Codex | `AGENTS.md`, project config, hooks, MCP declarations, skills/plugins | That the deployed application uses Codex or is runtime-governed |
| Claude Code | `CLAUDE.md`, project settings, hooks/plugins, `.mcp.json`, subagent definitions | That managed settings are active on every developer endpoint |
| Cursor | project rules, hooks, MCP config, CLI project permissions | That cloud agents run the same hooks or that runtime code is protected |
| DeepSeek Harness | Cordis config/profile, plugin graph, tool/MCP registry, approval/sandbox/telemetry providers | That dynamic branches are reachable or that the deployed composition matches source |

Project inspection should stay rooted in the selected repository. Reading
user-home or enterprise-managed configuration crosses a privacy and scope
boundary; expose it only through an explicit `--include-host-posture` option or
by consuming a separately generated `openbox doctor --json` artifact.

The deeper opportunity is continuity: a finding discovered during development
can produce an inert OpenBox runtime proposal, and a later controlled project
test can verify the deployed interception point. Development telemetry can
support the narrative, but it must not be substituted for runtime proof.

## Shared hardening work

### Capability probes before installers

Each installer should first ask the host what it can do, then select a known
configuration. A static minimum-version constant is necessary but insufficient
when features can be disabled, backported, or surface-specific.

### Conformance fixtures

Maintain provider fixtures for the same semantic cases:

- allow, deny, approval, and rewrite where supported;
- hook failure, timeout, invalid output, and host kill;
- file, shell, MCP, prompt, response, subagent, and session events;
- content redaction before egress;
- duplicate and missing events; and
- managed versus user configuration precedence.

Expected divergence belongs in the capability profile, not in adapter-specific
copies of the engine.

### Content and trust hygiene

Hook payloads, file reads, prompts, tool results, and MCP arguments can contain
secrets or proprietary code. Add per-event content posture and source-side
redaction tests before widening capture. Current Shift Left already documents
that prompt redaction is not fully wired and that server-visible bodies are
capped; new adapters must not obscure those facts.

### Installation integrity

Managed configuration can pin a hook entry, but the hook executable and its
credentials are still local assets. Record binary path, digest/version,
configuration source, and last successful conformance check. Do not call the
result tamper-resistant while the governed principal can replace or invoke
those assets.

## Delivery order

| Priority | Work | Why now | Exit evidence |
|---|---|---|---|
| P0 | Versioned capability/posture schema and host probes | Prevents every later adapter from turning documentation into an unqualified claim | `doctor --json` distinguishes documented, configured, observed, and degraded states |
| P0 | Codex managed-hook compatibility investigation | Current repository assurance text is stale relative to upstream | Pinned installed Codex loads required hook, excludes unmanaged hook when configured, and blocks a fixture call |
| P1 | Claude managed-posture generator/checks | Extends the strongest shipped adapter without a new runtime | Managed permissions/hooks/sandbox/MCP facts are independently verified |
| P1 | Cursor local adapter | Current official hooks offer real pre-action control | Repeated local conformance suite, including `failClosed` faults |
| P2 | Cursor cloud profile | Valuable enterprise surface but known coverage differences | Cloud-specific event matrix with MCP and early-turn gaps preserved |
| P2 | DeepSeek Harness experimental plugin | Direct runtime-policy seam and source-level integration | Pinned composition blocks one controlled tool scenario and reports sandbox quality |
| P2 | Project ecosystem detectors | Connects authoring posture to the new assurance lane without conflating it with runtime | Deterministic project model with explicit unknowns and no code execution |

Any new backend endpoint, table, or service needed for capability attestations,
audit-pack upload, or cross-runtime correlation needs an ADR. The initial work
can remain local and reuse existing event/evaluation APIs where their semantics
already fit.

## What not to build

- A second policy evaluator per host.
- A generic OpenBox MCP server presented as the enforcement boundary.
- Another filesystem sandbox that competes with host-native confinement.
- A provider adapter that reports capabilities the host did not expose.
- One "governed" boolean spanning local, CLI, and cloud execution.
- Automatic project-source edits or policy publication from an audit finding.
- Runtime-security claims derived only from coding-agent telemetry.

## Bottom line

The ecosystems are converging on richer native hooks, managed configuration,
approval, and sandboxing, but their coverage and failure semantics remain
different. Shift Left's opportunity is to normalize decisions and evidence—not
to erase those differences. Codex managed hooks and Cursor's current hook model
open immediate build-time work; DeepSeek Harness opens a credible native runtime
path; `openbox project` connects both lanes through bounded findings and reviewed
integration proposals.
