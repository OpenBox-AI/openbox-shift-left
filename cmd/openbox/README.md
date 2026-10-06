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

## `auth` is org-level only; registration lives in `init`

`auth` connects this machine to an org — two URLs and the control token — and
registers no agent. Identity is per tool: `openbox init --provider <tool>` is
what registers that tool's `keycloak_workload` agent (`agent/create`) and
writes its runtime credentials. `agent/create` has no upsert, so a name
collision at `init` time is handled there (auto-suffix, or an offer to adopt an
existing agent's workload key if this machine still holds it) rather than by
`auth`.

The control-plane token is read only from `OPENBOX_CONTROL_TOKEN` and is never
a flag: a flag puts a credential in `argv`, where `ps` and shell history can
read it. What `auth`/`init` write is nonetheless a plaintext file, on purpose;
see [where credentials
live](../../docs/credentials-and-secrets.md#where-credentials-live).

## The hook argv

Nobody types `openbox hook`; a tool's hook registration does. The forms are:

| Argv | What it is |
|---|---|
| `openbox hook <provider> [--home <abs dir>] <event>` | One hook event for `<provider>` (`claude-code`, `codex` or `muse`); the tool's payload arrives on stdin. `--home` names the OpenBox home (what `OPENBOX_HOME` sets). Muse clears its hooks' environment, so `init` bakes the flag into every Muse handler whenever `OPENBOX_HOME` is set to a non-default home; it must be an absolute path. |
| `openbox hook <provider> [--home <abs dir>] --fail-closed <event>` | The deny-only successor a host that fails open runs when a gate handler fails (Muse's `onFailure`). Only a provider whose engine has a fail-closed form accepts it, today `muse`. It needs no config, identity or network, writes nothing under the OpenBox home, and exits 2 with one line on stderr for a gated event, so a failed gate is a denial rather than an allow. |

The fail-closed form is handled before any provider store is bound, which is why
it works on a machine whose home, config and identity are all broken.
`--home` and `--fail-closed` are parsed by `runHook`, not declared on a flag set,
so the CI step that cross-checks documented flags against declared ones does not
see them; `TestHookArgvIsDocumented` holds this table to the parser instead.

## Build & test

```bash
go build ./... && go vet ./... && go test -race -count=1 ./...
```
