# Cursor: execution model, behavior, and security enforcement

Status: research snapshot, 2026-08-17.
Evidence: official Cursor documentation and official product/security posts.
Cursor is closed source, so implementation details beyond those sources are not
asserted here.

## Scope and evidence boundary

Cursor has several agent surfaces: the editor agent, a CLI, background/cloud
agents, and enterprise administration. They do not have identical hooks,
permissions, or containment. A feature advertised for local editor sessions
must not be assumed for cloud agents, and vice versa. The documented contract
also needs installed-version and black-box validation before Shift Left calls a
surface governed.

## How it works

Cursor describes an agent as the composition of instructions, tools, and a
selected model. The host presents model-appropriate context and tools, executes
the model's requested actions, supplies results, and continues until the task
ends. This supports an iterative inspect/edit/run/revise workflow rather than a
single completion.

The built-in capability set includes codebase search and reading, edits, shell
commands, browser or image-related tools, and questions back to the user.
Checkpoints allow supported workspace changes to be restored. MCP adds external
tools. Subagents can delegate bounded work into separate context. See
[Agent overview](https://cursor.com/docs/agent/overview) and
[Subagents](https://cursor.com/docs/subagents).

### Inputs that shape behavior

- User and project rules instruct the model.
- Tool exposure determines what the host will let the model invoke directly.
- Editor or CLI permission configuration controls filesystem and shell actions.
- Hooks can observe, allow, deny, ask, or modify selected event data, depending
  on the event.
- MCP configuration supplies external tool schemas and execution paths.
- Team and enterprise policy can centrally distribute selected controls.

Rules are guidance. Permissions, hooks, sandboxing, and tool registration are
the host enforcement surfaces.

## Observable behavioral properties

- Cursor can perform multiple code and command operations in a single task.
- Local interactive use, non-interactive CLI use, and cloud execution can have
  different prompting and hook behavior.
- Checkpoints can recover supported file edits but cannot undo external side
  effects.
- Shell permission matching and file-glob rules are configuration semantics,
  not intent classifiers. Broad interpreters or scripts can hide many effects
  behind one allowed executable.
- MCP tool approval controls invocation, not the safety or integrity of the MCP
  server itself.
- A hook that observes an event after execution supplies evidence; it is not a
  preventive control.

## Security enforcement layers

| Layer | What it controls | Enforcement character | Important limit |
|---|---|---|---|
| Rules/instructions | Intended model behavior | Guidance | Untrusted project content and prompt injection can redirect the model. |
| Tool inventory | Directly callable capabilities | Host-enforced | Shell and broad MCP tools can subsume narrower capabilities. |
| CLI/editor permissions | Read, write, and shell actions | Host-enforced on covered operations | Interactive and non-interactive behavior differs; match patterns can be too broad. |
| Before hooks | Pending shell, MCP, file-read, prompt, and generic tool events | Host-enforced on emitted events | Failure is open by default unless `failClosed` is set; cloud coverage differs. |
| After/lifecycle hooks | Telemetry, response processing, and later context | Evidence/advisory | Cannot reverse completed external effects. |
| Agent sandbox | Filesystem/process constraints and configured network controls | OS/platform boundary | Effective roots, exclusions, platform backend, and escalation paths must be verified. |
| Team/enterprise controls | Centrally supplied rules, hooks, MCP policy, and audit posture | Administrative boundary | Availability depends on plan, surface, rollout, and endpoint configuration. |

### Permissions

The Cursor CLI documents Read, Write, and Shell permission categories in user
or project CLI configuration. File permissions use path patterns; shell rules
match command patterns with semantics that should be tested against the exact
CLI version. Interactive execution can ask before commands, while automation or
non-interactive modes can carry broader authority. See
[CLI permissions](https://docs.cursor.com/en/cli/reference/permissions) and
[Using the CLI](https://docs.cursor.com/en/cli/using).

### Hooks

Cursor hooks are spawned processes that exchange JSON over standard input and
output. The documented event set spans sessions, prompts, generic tools,
subagents, shell, MCP, files, compaction, stops, responses/thoughts, Tab, and
workspaces. Security-relevant examples include:

- `beforeShellExecution`, which can allow, deny, or ask;
- `beforeMCPExecution`, which can allow, deny, or ask;
- `beforeReadFile`, which receives file content and can block the read; and
- post-file or post-tool events for observation.

The default process-failure behavior is important: a hook crash, timeout,
non-2 nonzero exit, or invalid output generally fails open. Setting
`failClosed: true` changes such a hook failure into a block. This setting should
be treated as part of the control, not an optional hardening footnote.

Cloud hooks have a different capability profile. The current hook reference
documents managed project/team/enterprise hooks in cloud execution, but no
`beforeMCPExecution` or `afterMCPExecution` there yet, and hooks do not run in
some early read-only turns. See [Hooks](https://cursor.com/docs/hooks).

### Sandbox

Cursor's official sandbox architecture describes platform-specific isolation:
macOS Seatbelt, Linux Landlock/seccomp with an overlay-based filesystem design,
and Windows through WSL2. The effective policy incorporates workspace and
administrator configuration and `.cursorignore`-related restrictions. The
agent is told about sandbox constraints and can request escalation where the
product permits it.

This is defense in depth, not proof that every Cursor tool and agent surface is
inside one identical boundary. The exact platform, workspace mounts, network
policy, exclusions, and escalation behavior need direct observation. See
[Agent sandboxing](https://cursor.com/blog/agent-sandboxing).

### MCP and enterprise controls

Cursor supports local and remote MCP transports, authentication including
OAuth, tool approval, and enterprise server controls such as allow-lists and
network restrictions. An allow-list proves an administrator permitted an
identity/configuration; it does not prove the server's code, returned content,
or downstream data handling is safe. See [MCP](https://cursor.com/docs/mcp),
[Enterprise](https://cursor.com/blog/enterprise), and
[Security](https://cursor.com/security).

## Trust boundaries and failure cases

1. **Closed implementation.** Architecture must be described at the documented
   surface. Source-derived claims from Codex or DeepSeek Harness cannot fill
   Cursor gaps.
2. **Surface split.** Editor, CLI, and cloud agents need separate capability
   profiles. "Cursor supports a hook" is insufficient without the execution
   surface.
3. **Fail-open hooks.** A configured blocking hook can silently become a bypass
   on process failure unless `failClosed` is active and actually honored.
4. **Coverage gaps.** Cloud MCP hook gaps and early read-only turns mean a
   managed hook deployment is not equivalent to full event coverage.
5. **Rules versus controls.** Repository rules are writable project content and
   must not be represented as an organization enforcement boundary.
6. **Shell authority.** Allowing an interpreter, package manager, or shell can
   expose effects not apparent from a first-token policy.
7. **MCP.** Tool descriptions and results are untrusted inputs to the model;
   server execution and data destinations remain separate trust domains.
8. **Checkpoint limit.** Local file restoration does not recall a sent request,
   revoked credential, published artifact, or database mutation.

## Relevance to Shift Left

The current Shift Left repository still treats Cursor as an unbuilt adapter and
its manual note reduces the integration to a few hooks. Current Cursor
documentation exposes a much richer surface: blocking shell/MCP/read hooks,
generic and lifecycle events, subagent events, managed hooks, and an explicit
`failClosed` setting. A real adapter is now plausible.

It must be split by execution surface. A local editor adapter and a cloud-agent
adapter cannot share one unqualified capability claim. The first implementation
should pin a Cursor version, install only documented hooks, set `failClosed` on
blocking gates, and black-box test allow, deny, timeout, malformed output,
subagent lineage, and MCP coverage. Unsupported events should remain explicit
coverage gaps.

Cursor remains mainly a development host. Its development hooks do not govern
the deployed agentic project. The project evaluator can use Cursor config,
rules, MCP files, and source as discovery evidence, but runtime findings must be
based on the application's own reachable behavior and integration points.

## Primary sources

- [Agent overview](https://cursor.com/docs/agent/overview)
- [Hooks](https://cursor.com/docs/hooks)
- [MCP](https://cursor.com/docs/mcp)
- [Subagents](https://cursor.com/docs/subagents)
- [CLI permissions](https://docs.cursor.com/en/cli/reference/permissions)
- [Using the CLI](https://docs.cursor.com/en/cli/using)
- [Agent sandboxing](https://cursor.com/blog/agent-sandboxing)
- [Enterprise controls](https://cursor.com/blog/enterprise)
- [Cursor security](https://cursor.com/security)
