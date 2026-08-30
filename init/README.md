# `init/` — supervisor units, for reading only

Every file here is an **illustrative copy**. Nothing in this directory is loaded,
installed, embedded, or consulted at runtime.

## The authority is `internal/cli/laneservice`

A unit that actually lands on a machine is **rendered at install time** with that
machine's binary path, its `HOME`, the address the developer chose, and a stop
timeout matched to the daemon's own shutdown-grace value. `kardianos/service`
owns the render; `internal/cli/laneservice` owns the `Spec` that drives it.

The copies here came out of that same renderer with placeholder values
(`/Users/dev`, the default addresses), so they show the **shape** and nothing
more. Read them to see what a unit looks like. Do not treat any value in them as
the value your machine will get.

## Why these are not embedded

Two silent failure modes live in these files: a plist that forgets
`StandardErrorPath` logs nowhere, and a stop timeout that stops matching the
daemon's shutdown grace gets the process SIGKILLed mid-drain on every restart.
Neither surfaces as an error.

An extracted `go:embed` copy would be a **second store of derivable state** with a
sync obligation, and the drift would be invisible in exactly those two ways. This
repository has already made that mistake once and reverted it: the lane election
was written as a stored field in `dev.json`, tested green, and backed out, because
a second copy of derivable state drifts silently in the worst direction.

So there is one renderer and one authority. If a copy here disagrees with what
`laneservice` produces, **the copy is wrong** — regenerate or delete it. Do not
edit it into agreement, and do not add a `go:embed` that reads it.

## What is here

| file | lane | platform |
|---|---|---|
| `ai.openbox.gateway.plist` | model-call gateway | launchd (macOS) |
| `ai.openbox.telemetry.plist` | OTLP receiver | launchd (macOS) |
| `ai.openbox.transport.plist` | in-path CONNECT relay | launchd (macOS) |
| `openbox-gateway.service` | model-call gateway | systemd (Linux) |
| `openbox-telemetry.service` | OTLP receiver | systemd (Linux) |
| `openbox-transport.service` | in-path CONNECT relay | systemd (Linux) |

Windows is refused rather than silently skipped — there is no unit to show.

## Installing a lane

Not from here. `openbox init --provider claude-code --full` installs all three in
proof order (write the unit, start it, prove it is listening, only then point the
tool at it); `--remove-all` reverses it. `openbox doctor` reports which lane is
routed, which can see a call, and — the failure that is otherwise invisible —
whether the elected lane is actually running.
