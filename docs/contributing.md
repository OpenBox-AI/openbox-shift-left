# Contributing

How to build, test and change `openbox`. Read
[Architecture](architecture.md) first: it has the diagrams, the key terms and
the code layout. [`CLAUDE.md`](../CLAUDE.md) at the root lists the invariants
that are easiest to break by accident; it is written for AI coding agents but
is just as useful for people.

## Set up

You need Go 1.27 or newer (`GOTOOLCHAIN=auto` fetches it) and git. The repo is
one Go module. No OpenBox platform is needed: tests run the real hook binary
against an in-process fake.

```bash
git clone https://github.com/OpenBox-AI/openbox-shift-left.git
cd openbox-shift-left
go build ./... && go vet ./...
go build -o openbox ./cmd/openbox   # a local binary to try
```

## Test

```bash
go test -race -count=1 ./...                        # everything
go test -run TestGovernanceEval -v ./cmd/openbox/   # the end-to-end governance evals
GOOS=windows GOARCH=amd64 go build ./...            # the cross-compiles CI runs
GOOS=linux GOARCH=arm64 go build ./...
```

`-count=1` is required, not a speed setting. Some guards shell out to
`go list`, and the Go test cache does not see a child process's file reads, so
a cached run can report a stale pass.

Some hosts refuse to bind a local port, which makes `httptest.NewServer` panic
and kills the whole test binary. `internal/client/memhttptest` serves HTTP over
in-memory pipes for that case; prefer it in new tests.

When a claim is about what goes over the wire, test the bytes sent, not the Go
struct that produced them.

## What CI checks

The workflow is [`.github/workflows/ci.yml`](../.github/workflows/ci.yml).
Besides build, vet, tests, a vulnerability scan, a short fuzz run and the
cross-compiles, a few steps check the docs against the code. They are the ones
a first change most often trips:

| Check | What to do |
|---|---|
| Every top-level directory has a row in [Architecture § Layout](architecture.md#layout) | Add a row when you add a directory. |
| Every `openbox` flag shown in a copyable command in `README.md` or `docs/` exists | Keep command examples in sync with `cmd/openbox`. |
| The evidence table in [coverage.md](coverage.md#5-evidence-what-is-proven-and-by-what) cites tests that exist, and names every grader | Update the table when you rename a test or add a grader (`cmd/openbox/evidencetable_test.go`). |
| Docs never overstate bypass protection (a short list of banned phrases) | Bypass is detected, not prevented. Say it that way. |

## Conventions

- **Filenames.** Go files are flat lowercase with no separators
  (`approvalhold.go`); an underscore only where the toolchain reads it
  (`_test.go`, `_unix.go`). Test files may use underscores to name their
  subject. Non-Go files are kebab-case.
- **Dependencies.** Prefer a maintained package to hand-rolled code. The
  allowlists in `internal/depguard` are scoped by package subtree; widening
  one is a design decision, so say why in the PR.
- **Commits.** [Conventional commits](https://www.conventionalcommits.org/)
  (`fix(hookflow): …`, `docs: …`), one logical change each.
- **Secrets in fixtures.** The local secret redactor also runs on your own
  edits while you use a governed tool, so a secret-shaped literal can be
  rewritten on disk to `${OPENBOX_REDACTED_…}`. Build such fixtures in code
  (for example, derive a base64 string at runtime).

## Adding a coding tool

An adapter is four things, in `internal/adapters/<tool>/`:

1. its native hook payload types;
2. its mapper, from those payloads to the [event contract](dev-event-contract.md);
3. an `OutputContract`: how it writes a hook response, and what a
   require-approval verdict becomes;
4. its installer: how hooks and settings are written for that tool.

Anything tool-independent belongs in `internal/adapters/common/hookflow` or
`devconfig`, not in the adapter. Register the tool in `internal/provider`.
Most of the rest is driven by that list, but a few places still name each tool
on purpose; search `cmd/openbox/` for `laneCapable`, `printGovernedScope` and
the provider switches in `main.go`, `doctor.go` and `uninstall.go`, and read
each one. Then document the tool in [coverage.md](coverage.md) and add it to
the conformance suite (`internal/conformance`).

## Changing docs

`README.md` and `docs/` are for people using or changing `openbox`. Explain
why and where; point at code for what and how, rather than copying it. Keep
the claims honest: credentials are plaintext files, redaction is pattern-based,
and bypass is detected but not prevented.

## Sending a change

1. Branch, make the change, and run the build and test commands above.
2. Update the docs a user or contributor would read differently afterwards.
3. Open a pull request describing what changed and how you verified it.
