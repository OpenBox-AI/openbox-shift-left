# OpenBox Claude Code adapter

The first realization of the generic Provider Adapter Contract (architecture
§1b): it maps Claude Code's native hooks onto the normalized developer event
contract (story-SL-1) and emits them through the shared AIP-signed transport
(story-SL-3). **Observe-only, metadata-only, fail-open**; it can never block,
deny, or slow a Claude Code tool call (Phase-1 / INV-3 / D7).

```
Claude Code hook (stdin JSON)
   └─ openbox hook claude-code <event>     # the plugin wires each hook here
        ├─ map → normalized SL-1 DevEvent  # mapper.go (no content; INV-2)
        ├─ append → local spool            # spool.go (hot path: local I/O only)
        └─ exit 0, empty stdout            # can't block / inject (D7)
   SessionEnd / `flush`
        └─ drain spool → client.Emit → POST /api/v1/governance/evaluate  (off the hot path)
```

## Why a spool instead of emitting inline

Claude Code command hooks are synchronous; a slow hook delays the tool call.
Doing network I/O on `PreToolUse`/`PostToolUse` would blow the NFR-2 `<50 ms`
budget and couple the tool call to OpenBox reachability. So the hot path only
maps + appends one JSON line to a per-session spool file (local,
sub-millisecond), and delivery happens off the hot path at `SessionEnd` (bounded
to 12 s) or via the `flush` subcommand. Delivery is **best-effort and
fail-open**: an outage delays telemetry, never a tool call, and an undelivered
event is retried on a later flush rather than dropped (E8-S7); up to
`maxRecoveryAttempts`, after which loss becomes permanent. A flush cut short by
its time budget persists the **undelivered remainder** to a recovery file that
the next `SessionEnd` re-drains (`SweepRecovery`, or an explicit
`flush`/`FlushAll`); the tail is not dropped, and delivered events are never
re-sent. The sweep is not scoped to the ending session: a recovery file belongs
to a session that has already finished, so nothing else would ever retry it.

Each event's `event_id` is derived deterministically from its structural fields
(`deriveID` in `mapper.go`): the same logical event always hashes to the same id
and two distinct events never collide, so the id is stable through the whole
spool → rotate → flush → recovery lifecycle (INV-5). That is the client half of
idempotency. The server-side half is partial and lives outside this adapter -
[`client/README.md`](../../client/README.md) owns which events core deduplicates
and which it does not.

## Event mapping

| Claude Code hook | SL-1 `event_type` | Span (`semantic_type`) |
|---|---|---|
| `SessionStart` | `SessionStarted` |; |
| `UserPromptSubmit` | `PromptSubmitted` |; |
| `PreToolUse` | `ToolCall` | by tool (`stage=started`) |
| `PostToolUse` | `ToolResult` | by tool (`stage=completed`) |
| `SessionEnd` | `SessionEnded` |; |

Tool classification (`classifyTool`): `Write`/`Edit`/`MultiEdit`/`NotebookEdit`
→ `file`/`file_write`; `Read`/`NotebookRead` → `file`/`file_read`; `Bash` →
`shell`/`internal`; `mcp__<server>__<tool>` → `mcp`/`mcp_tool_call`; everything
else (`Glob`, `Grep`, `WebFetch`, `Task`, …) → the coarse catch-all
`shell`/`internal`. The real tool name always rides on `tool.name` +
`metadata.tool_name`, so nothing is lost to the 3-value `kind` enum.

`semantic_type` is now **adapter-local**. It used to be a hint core recomputed
server-side from the span it received; a tool event now carries no span, so
nothing classifies it and the field never reaches the wire. `tool.kind` is what
carries the distinction downstream. The mapper still sets `semantic_type`
because the adapter contract is frozen at schema v1.0; see
[MAPPING.md](../../../docs/MAPPING.md) §3 for which `span` fields the client still
reads and which are inert.

## Privacy (INV-2)

**Content capture is ON by default (2026-07-15).** One key, `content_capture`,
gates every content class this adapter binds:

| Class | Since | Redacted before attach? |
|---|---|---|
| prompt text (`UserPromptSubmit`) | v1.0 | yes |
| enforced-call body (`Write`/`Edit`) | v1.0 | yes |
| assistant reply (`Stop`/`SubagentStop`) | v1.2 | yes |
| tool input on the **observe** path | v1.3 | yes |
| tool output (`tool_response`), incl. a failed call's `error` | v1.3 | yes |
| **the turn's thinking** (`Stop`/`SubagentStop` transcript) | v1.4 | yes |
| refusal free text (`PermissionDenied.reason`, `StopFailure.error_details`) | v1.3 | yes |

Redaction here is local and keyword-driven, so it is a control with a measured
reach rather than a guarantee. See
[data and privacy](../../../docs/data-and-privacy.md).

**Opt out** with `content_capture:false` in `~/.openbox/dev.json` or
`OPENBOX_CONTENT_CAPTURE=0` to restore the metadata-only projection: tool
identifiers, file paths, and lifecycle enums (`source`, `reason`,
`permission_mode`, `model`, `cwd`), plus the ungated structural `status`.

**SL3-SEC-3 ("commands, file bodies and tool output never egress on observe
events") is retired** by. It was an unconditional guarantee; what replaces it is
a gate plus a redaction plus a cap, none of which is structural and each of
which can be got wrong. That is why they are asserted on the **outbound bytes**
- Conformance C32–C38, plus C18/C26 for the ordering; rather than on the
  mapper's return. `TestMap_NoContentLeak` still holds the capture-OFF half.

## Known Phase-1 limitations (honest, no silent caps)

- **No tokens/cost.** Claude Code hooks do not expose token or cost usage
  (verified). `PromptSubmitted`/`SessionEnded` carry no finops fields. A Phase-2
  enhancement could parse `transcript_path` (content; privacy-gated); Phase-1
  does not even record the path. See `Capabilities()` →
  `telemetry.tokens=false`.
- **At-most-once delivery.** The client's `Emit` is fail-open and does not
  signal delivery success, so a delivered-then-lost event can't be retried
  without risk of a double-send. (The undelivered *remainder* of a
  budget-bounded flush IS preserved and re-drained.) True durable retry awaits a
  client success signal.
- **[EXT-core] not yet live.** The 7 developer `event_type` strings are not yet
  in core's accept-list, so a live POST returns HTTP 400 → a fail-open drop.
  Tests run against a fake `Emitter`; end-to-end delivery lands when EXT-core's
  3 additive edits ship (assumed-satisfied, od14).

## Credentials (INV-1)

Identity is minted by `openbox init` (story-SL-2) and stored in the OS secret
store. The hook reads the **DID only** on the hot path (no secret I/O); the obx_
key + Ed25519 seed are read (secret store, or `OPENBOX_API_KEY`/
`OPENBOX_ED25519_SEED` for CI) only at flush and go straight into the client,
never logged/printed/argv'd. Non-secret coordinates live in a config file
(`OPENBOX_CONFIG`, default `~/.config/openbox/dev.json`) the installer writes.

## Packaging & install

`Installer` materializes the plugin bundle (`plugin/`) + writes the dev config,
and (story-SL4-wire-2) copies the unified engine into `bin/openbox` when
`Installer.EngineBinary` is set; `openbox init` sets it to its own executable.
The hooks invoke `${CLAUDE_PLUGIN_ROOT}/bin/openbox hook claude-code <event>`.
Packaging/marketplace builds place the per-platform binary instead:

```bash
go build -o plugin/bin/openbox ../../cmd/openbox
```

The standalone `cmd/openbox-cc-hook` alias is **gone**. It was never built by
`.goreleaser.yaml`, so no release ever carried it, and nothing outside its own
tests invoked it; the plugin manifest and every installer name the engine
(`bin/openbox hook claude-code <event>`), which is the only entrypoint now.

Org-wide force-enable via managed settings
(`{"enabledPlugins":["openbox-observe"]}`) is **verified, not activated** for
the Phase-1 opt-in pilot (NFR-5).

## Integration follow-up

`Installer` here is the real installer for the `claude-code` provider. Wiring it
into the CLI's `provider` registry (replacing the SL-2 `stub`) is a one-line CLI
change: the `cli` module imports this adapter and registers a
`provider.Installer` that delegates to `claudecode.Installer`. It is deferred
because the CLI's `provider.Installer` interface lives under `internal/cli/`
(not importable across modules); moving it out is a `cli`-scoped edit tracked as
**SL4-wire-1**.

## Test / validate

```bash
go build ./internal/adapters/claude-code/... && go vet ./internal/adapters/claude-code/... && go test ./internal/adapters/claude-code/...
# 54 tests incl. SL-1 conformance of every emitted event + a real-binary
# observe-only end-to-end (exit 0, empty stdout, no content leak).
```
