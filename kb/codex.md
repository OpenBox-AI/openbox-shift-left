# Codex: execution model, behavior, and security enforcement

Status: research snapshot, 2026-08-17.
Evidence: official documentation plus the open-source Codex CLI at commit
[`37cf6c84c086229ed78d69e726b965c0252e9b3e`](https://github.com/openai/codex/tree/37cf6c84c086229ed78d69e726b965c0252e9b3e).

## Scope and evidence boundary

"Codex" spans an Apache-2.0 CLI/runtime and closed hosted model, service, and
cloud surfaces. The source findings below apply to the pinned CLI commit. They
do not establish how the hosted model or service is implemented, and a source
capability is not proof that a particular installed binary has it enabled.

The current Shift Left adapter was reviewed separately. Its supported-version
and installation claims should be validated against the actual Codex binary;
upstream changed materially around managed hooks.

## How it works

### Agent loop

The CLI constructs a turn from user input, effective configuration, workspace
instructions, skills/plugins, available tools, and MCP servers. Its core loop
asks the model for either assistant text or one or more function calls. A
function call is executed and its result is returned to the next model sample;
an assistant-only response completes the turn. The source also shows
pre-sampling compaction, required-MCP resolution, and context injection before
sampling ([turn loop](https://github.com/openai/codex/blob/37cf6c84c086229ed78d69e726b965c0252e9b3e/codex-rs/core/src/session/turn.rs#L139-L260)).

Codex can therefore behave iteratively rather than as a one-shot code
generator: inspect, plan, call tools, observe results, revise, and continue.
The model proposes actions; the host owns tool registration, approval,
sandboxing, hook dispatch, and actual execution.

### Inputs that shape behavior

- Configuration is layered, with user, project, command-line, and managed
  sources contributing to the effective runtime posture.
- `AGENTS.md`, skills, and plugin material contribute model-visible guidance or
  capabilities. They influence behavior but are not execution boundaries.
- Built-in tools cover repository inspection, file edits, shell execution, and
  other host capabilities. MCP adds external tools.
- Context may be compacted as it grows. A long session therefore should not be
  treated as verbatim, indefinitely retained prompt state.

### MCP

MCP servers are first-class tool providers. The source supports required-server
startup behavior, server-level and per-tool approval modes, explicit tool
allow/deny lists, timeouts, and OAuth settings
([MCP configuration](https://github.com/openai/codex/blob/37cf6c84c086229ed78d69e726b965c0252e9b3e/codex-rs/config/src/mcp_types.rs#L74-L251)).
An MCP server expands both authority and data exposure: its tool descriptions
enter the model's capability surface and its tool process or remote endpoint is
a separate trust domain.

## Observable behavioral properties

- Codex may issue multiple tool calls from one model response. Tool order and
  parallelism cannot be inferred solely from the conversational transcript.
- Approval is action-specific host mediation, not evidence that an action is
  safe. A user can approve a harmful operation.
- With workspace-write permissions, the agent can normally modify the selected
  workspace and permitted temporary locations. Wider access depends on the
  active permission profile, sandbox, and approvals.
- With danger/full-access permissions, the OS sandbox no longer provides the
  normal filesystem boundary. The agent then has essentially the authority of
  the launching user, subject to remaining host and network controls.
- Instructions can discourage an action, but only a host control that sees the
  relevant execution path can prevent it.

## Security enforcement layers

| Layer | What it controls | Enforcement character | Important limit |
|---|---|---|---|
| Instructions, `AGENTS.md`, skills | Model behavior and workflow | Guidance | Prompt injection or model error can defeat guidance; it is not a reference monitor. |
| Tool registration and MCP tool filters | Which capabilities the model can call | Host-enforced for registered tools | Does not constrain equivalent capability reachable through another tool, such as shell. |
| Approval policy | Whether selected actions need a human decision | Host-enforced before the covered action | Full-auto/never-ask modes remove prompts; approval is not containment. |
| OS sandbox | Filesystem and, when proxying is configured, network effects | OS/process boundary | `danger-full-access` bypasses confinement; writable roots and network grants remain attack surface. |
| Network proxy/domain rules | Outbound reachability for sandboxed commands | Host/proxy boundary | Applies only when network traffic is routed through the controlling path. |
| Exec policy | Classification/escalation of shell commands | Host-enforced on covered execution | Coverage is the shell execution path, not every possible tool implementation. |
| `PreToolUse` hooks | Observe a pending tool call and synchronously deny or rewrite supported inputs | Host-enforced on surfaced hook events | Upstream warns that specialized tool paths can opt out; hooks are a guardrail, not a complete security boundary. |
| Managed requirements | Pin permission profiles and managed hooks; optionally ignore unmanaged hooks | Organization-managed configuration | Requires a supporting Codex version and intact managed delivery on the endpoint. |

### Permissions and sandboxing

Current Codex documentation describes permission profiles that compose
filesystem and network policies. The underlying controls include read-only,
workspace-write, and danger-full-access sandbox modes plus approval policies.
Network domain rules depend on the network proxy path; setting a domain rule
does not independently create network confinement. See
[Permissions](https://learn.chatgpt.com/docs/permissions),
[Sandboxing](https://learn.chatgpt.com/docs/sandboxing), and
[Agent approvals and security](https://learn.chatgpt.com/docs/agent-approvals-security).

### Hooks

Hooks are spawned handlers around lifecycle and tool events. `PreToolUse` is
the relevant synchronous enforcement point: supported structured output can
deny a call or allow it with rewritten input; exit status 2 is also a blocking
signal. Asynchronous hooks can observe or add later context but cannot be the
authority for an already executed action. The source keeps synchronous control
effects distinct from asynchronous results
([hook engine](https://github.com/openai/codex/blob/37cf6c84c086229ed78d69e726b965c0252e9b3e/codex-rs/hooks/src/engine/mod.rs#L90-L112),
[pre-tool event](https://github.com/openai/codex/blob/37cf6c84c086229ed78d69e726b965c0252e9b3e/codex-rs/hooks/src/events/pre_tool_use.rs)).

Current documentation and source support managed hooks in
`requirements.toml`, plus `allow_managed_hooks_only`. Discovery loads required
managed handlers before lower configuration layers and can exclude unmanaged
sources
([hook discovery](https://github.com/openai/codex/blob/37cf6c84c086229ed78d69e726b965c0252e9b3e/codex-rs/hooks/src/engine/discovery.rs#L79-L165)).
This is a current upstream capability, not proof that every deployed Codex
version supports it. See [Hooks](https://learn.chatgpt.com/docs/hooks) and the
[configuration reference](https://learn.chatgpt.com/docs/config-file/config-reference).

## Trust boundaries and failure cases

1. **Model versus host.** The model requests; the host executes. Security claims
   should be made about the host path, not the model's stated intention.
2. **Hook coverage.** A hook can govern only events Codex emits to it. Upstream
   explicitly documents exceptions for specialized tool paths, so "hook
   installed" must not be translated into "all actions governed."
3. **Configuration authority.** User-owned hook files are removable. Managed
   requirements improve authority only when endpoint management and the pinned
   Codex version are verified.
4. **Hook availability.** Timeouts, crashes, invalid output, and disabled hooks
   need an explicit fail-open/fail-closed interpretation. The host's behavior,
   not the hook author's intent, determines the outcome.
5. **Local principal.** A process running as the developer may read the same
   files and credentials as the agent unless the OS sandbox or another boundary
   prevents it.
6. **MCP.** An approved or allow-listed MCP server is still executable code or a
   remote trust boundary. Its server identity, tool inventory, authentication,
   and data destinations need separate review.
7. **Source versus installed product.** Open source establishes implementation
   possibilities at one commit. A capability probe and controlled black-box
   test are still required before making fleet claims.

## Relevance to Shift Left

Codex offers the surfaces Shift Left needs for build-time governance:
pre-execution tool decisions, post/lifecycle telemetry, organization-managed
configuration, native sandbox posture, and MCP inventory. Shift Left should
add behavior-specific policy rather than duplicate the Codex sandbox.

The most important current delta is managed hooks. Shift Left's shipped
installer writes user-scoped `hooks.json`; its architecture and managed-deploy
guide still describe the hook as not yet mandated, while the adapter README
already names managed hooks as the future hardening path. Upstream now documents
and implements managed requirement hooks. Migration must be version-gated and
black-box tested; it should not simply replace the legacy installer based on
source inspection.

For project/runtime evaluation, Codex is primarily an authoring host. Inspecting
a repository from Codex or observing Codex's development actions does not prove
the deployed agentic project is governed. That requires a separate explicit
project test and an OpenBox integration in the application's runtime path.

## Primary sources

- [Codex permissions](https://learn.chatgpt.com/docs/permissions)
- [Codex sandboxing](https://learn.chatgpt.com/docs/sandboxing)
- [Agent approvals and security](https://learn.chatgpt.com/docs/agent-approvals-security)
- [Codex hooks](https://learn.chatgpt.com/docs/hooks)
- [Codex configuration reference](https://learn.chatgpt.com/docs/config-file/config-reference)
- [Codex source, pinned snapshot](https://github.com/openai/codex/tree/37cf6c84c086229ed78d69e726b965c0252e9b3e)
