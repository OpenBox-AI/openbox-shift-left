# ADR-0011 — Keep the multi-module layout, with a workspace

Date: 2026-07-31
Status: Accepted
Context: the RF refactor, story RF-S10 (its plan is no longer in the repo)

## Context

The repo is split into eleven Go modules. The refactor review argued for
collapsing them into one, on solid grounds: every module has zero external
dependencies, none is published (all inter-module requires are `v0.0.0` plus a
relative `replace`), and the split therefore buys no dependency isolation while
costing a `replace` directive in every consumer — `cli/go.mod` carries eight.

Before the refactor there was no `go.work` either, so there was no command that
built or tested the repo as a whole, and no CI at all. That combination is what
made the split genuinely harmful.

## Decision

Keep the multi-module layout. Add the workspace and CI instead, and fold the one
module that earned nothing.

`go.work` gives CI, and any editor, a single view of every module; the CI job
discovers modules from it and fails if a `go.mod` on disk is missing from it, so
a new module cannot slip through unverified. The `replace` directives remain
authoritative for anyone building a module on its own.

`contracts/dev-event/acceptance` is folded into `client` as
`client/acceptancetest`. It exported nothing, was imported by nothing, and
existed only to host two tests — a module's worth of ceremony for a package's
worth of content.

`contracts/dev-event/conformance` stays a module and stays dependency-free, with
a test asserting its `go.mod` gains no `require` or `replace`. That property is
the point: adapters import it from their tests, and it must never pull anything
in.

## Why not collapse

Three reasons, in order of weight.

1. **GoReleaser builds from `cli`** with its own `replace` graph, and the
   release path is the one thing in this repo with no test coverage — an
   unreleased tag is how the last prebuilt-binary gap happened. Rewriting every
   import path underneath it, in the same change that also moved most of the
   adapter code, stacks two hard-to-review risks.
2. **The module boundary is doing real architectural work now.** `provider` and
   `devconfig` being separate modules is what makes the "adapters must not
   import each other, and the CLI must reach them only through the registry"
   rule mechanical rather than aspirational. A single module would leave that to
   convention, which is precisely the failure mode RF-S4 fixed.
3. The cost the review priced — no whole-repo build, no CI — is now paid by
   `go.work` and `ci.yml`, not by the layout.

## Consequences

- Adding a module means adding it to `go.work`; CI fails otherwise.
- A shared package still costs a `replace` in each consumer. That is the
  standing tax, and it is accepted deliberately rather than by inattention.
- If the release path ever gains real coverage, collapsing becomes a cheap
  follow-up and this ADR should be revisited.
