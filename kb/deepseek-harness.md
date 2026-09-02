# DeepSeek Harness: execution model, behavior, and security enforcement

Status: research snapshot, 2026-08-17.
Evidence: official documentation plus the MIT-licensed source at commit
[`47f943859bef60e4160492346772ded9b24f765a`](https://github.com/deepseek-ai/DeepSeek-Harness/tree/47f943859bef60e4160492346772ded9b24f765a).

## Scope and maturity

This document covers the official `deepseek-ai/DeepSeek-Harness` repository,
not similarly named third-party projects. The project identifies itself as a
developer preview and warns that compatibility-breaking changes will occur.
Source findings are pinned to the commit above; they are not release-qualified
behavior for every build or deployment.

Unlike Codex, Claude Code, and Cursor, DeepSeek Harness is not only an authoring
product. It is a composable TypeScript agent harness that can be embedded or
assembled into application runtimes. That lets it participate in both Shift
Left lanes: developer-machine governance when used as a coding agent, and
runtime governance when an OpenBox control plugin is actually composed into a
deployed harness.

## How it works

### Cordis composition

DeepSeek Harness is built as a plugin system over Cordis. Models, tools,
sessions, persistence, settings, approval, sandbox providers, credentials,
telemetry, MCP, and user interfaces are composed as services and consumers.
Profiles and bundles select a deployment's plugin graph. Services can be
replaced without changing their consumers, and scoped composition can provide
different capabilities to different agents. See
[Architecture](https://github.com/deepseek-ai/DeepSeek-Harness/blob/47f943859bef60e4160492346772ded9b24f765a/docs/architecture.md).

This makes the effective security posture a property of the loaded composition,
not the package name. A deployment with a sandbox-aware shell provider differs
from one with an unconfined executor even if both use the same agent loop.

### Agent and tool loop

The loop asks a model for messages and tool calls, schedules requested calls,
and feeds ordered results back into later model steps. Tool dispatch may overlap
through a bounded pool, while calls, policy decisions, results, and added
context are committed in model order. The session appends `tool/call` before
preparation or dispatch and later appends a linked result; aborted unstarted
calls receive synthetic error results so replay remains structurally valid
([tool-call scheduler](https://github.com/deepseek-ai/DeepSeek-Harness/blob/47f943859bef60e4160492346772ded9b24f765a/packages/core/agent-loop/src/tool-calls.ts#L59-L280)).

The session event log is append-only and is the durable source used to rebuild
model context. This is valuable audit structure, but durability alone does not
authenticate the event producer or prove an external side effect happened as
recorded.

### Tool execution pipeline

Every registered tool travels through a shared runtime pipeline:

1. parse and freeze the call identity and arguments;
2. run ordered `tools/pre-execute` policy listeners;
3. resolve `ask` through the approval service;
4. run monotonic guards, where a deny cannot be reversed by listener ordering;
5. dispatch the tool body through around-execution wrappers;
6. normalize errors and values;
7. run `tools/post-execute`, which can accept, replace/enrich, or block the
   result before it reaches the model; and
8. freeze and durably append the final result.

The source implements the pre-execute and monotonic-guard ordering directly
([tool runtime](https://github.com/deepseek-ai/DeepSeek-Harness/blob/47f943859bef60e4160492346772ded9b24f765a/packages/core/tools/src/index.ts#L137-L175),
[guard contract](https://github.com/deepseek-ai/DeepSeek-Harness/blob/47f943859bef60e4160492346772ded9b24f765a/packages/core/tools/src/index.ts#L703-L711),
[pre-execution path](https://github.com/deepseek-ai/DeepSeek-Harness/blob/47f943859bef60e4160492346772ded9b24f765a/packages/core/tools/src/index.ts#L1453-L1506)).
See also the [tool execution pipeline](https://github.com/deepseek-ai/DeepSeek-Harness/blob/47f943859bef60e4160492346772ded9b24f765a/docs/tool-execution-pipeline.md).

### Native tools, Code Mode, MCP, and subagents

The tool registry can present native tools individually or collapse them behind
a `run_code` interface. Code Mode subcalls still traverse the tool runtime rather
than bypassing it. MCP tools are synchronized into the registry with stable
server-qualified names and dispatched through the same runtime. The MCP bridge
uses a two-phase inventory swap so a failed refresh leaves the previous full
generation or no new generation, rather than a partial tool set
([MCP bridge](https://github.com/deepseek-ai/DeepSeek-Harness/blob/47f943859bef60e4160492346772ded9b24f765a/packages/mcp/mcp-client/src/tools.ts#L1-L174)).

Subagent providers are pluggable and can run in-process, in a subprocess, or
through other agent protocols. Delegation therefore creates an authority and
lineage boundary that depends on the selected provider and its inherited tool
filter, workspace, and depth controls.

## Security enforcement layers

| Layer | What it controls | Enforcement character | Important limit |
|---|---|---|---|
| Persona/system prompt/skills | Model behavior | Guidance | Not a reference monitor and can be influenced by untrusted content. |
| Scoped plugin composition | Which services and tools exist for an agent | Runtime-enforced by the composed registry | Deployment owner must actually select the restrictive composition. |
| Tool restrictions and monotonic guards | Visibility and final pre-dispatch deny | In-process runtime boundary | Applies to calls that traverse `ToolRuntime`; alternate execution paths need review. |
| Approval service | One-call human grant or rejection | Fail-closed runtime boundary | `never` rejects rather than asks; UI/answerer availability affects outcome. |
| Post-execute policy | What tool result reaches the model | Runtime boundary after side effect | Can block poisoned output from the model but cannot undo the tool's external effect. |
| Process sandbox | File effects of spawned processes | OS/process boundary | Explicitly does not govern network or process visibility; `danger-full-access` bypasses it. |
| Session log | Ordered calls, results, approvals, and policy changes | Durable evidence | Local durability is not independent attestation. Telemetry export needs redaction policy. |
| Credential/environment handling | Provider keys and subprocess environment | Process/data boundary | Configuration and caller-supplied environment remain part of the trusted base. |

### Approval

The approval service has four outcomes: `allowed-once`, `rejected`, `cancelled`,
and `unavailable`. Only `allowed-once` grants the pending action. A missing,
throwing, cancelled, or invalid answerer does not become an implicit allow.
Each request and outcome is durably logged as a paired audit event tied to the
tool and, when available, its call identifier
([approval source](https://github.com/deepseek-ai/DeepSeek-Harness/blob/47f943859bef60e4160492346772ded9b24f765a/packages/interaction/user-approval/src/index.ts#L1-L94),
[request path](https://github.com/deepseek-ai/DeepSeek-Harness/blob/47f943859bef60e4160492346772ded9b24f765a/packages/interaction/user-approval/src/index.ts#L240-L328)).
See [Approval](https://github.com/deepseek-ai/DeepSeek-Harness/blob/47f943859bef60e4160492346772ded9b24f765a/docs/subsystems/approval.md).

### Sandbox and permission presets

The sandbox vocabulary is intentionally about filesystem effects:
`read-only`, `workspace-write`, and `danger-full-access`. The policy service's
default mode is read-only, while user-facing permission presets bundle a
sandbox mode with an approval policy. The selected composition can choose a
different starting preset, so consumers should record the effective values
rather than infer them from a library default.

Local backends include bubblewrap or Landlock on Linux, Seatbelt on macOS, and
an ACL/restricted-token implementation on Windows. A backend reports `full` or
`partial` enforcement. If a confined mode is requested and no backend is
usable, the sandbox provider is specified to fail closed instead of silently
running unconfined
([sandbox contract](https://github.com/deepseek-ai/DeepSeek-Harness/blob/47f943859bef60e4160492346772ded9b24f765a/packages/sandbox/sandbox/src/index.ts#L24-L175),
[sandbox policy](https://github.com/deepseek-ai/DeepSeek-Harness/blob/47f943859bef60e4160492346772ded9b24f765a/packages/sandbox/sandbox-policy/src/index.ts#L60-L150)).

The boundary is narrower than some coding-host sandboxes: the project explicitly
places network and process visibility outside this sandbox vocabulary. See
[Sandbox](https://github.com/deepseek-ai/DeepSeek-Harness/blob/47f943859bef60e4160492346772ded9b24f765a/docs/subsystems/sandbox.md) and
[Permission presets](https://github.com/deepseek-ai/DeepSeek-Harness/blob/47f943859bef60e4160492346772ded9b24f765a/docs/subsystems/permission-presets.md).

### Credentials, subprocesses, and telemetry

The defensive guidance scrubs credential-like environment names such as keys,
secrets, tokens, and passwords from selected spawned-process environments, uses
private temporary locations, and recommends orthogonal result states rather
than ambiguous booleans. These are concrete hardening patterns, not a guarantee
that every plugin uses them
([defensive patterns](https://github.com/deepseek-ai/DeepSeek-Harness/blob/47f943859bef60e4160492346772ded9b24f765a/docs/defensive-patterns.md)).

Telemetry deserves separate treatment. The session-telemetry seam documents no
built-in redaction rules: without a deployment-provided listener, captured file
content or command output may leave the process as recorded. Provider API keys
passed as adapter constructor parameters are structurally absent from session
events, but secrets embedded in ordinary content are not automatically safe
([telemetry contract](https://github.com/deepseek-ai/DeepSeek-Harness/blob/47f943859bef60e4160492346772ded9b24f765a/packages/session/session-telemetry/README.md)).

## Trust boundaries and failure cases

1. **Composition is authority.** The loaded plugin graph determines whether
   policy, approval, sandbox, persistence, and redaction are present. Package
   installation is not evidence of activation.
2. **ToolRuntime coverage.** The pre/post pipeline is strong for registered
   tools. A plugin or application path that performs effects outside it is a
   separate boundary.
3. **Filesystem-only sandbox vocabulary.** Network egress and process visibility
   require other controls. They must not be implied by `workspace-write`.
4. **Partial enforcement.** A reported partial backend is not equivalent to full
   confinement; downstream policy must preserve that distinction.
5. **Post-execute timing.** Blocking a malicious tool result protects the next
   model step, but the tool body may already have changed external state.
6. **Developer preview.** API and behavior drift are expected. An integration
   needs a pinned version and conformance suite.
7. **Mutable deployment.** The open plugin configuration is not automatically an
   organization-managed, non-user-overridable policy channel.
8. **Telemetry content.** Exporting raw session records without a redaction
   plugin can disclose sensitive prompts, file content, and command output.

## Relevance to Shift Left

DeepSeek Harness is the cleanest source-level opportunity for a native OpenBox
runtime integration. A plugin can attach at `tools/pre-execute`, use a monotonic
guard for an irrevocable deny, observe or filter `tools/post-execute`, and record
the exact session/call lineage. This is stronger and less lossy than translating
the harness through a generic shell hook.

The integration should remain experimental while the harness is in developer
preview. It should be a thin Cordis plugin over the existing OpenBox decision
and event contracts, pin a tested harness version, and report when sandbox
enforcement is partial or absent. It should not claim network isolation because
the native sandbox does not provide it.

DeepSeek Harness is also a high-value target for the project evaluator: its
profile graph, tool registry, MCP configuration, approval policy, sandbox
provider, telemetry/redaction composition, and subagent providers are all
discoverable from source/configuration. Static discovery still needs controlled
tests to establish which branches are actually reachable.

## Primary sources

- [Official repository and developer-preview notice](https://github.com/deepseek-ai/DeepSeek-Harness)
- [Pinned source snapshot](https://github.com/deepseek-ai/DeepSeek-Harness/tree/47f943859bef60e4160492346772ded9b24f765a)
- [Architecture](https://github.com/deepseek-ai/DeepSeek-Harness/blob/47f943859bef60e4160492346772ded9b24f765a/docs/architecture.md)
- [Tool execution pipeline](https://github.com/deepseek-ai/DeepSeek-Harness/blob/47f943859bef60e4160492346772ded9b24f765a/docs/tool-execution-pipeline.md)
- [Approval](https://github.com/deepseek-ai/DeepSeek-Harness/blob/47f943859bef60e4160492346772ded9b24f765a/docs/subsystems/approval.md)
- [Sandbox](https://github.com/deepseek-ai/DeepSeek-Harness/blob/47f943859bef60e4160492346772ded9b24f765a/docs/subsystems/sandbox.md)
- [Permission presets](https://github.com/deepseek-ai/DeepSeek-Harness/blob/47f943859bef60e4160492346772ded9b24f765a/docs/subsystems/permission-presets.md)
- [Defensive patterns](https://github.com/deepseek-ai/DeepSeek-Harness/blob/47f943859bef60e4160492346772ded9b24f765a/docs/defensive-patterns.md)
