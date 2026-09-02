# Claude Code: execution model, behavior, and security enforcement

Status: research snapshot, 2026-08-17.
Evidence: official Anthropic documentation. Claude Code is closed source, so
this document does not infer undocumented internals from other coding agents.

## Scope and evidence boundary

Anthropic documents Claude Code's user-visible loop, tools, settings,
permissions, hooks, sandbox, MCP integration, storage, and enterprise controls.
Those documents establish the advertised contract. They do not expose the full
system prompt or implementation, and they do not prove the behavior of a
particular version on a particular endpoint. Security-critical claims still
need an installed-version capability probe and controlled black-box tests.

## How it works

### Agent loop

Anthropic describes Claude Code and the Claude Agent SDK as sharing the same
agent loop:

1. Assemble the prompt from the user's message, system instructions, available
   tools, conversation history, settings-selected project sources, and any MCP
   tools.
2. Ask the model for text or tool calls.
3. Execute requested tools through the host after permission and hook checks.
4. Return tool results to the model.
5. Repeat until the model returns a final response or a configured limit stops
   the run.

Context is finite. Claude Code compacts older conversation state as needed, and
the documented local session representation is JSONL. File checkpoints are
taken before file-changing actions, allowing supported edits to be rewound.
These are recovery and continuity features, not security boundaries. See
[How Claude Code works](https://code.claude.com/docs/en/how-claude-code-works)
and the [agent loop](https://code.claude.com/docs/en/agent-sdk/agent-loop).

### Inputs that shape behavior

- `CLAUDE.md` supplies persistent project or user instructions.
- Skills package reusable guidance and supporting material.
- Subagents isolate delegated work into separate context windows and tool
  scopes, subject to their configuration.
- Plugins can package commands, skills, agents, hooks, and MCP servers.
- MCP connects external tools and data sources.
- Settings determine which project sources are loaded and which permissions,
  hooks, sandbox rules, and enterprise restrictions apply.

These inputs are not equivalent. Instructions influence the model. Permission
rules, hooks, tool registration, and sandboxing are implemented by the host.

## Observable behavioral properties

- Claude Code is iterative and may inspect, edit, execute, delegate, and revise
  within one session.
- Permission checks are evaluated in documented `deny`, then `ask`, then
  `allow` order; the first matching rule determines the result.
- Permission modes alter the amount of interaction: default, accept-edits,
  plan, auto/don't-ask variants, and bypass modes have materially different
  authority. A mode label alone is not enough; effective rules and managed
  restrictions also matter.
- A permission prompt is a decision point, not proof of safety. The developer
  can authorize an unsafe command or MCP operation.
- A checkpoint can restore supported file changes, but cannot undo external
  side effects such as a network request, database mutation, or leaked secret.
- Subagents expand the execution graph. Their parent/child identity, inherited
  authority, and result path need to be captured explicitly for audit.

## Security enforcement layers

| Layer | What it controls | Enforcement character | Important limit |
|---|---|---|---|
| `CLAUDE.md`, skills, model instructions | Intended behavior and workflow | Guidance | Not a reference monitor; untrusted content can influence the model. |
| Tool availability and subagent scopes | Which host capabilities are exposed | Host-enforced | Shell or a broad MCP tool can subsume narrower capabilities. |
| Permission rules and modes | Allow, deny, or prompt for covered tool actions | Host-enforced | Rules are only as complete as their match patterns and surfaced actions. |
| `PreToolUse` and `PermissionRequest` hooks | Synchronous decision before a covered action | Host-enforced on emitted hook events | Hook process failures, timeouts, configuration authority, and event coverage remain material. |
| Post/lifecycle hooks | Observation and later feedback | Evidence/advisory | A post hook cannot reverse an external side effect that already happened. |
| OS sandbox | Filesystem and network constraints for sandboxed processes | OS/process boundary | Availability and escape hatches are configurable; limitations remain on sockets and network identity. |
| Managed settings | Organization-pinned permissions, hooks, sandbox, and MCP rules | Enterprise configuration | Requires managed delivery to the endpoint and a compatible client. |
| Managed MCP controls | Fixed, allowed, denied, or disabled MCP servers | Host configuration | A listed server is not thereby security-audited. |

### Permissions and managed settings

Permission rules are a host control, separate from prompt instructions.
Anthropic explicitly documents that `CLAUDE.md` cannot override a denied
permission. Managed settings have the highest authority and cannot be
overridden by lower-precedence user or project settings.

Relevant managed controls include:

- permission allow/ask/deny rules and permitted modes;
- disabling bypass-permissions behavior;
- `allowManagedPermissionRulesOnly`, which excludes lower-precedence permission
  rules;
- `allowManagedHooksOnly`, which excludes user and project hooks;
- managed sandbox filesystem and network settings; and
- fixed, allow-listed, deny-listed, or fully disabled MCP configuration.

See [Permissions](https://code.claude.com/docs/en/permissions) and
[Settings](https://code.claude.com/docs/en/settings).

### Hooks

Claude Code exposes lifecycle, prompt, tool, permission, subagent, notification,
and session hooks. `PreToolUse` can block before execution; documented hook
output can also update supported inputs or return a permission decision.
`PermissionRequest` can answer a pending host permission request. Exit status 2
is a documented blocking path for blocking-capable events. Post hooks are useful
for telemetry or corrective feedback but are too late to prevent the completed
effect. See [Hooks reference](https://code.claude.com/docs/en/hooks).

### Sandbox

Anthropic documents macOS Seatbelt and Linux bubblewrap-based process
confinement, filesystem boundaries, and a network proxy. The sandbox can reduce
repeated approval prompts while still constraining command effects.

The failure mode is configuration-sensitive. If a sandbox cannot start, the
documented default may warn and run the command unsandboxed; `failIfUnavailable`
makes that a hard failure. `allowUnsandboxedCommands` controls an explicit
escape route. Documented limitations include lack of TLS inspection, domain
fronting risk, Unix-socket reachability, broad writable paths, and weaker
protection in nested sandbox environments. See
[Sandboxing](https://code.claude.com/docs/en/sandboxing).

### MCP

MCP servers add external code and data to the tool graph. Project-scoped MCP
configuration has a trust/approval step. Enterprise deployments can mandate a
specific set, filter it, or disable MCP. Anthropic states that inclusion in its
MCP directory is not a security audit. See
[MCP](https://code.claude.com/docs/en/mcp) and
[Managed MCP configuration](https://code.claude.com/docs/en/managed-mcp).

## Trust boundaries and failure cases

1. **Closed implementation.** The documented loop and controls are the evidence
   boundary. Internal equivalence with Codex, Cursor, or an open harness must
   not be assumed.
2. **Prompt versus policy.** Repository instructions can be modified by a
   contributor and untrusted files can contain prompt injection. They cannot be
   treated as security policy.
3. **Configuration authority.** Project and user hooks are mutable by the same
   principal using the agent. Managed settings are stronger, but only when
   their delivery and effective state are verified.
4. **Sandbox degradation.** Without `failIfUnavailable`, a missing sandbox may
   result in unsandboxed execution after a warning. Posture collection must
   record the effective failure mode, not merely `sandbox.enabled`.
5. **Network identity.** A domain allow-list does not inspect encrypted
   application content and can be affected by redirects, domain fronting, local
   sockets, and subprocess behavior.
6. **Hooks.** A blocking hook governs only documented events that actually fire.
   Timeout, malformed output, recursion, and version drift require probes.
7. **MCP.** Server approval is not a software supply-chain review and does not
   constrain the server's own downstream behavior.
8. **Local credentials.** A command that runs with the developer's authority may
   reach credentials readable by that principal unless sandbox and environment
   handling remove the path.

## Relevance to Shift Left

Claude Code is the most mature current Shift Left integration: it offers rich
hook coverage, native ask/deny behavior, plugin packaging, subagent lifecycle
events, and enterprise-managed settings. Shift Left can add organization- and
behavior-specific decisions while recording the native permission, sandbox,
and MCP posture.

It should not claim complete prevention merely because a plugin is installed.
Assurance requires effective managed settings, blocking-hook behavior, sandbox
failure mode, supported version, and observed event coverage. Where an
organization needs non-removable governance, managed hook and permission rules
are materially stronger than a project-local plugin alone.

Claude Code governs development actions. Its telemetry does not establish the
security of an agentic application after deployment. Project/runtime assurance
must exercise the application's own agent, tools, and data flows, then recommend
or verify an OpenBox runtime integration on those paths.

## Primary sources

- [How Claude Code works](https://code.claude.com/docs/en/how-claude-code-works)
- [Claude Agent SDK agent loop](https://code.claude.com/docs/en/agent-sdk/agent-loop)
- [Features overview](https://code.claude.com/docs/en/features-overview)
- [Permissions](https://code.claude.com/docs/en/permissions)
- [Hooks reference](https://code.claude.com/docs/en/hooks)
- [Sandboxing](https://code.claude.com/docs/en/sandboxing)
- [Security](https://code.claude.com/docs/en/security)
- [MCP](https://code.claude.com/docs/en/mcp)
- [Managed MCP configuration](https://code.claude.com/docs/en/managed-mcp)
- [Data usage](https://code.claude.com/docs/en/data-usage)
