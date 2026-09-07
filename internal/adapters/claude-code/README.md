# OpenBox Claude Code adapter

The first realization of the generic Provider Adapter Contract (architecture
§1b): it maps Claude Code's native hooks onto the normalized developer event
contract and emits them through the shared AIP-signed transport.

It has **two legs, and they are not the same posture.** The telemetry leg is
observe-only and fail-open: it spools and never affects the tool call. The
enforce leg gates a call synchronously and can deny it. Content capture is on by
default on both. `capabilities.go` is the authority on which is which — every
capability key carries what it does and what it deliberately does not.

```
Claude Code hook (stdin JSON)
   └─ openbox hook claude-code <event>     # the installer wires each hook here
        ├─ map → normalized DevEvent       # mapper.go
        ├─ append → local spool            # spool.go (hot path: local I/O only)
        └─ exit 0, empty stdout            # telemetry leg: no verdict to render
   gated call (PreToolUse / UserPromptSubmit / ConfigChange)
        └─ /evaluate → outputcontract.go   # enforce leg: renders deny/ask/block
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
event is retried on a later flush rather than dropped; up to
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

`localHookEvents` in `localhooks.go` is the one table of which hooks are
registered, with what timeout and matcher; `mapper.go` maps each to its
`event_type`. Both are iterated by tests, so a hook added to the table is
covered without a second list to update — which is why this document does not
carry a copy of either.

Reading it back: `grep -c '{Event: "' localhooks.go` for the registered count,
and `capabilities.go`'s `telemetry.hook` for why the ceiling is 32 of Claude
Code's 33 documented hooks (`WorktreeCreate` is deliberately not registered —
its stdout last line *is* the created worktree path).

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
[mapping.md](../../../docs/mapping.md) §3 for which `span` fields the client still
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

**The old posture — "commands, file bodies and tool output never egress on
observe events" — is retired.** It was an unconditional, structural guarantee.
What replaces it is a gate plus a redaction plus a cap: none of them structural,
and each one able to be got wrong. That is why they are asserted on the
**outbound bytes** (conformance C32–C38, plus C18/C26 for the ordering) rather
than on the mapper's return. `TestMap_NoContentLeak` still holds the
capture-OFF half.

## Known limitations (honest, no silent caps)

- **Token counts are per turn, not per model call.** Hooks fire per turn, so
  `Stop`/`SubagentStop` carry window sums rather than one row per call, and cost
  is never derived here. The numbers come from reading `transcript_path`, off
  the hot path. `capabilities.go`'s `telemetry.tokens` and `telemetry.model`
  state the bound exactly.
- **At-most-once delivery.** The client's `Emit` is fail-open and does not
  signal delivery success, so a delivered-then-lost event can't be retried
  without risk of a double-send. (The undelivered *remainder* of a
  budget-bounded flush IS preserved and re-drained.) True durable retry awaits a
  client success signal.
- **A new hook needs `openbox init` re-run.** Registration happens at install
  time, so a machine that upgrades this binary keeps observing its pre-upgrade
  hook set until a human re-runs `init`. Nothing schedules that.

## Credentials (INV-1)

Identity is minted by `openbox auth`. Secrets live in `~/.openbox/.env`, in
**plaintext** — `0600` on macOS and Linux, unprotected on Windows. Anything
running as the developer, the governed agent included, can read the signing key,
so attestation proves the origin of config rather than tamper resistance. An
earlier build kept these in the OS keychain; those entries are not migrated.

The hook reads the **DID only** on the hot path (no secret I/O). The obx_ key +
Ed25519 seed are read at flush (from `.env`, or `OPENBOX_API_KEY` /
`OPENBOX_ED25519_SEED` for CI) and go straight into the client, never
logged/printed/argv'd. Non-secret coordinates live in `~/.openbox/dev.json`
(`OPENBOX_CONFIG` overrides the path, `OPENBOX_HOME` the directory) — one store
per field, so a coordinate never has a second copy in `.env` to go stale.

## Packaging & install

`Installer` materializes the plugin bundle (`plugin/`) + writes the dev config,
and copies the unified engine into `bin/openbox` when
`Installer.EngineBinary` is set; `openbox init` sets it to its own executable.
The hooks invoke `${CLAUDE_PLUGIN_ROOT}/bin/openbox hook claude-code <event>`.
Packaging/marketplace builds place the per-platform binary instead:

```bash
go build -o plugin/bin/openbox ./cmd/openbox
```

The standalone `cmd/openbox-cc-hook` alias is **gone**. It was never built by
`.goreleaser.yaml`, so no release ever carried it, and nothing outside its own
tests invoked it; every installer names the engine
(`bin/openbox hook claude-code <event>`), which is the only entrypoint now.

Activation and mandate are two tiers, and only one of them is needed to be
governed. **Activation is self-serve**: `init` registers the hooks in
`~/.claude/settings.json`, so every session on this machine is governed at
once, with no administrator step and no restart — the tool's file watcher picks
the change up in sessions that are already running. **The mandate tier** is
managed settings with `allowManagedHooksOnly`, which makes governance
non-removable by the developer; it is enforcement, not activation.

The bundle under `~/.claude/plugins/openbox-observe` hosts the engine binary
and nothing else. It used to ship a plugin manifest and its own copy of every
handler; a plugin's handlers are separate from settings and do not
de-duplicate against them, so anything that loaded that directory doubled every
event against the registrations `init` writes.

`Installer` is reached through `internal/cli/providers`, which `openbox init`
delegates to. There is one Go module, so nothing about that wiring is deferred.

## Test / validate

```bash
go build ./internal/adapters/claude-code/... && go vet ./internal/adapters/claude-code/... && go test ./internal/adapters/claude-code/...
# covers event-contract conformance for every emitted event, plus a real-binary
# end-to-end for both legs: the telemetry path's empty stdout, and the enforce
# path's rendered verdict.
```
