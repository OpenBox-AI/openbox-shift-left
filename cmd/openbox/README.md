# `openbox` CLI

`package main` for the shipped binary: command dispatch, flag parsing, and the
wiring that hands each command to the packages that do the work. Behaviour lives
under `internal/`; this package stays thin enough that a new command is a `case`
and a file.

`openbox help` is the command surface, and the only one — five commands a person
types, plus `hook`, `rewake`, `gateway`, `telemetry` and `transport`, which the
hook registrations and the service units invoke by string and nobody types. It
is not restated here, because a second copy goes stale the first time a flag
moves.

- What each command is for, and how to run them: [getting
  started](../../docs/getting-started.md).
- Which package owns what: [architecture §
  Layout](../../docs/architecture.md#layout).
- What the binary sends, stores, and can never take back: [data and
  privacy](../../docs/data-and-privacy.md).

## Why the seams in here look odd

Several indirections exist for testability rather than for the product, and
their absence fails in ways that are hard to attribute to this package:

- `installUnitFn`, `installLaneUnitFn`, `portOccupied` and the wait helpers are
  variables because `kardianos/service` ignores `$HOME`. Without the seam,
  `go test ./...` installs a real daemon on the developer's own machine and
  dials their real lanes. `refuseTheRealSupervisor` in `testmain_test.go` panics
  on each one, so a test that escapes the harness names which seam it escaped
  through instead of silently touching the host.
- `newPrompt` covers the terminal *requirement* and the prompter together,
  deliberately as one seam: a test that could supply answers but not get past
  `RequireTerminal` could not drive `auth` at all.
- `stdout`, `stderr`, `stdin` and `getenv` are fields rather than globals
  because what these commands print *is* most of what they do, and a test has to
  be able to assert it.

## Registration is not idempotent, and that shapes `auth`

The control plane shows the `obx_` API key and the Ed25519 seed exactly once,
and `agent/create` has no upsert. Local credential presence is therefore the
idempotency key: an agent that exists remotely but whose credentials were never
written here cannot be recovered at all. So `auth` refuses to duplicate a name
and states the cost rather than offering a flag past it — deleting that agent to
re-register mints a new DID and leaves the old lineage attached to the old one.

For the same reason the control-plane token is read only from
`OPENBOX_CONTROL_TOKEN` and is never a flag: a flag puts a credential in `argv`,
where `ps` and shell history can read it. What `auth` writes is nonetheless a
plaintext file, on purpose; see [where credentials
live](../../docs/credentials-and-secrets.md#where-credentials-live).

## Build & test

```bash
go build ./... && go vet ./... && go test -race -count=1 ./...
```
