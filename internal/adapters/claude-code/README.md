# OpenBox Claude Code adapter

The first realization of the generic Provider Adapter Contract (see
[architecture.md](../../../docs/architecture.md)): it maps Claude Code's native hooks onto the normalized developer event
contract and emits them through the shared AIP-signed transport.

It has **two legs, and they are not the same posture.** The telemetry leg is
observe-only: it spools and never renders a verdict, so it never denies the
current tool call. Delivery to core is single-attempt (plus one retry for a
transient failure): if an event this leg produced is never accepted, that
event is lost, but nothing about the run is stopped — the next hook event and
the next gated call each get their own fresh attempt. The enforce leg gates a
call synchronously and can deny it, unconditionally now (there is no
observe-only opt-out for a gated hook). Content capture is on by default on
both. `capabilities.go` is the authority on which is which — every capability
key carries what it does and what it deliberately does not.

```
Claude Code hook (stdin JSON)
   └─ openbox hook claude-code <event>     # the installer wires each hook here
        ├─ map → normalized DevEvent       # mapper.go
        ├─ append → local spool            # spool.go (hot path: local I/O only)
        └─ exit 0, empty stdout            # telemetry leg: no verdict to render
   gated call (PreToolUse / UserPromptSubmit / ConfigChange[user_settings])
        ├─ drain this session's own queued backlog, in slack   # gate.go: hooks/lanes interleaved
        └─ /evaluate → outputcontract.go   # enforce leg: renders deny/block, always
   SessionStart / SessionEnd
        └─ inline, in the hook itself: one drain attempt within its own window,
           falling back to the detached flusher only if that window runs out
   `flush` (spawned fallback, or explicit)
        └─ drain spool → client.Emit → POST /api/v3/governance/evaluate  (off the hot path)
```

## Why a spool instead of emitting inline

Claude Code command hooks are synchronous; a slow hook delays the tool call.
Doing network I/O on `PreToolUse`/`PostToolUse` would blow the `<50 ms` hot-path
budget and couple the tool call to OpenBox reachability. So the hot path only
maps + appends one JSON line to a per-session spool file (local,
sub-millisecond); delivery itself happens either **inline, in the hook that
just appended it** (`SessionStart`/`SessionEnd` only, each within its own
window measured from the hook process's own start: 4s window/3s attempt for
`SessionStart`, 12s/10s for `SessionEnd`, both strictly inside their installed
hook timeout of 5s/15s), or **off the hot path** via a detached `flush`
subcommand (60s attempt bound) that a `SessionStart`/`SessionEnd` inline
attempt spawns only when its own window runs out before every queued event
was attempted, or that a periodic `Sweeper` spawns for a session nothing else
has touched in a while.

Delivery is a bounded number of attempts, not best-effort: every event -- a
hook's own, or a lane daemon's, since `telemetry`/`transport` drain this same
per-session spool too -- gets one delivery attempt (plus one retry for a
timeout, network fault, or 5xx; a 401 gets one further retry with a
force-refreshed token), in append order, through one striped per-session
drainer. If core still does not accept it after that, the event is ledgered
and gone — that event alone is lost, but the run itself is not stopped: the
next gated call and the next hook event each get their own fresh attempt.
Only a real HALT verdict *from core* (as opposed to a delivery failure) stops
the run, by writing a run-scoped latch every later gated call in that run
checks. There is no recovery file or retry count beyond that: an
attempted-and-refused event is never resent, on any later flush. What a spool
file still holds between drains is only what has not been **attempted** yet: a
backlog with no further hook activity to trigger a drain, or a crash-orphaned
file waiting past its 5-minute reclaim window; a periodic sweep picks those
up, and a file untouched for 30 days is deleted with its count recorded
(`.discarded`), never silently. A cut-short drain pass is not a failure
either: whatever it could not get to stays queued, unchanged, for the next
drain.

Each event's `event_id` is derived deterministically from its structural fields
(`deriveID` in `mapper.go`): the same logical event always hashes to the same id
and two distinct events never collide, so the id is stable through the whole
spool → rotate → drain → reclaim lifecycle -- there is no separate
recovery-file stage any more; a crash-orphaned file is reclaimed by the same
drain path as an ordinary one. That is the client half of idempotency. The
server-side half is partial and lives outside this adapter -
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

**Content capture is ON by default.** One key, `content_capture`,
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
**outbound bytes** (conformance C32–C38, C40–C49 and C51–C56, plus C18/C26 for
the ordering) rather
than on the mapper's return. `TestMap_NoContentLeak` still holds the
capture-OFF half.

## Known limitations (honest, no silent caps)

- **Token counts are per turn, not per model call.** Hooks fire per turn, so
  `Stop`/`SubagentStop` carry window sums rather than one row per call, and cost
  is never derived here. The numbers come from reading `transcript_path`, off
  the hot path. `capabilities.go`'s `telemetry.tokens` and `telemetry.model`
  state the bound exactly.
- **At-most-once delivery per event, by design.** The client's `Emit` return
  (its error, classified by `client.FailureClass`) decides one attempt's
  outcome: an explicit non-acceptance, after its retry budget, denies that
  event alone rather than being retried further, because a durable retry
  risks a double-send core cannot always dedupe. It does not stop the run —
  only a real HALT verdict from core does that. (An **unattempted** remainder
  of a budget-bounded drain -- as opposed to an attempted-and-refused one --
  IS preserved and picked up by the next drain; that is not a retry of a
  failure, since nothing was attempted yet.)
- **A new hook needs `openbox init` re-run.** Registration happens at install
  time, so a machine that upgrades this binary keeps observing its pre-upgrade
  hook set until a human re-runs `init`. Nothing schedules that.

## Credentials (INV-1)

Identity is minted by `openbox init --provider claude-code` (a `keycloak_workload`
agent; RS256 client-assertion key). Secrets live in `~/.openbox/.env`, in
**plaintext** — `0600` on macOS and Linux, unprotected on Windows. Anything
running as the developer, the governed agent included, can read the signing key,
so attestation proves the origin of config rather than tamper resistance.

The hook reads the **DID only** on the hot path (no secret I/O). The obx_ key +
workload private key are read at flush (from `.env`, or `OPENBOX_API_KEY` /
`OPENBOX_WORKLOAD_PRIVATE_KEY` for CI) and go straight into the client, never
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
