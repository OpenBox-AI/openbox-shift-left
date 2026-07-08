# OpenBox Claude Code adapter (STORY-SL-4)

The first realization of the generic Provider Adapter Contract (architecture
§1b): it maps Claude Code's native hooks onto the normalized developer event
contract (STORY-SL-1) and emits them through the shared AIP-signed transport
(STORY-SL-3). **Observe-only, metadata-only, fail-open** — it can never block,
deny, or slow a Claude Code tool call (Phase-1 / INV-3 / D7).

```
Claude Code hook (stdin JSON)
   └─ openbox-cc-hook <HookName>           # the plugin wires each hook here
        ├─ map → normalized SL-1 DevEvent  # mapper.go (no content — INV-2)
        ├─ append → local spool            # spool.go (hot path: local I/O only)
        └─ exit 0, empty stdout            # can't block / inject (D7)
   SessionEnd / `flush`
        └─ drain spool → client.Emit → POST /api/v1/governance/evaluate  (off the hot path)
```

## Why a spool instead of emitting inline

Claude Code command hooks are synchronous — a slow hook delays the tool call.
Doing network I/O on `PreToolUse`/`PostToolUse` would blow the NFR-2 `<50 ms`
budget and couple the tool call to OpenBox reachability. So the hot path only
maps + appends one JSON line to a per-session spool file (local, sub-millisecond),
and delivery happens off the hot path at `SessionEnd` (bounded to 12 s) or via
the `flush` subcommand. Delivery is **at-most-once best-effort** (fail-open): a
hard outage loses telemetry, never a tool call. A flush cut short by its time
budget persists the **undelivered remainder** to a recovery file that the next
`flush`/`FlushAll` completes — the tail is not dropped, and delivered events are
never re-sent.

## Event mapping (SL-1 contract)

| Claude Code hook | SL-1 `event_type` | Span (`semantic_type`) |
|---|---|---|
| `SessionStart` | `SessionStarted` | — |
| `UserPromptSubmit` | `PromptSubmitted` | — |
| `PreToolUse` | `ToolCall` | by tool (`stage=started`) |
| `PostToolUse` | `ToolResult` | by tool (`stage=completed`) |
| `SessionEnd` | `SessionEnded` | — |

Tool classification (`classifyTool`): `Write`/`Edit`/`MultiEdit`/`NotebookEdit`
→ `file`/`file_write`; `Read`/`NotebookRead` → `file`/`file_read`; `Bash` →
`shell`/`internal`; `mcp__<server>__<tool>` → `mcp`/`mcp_tool_call`; everything
else (`Glob`, `Grep`, `WebFetch`, `Task`, …) → the coarse catch-all
`shell`/`internal`. The real tool name always rides on `tool.name` +
`metadata.tool_name`, so nothing is lost to the 3-value `kind` enum.

`semantic_type` is an **intent/hint**: openbox-core recomputes it server-side
from the span name + attributes, and the SL-3 client already sets the fields
core reads (file ops → `file.*` name + `file_path`; MCP → `mcp.method=callTool`).

## Privacy (INV-2 / SL3-SEC-3)

Metadata-only by default. The adapter carries **only structural** data — tool
identifiers, file paths, and lifecycle enums (`source`, `reason`,
`permission_mode`, `model`, `cwd`). It **never** copies prompt text, Bash command
strings, file contents, or tool output into an event — not into `content`, not
into `metadata`, not into `tool.name`. This is asserted end-to-end by
`TestMap_NoContentLeak` and the binary subprocess test.

## Known Phase-1 limitations (honest, no silent caps)

- **No tokens/cost.** Claude Code hooks do not expose token or cost usage
  (verified). `PromptSubmitted`/`SessionEnded` carry no finops fields. A Phase-2
  enhancement could parse `transcript_path` (content — privacy-gated); Phase-1
  does not even record the path. See `Capabilities()` → `telemetry.tokens=false`.
- **At-most-once delivery.** The client's `Emit` is fail-open and does not signal
  delivery success, so a delivered-then-lost event can't be retried without risk
  of a double-send. (The undelivered *remainder* of a budget-bounded flush IS
  preserved and re-drained.) True durable retry awaits a client success signal.
- **[EXT-core] not yet live.** The 7 developer `event_type` strings are not yet
  in core's accept-list, so a live POST returns HTTP 400 → a fail-open drop.
  Tests run against a fake `Emitter`; end-to-end delivery lands when EXT-core's
  3 additive edits ship (assumed-satisfied, OD14).

## Credentials (INV-1)

Identity is minted by `openbox dev init` (STORY-SL-2) and stored in the OS secret
store. The hook reads the **DID only** on the hot path (no secret I/O); the obx_
key + Ed25519 seed are read (secret store, or `OPENBOX_API_KEY`/
`OPENBOX_ED25519_SEED` for CI) only at flush and go straight into the client,
never logged/printed/argv'd. Non-secret coordinates live in a config file
(`OPENBOX_CONFIG`, default `~/.config/openbox/dev.json`) the installer writes.

## Packaging & install

`Installer` materializes the plugin bundle (`plugin/`) + writes the dev config.
The engine binary is built per-platform and placed at `bin/openbox-cc-hook`:

```bash
go build -o plugin/bin/openbox-cc-hook ./cmd/openbox-cc-hook
```

Org-wide force-enable via managed settings (`{"enabledPlugins":["openbox-observe"]}`)
is **verified, not activated** for the Phase-1 opt-in pilot (NFR-5).

## Integration follow-up (SL-4 ↔ SL-2 seam)

`Installer` here is the real installer for the `claude-code` provider. Wiring it
into the CLI's `provider` registry (replacing the SL-2 `stub`) is a one-line CLI
change: the `cli` module imports this adapter and registers a `provider.Installer`
that delegates to `claudecode.Installer`. It is deferred because the CLI's
`provider.Installer` interface lives under `cli/internal/` (not importable across
modules); moving it out is a `cli`-scoped edit tracked as **SL4-WIRE-1**.

## Test / validate

```bash
cd adapters/claude-code && go build ./... && go vet ./... && go test ./...
# 54 tests incl. SL-1 conformance of every emitted event + a real-binary
# observe-only end-to-end (exit 0, empty stdout, no content leak).
```
