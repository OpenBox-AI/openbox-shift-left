---
title: "Porting golang-standards/project-layout into a 14-module workspace"
type: xia-analysis
mode: --improve
date: 2026-08-29
branch: feat/tool-content-capture
status: analysis complete — plan handed off
---

# Project layout, folder structure, file naming — source vs. this repo

## Source manifest

| Field | Value |
|---|---|
| Source | `golang-standards/project-layout` (local clone at `/tmp/project-layout`) |
| Remote | `git@github.com:golang-standards/project-layout.git` |
| Ref | `master` |
| Commit | `a9d6fae7015527b10550ebb6d6b71a71b1ef5ea7` (2026-04-29) |
| Scope | whole repo — a layout template, ~34 files, all `README.md` + `.keep` |
| Content treated as | untrusted data; structure and directory semantics extracted only |

## Source anatomy

The repo carries no executable code. It is 20 top-level directories, each holding a
`README.md` that says what belongs in it, plus `.keep` placeholders and three root
config files (`.editorconfig`, `.gitattributes`, `Makefile`).

Two statements in its own README bound how much of it should be adopted anywhere:

- **"This is `NOT an official standard defined by the core Go dev team`"** — it points
  instead to [Organizing a Go module](https://go.dev/doc/modules/layout) as the actual
  Go-team guidance.
- **"Clone the repository, keep what you need and delete everything else!"**

So wholesale adoption is disclaimed by the source itself. Its transferable idea is not
the directory list — it is *every directory states what belongs in it*.

## Dependency matrix — source component → local equivalent

| Source | Purpose | Local equivalent | Status |
|---|---|---|---|
| `/cmd` | main applications | `cli/cmd/openbox`, `adapters/claude-code/cmd/openbox-cc-hook`, `adapters/common/git/cmd/openbox-git-hook`, `actions/openbox-git-action/cmd/openbox-git-action` | **EXISTS** (per-module, correct) |
| `/internal` | private code | `cli/internal/*` (11 pkgs); 13 modules use the module root package | **EXISTS** (partial, by design) |
| `/pkg` | public libraries | none — modules serve the role | N/A (source itself calls it contested) |
| vendoring | vendored deps | none — go modules | N/A (correct) |
| `/api` | schemas, protocol defs | `contracts/dev-event/schema/dev-event.schema.json` | **EXISTS** (renamed; cohesive with the contract) |
| `/configs` | config templates | `deploy/managed/*` | **EXISTS** (renamed) |
| `/init` | systemd/launchd units | generated at runtime via `kardianos/service` + `cli/internal/gatewayservice` | N/A (programmatic, D-OSS-3) |
| `/scripts` | build/install scripts | `install.sh` (root), `testbed/*.sh` | **EXISTS** (distributed, cohesive) |
| `/build` | packaging + CI | `.goreleaser.yaml`, `.github/workflows/` | **EXISTS** (tool-mandated locations) |
| `/deployments` | deploy configs | `deploy/` | **EXISTS** (source accepts `/deploy` as an alias) |
| `/test` | external tests + data | `testbed/`, `*/testdata/` | **EXISTS** (renamed) |
| `/docs` | design + user docs | `docs/`, `docs/adr/` | **EXISTS** |
| `/tools`, `/examples`, `/third_party`, `/assets`, `/web`, `/website`, `/githooks` | — | none | N/A for a CLI + daemon product |
| `.editorconfig` | whitespace policy | **absent** | **NEW — adopt, adapted** |
| `.gitattributes` | line endings | **absent** | NEW — **reject** (see D7) |
| `Makefile` | entry point | absent (`go build`, `go test`, `testbed/run-all.sh`) | N/A |
| **README per directory** | **what belongs here** | 6 of 14 modules have one | **GAP — the portable idea** |

Every applicable source directory already exists locally under a different name. The
gap is documentation, not structure.

## What the local repo actually is

14 Go modules in a `go.work` workspace (`go.mod` on disk = 14, `use` entries = 14).
Module boundaries do architectural enforcement work: `provider` and `devconfig` being
separate modules is what makes "adapters must not import each other, and the CLI
reaches them only through the registry" mechanical rather than aspirational
(ADR-0011, `TestOnlyTheRegistryImportsAdapters`).

### Measured file-naming convention (not written down anywhere today)

A grep over every `*.md` in the repo returns **no** statement of a naming or layout
convention. One exists in practice and is near-uniform:

- **Go source: flat lowercase, no separators** — `approvalhold.go`, `outputcontract.go`,
  `turncursor.go`, `failurepolicy.go`, `initgateway.go`, `sessionhalt.go`. ~100 files.
- **Underscore reserved for build constraints** — `_test.go`, `_unix.go`, `_windows.go`.
  All 6 current `_GOOS.go` files *also* carry an explicit `//go:build` line, so the
  suffix is belt-and-braces rather than the sole constraint.
- **Non-Go: kebab-case** — `dev-event.schema.json`, `managed-settings.json`,
  `00-preflight.sh`, `data-and-privacy.md`, `ADR-0021-openbox-local-gateway.md`.
- **Exception: provider-mandated filenames kept verbatim** — `managed_config.toml`,
  `requirements.toml` (Codex reads those exact names; renaming breaks the provider).

**Three deviations**, all cosmetic and all compile-safe to rename:
`adapters/common/hookflow/consts_paths.go`, `adapters/claude-code/enforce_evaluate.go`,
`adapters/codex/enforce_evaluate.go`. (`plans/.../probe-server.go` sits inside a
stateful record and is out of scope.)

### Measured whitespace conventions

| Type | Measured | Note |
|---|---|---|
| `*.md` | **spaces**, 0 tabs across README / architecture / CLAUDE | source's `[*.md] indent_style = tab` is wrong here |
| `*.go`, `go.mod` | tabs | gofmt |
| `*.yml` | 2 spaces | |
| `*.sh` | **split** — `testbed/*.sh` tabs, `install.sh` spaces | no single rule fits; omit `.sh` |
| trailing whitespace | none in any `.go` / `.md` | already true; safe to codify |

## The three documentation defects this exercise found

1. **`telemetry/` and `transport/` appear in neither canonical layout map.** Not in
   `CLAUDE.md`'s "Where things live" table, not in `docs/architecture.md` §Modules.
   Two of fourteen modules are invisible in both places a reader would look — and they
   are the ADR-0022 lanes, the newest and least understood code in the repo.
2. **8 of 14 modules have no `README.md`**: `devconfig`, `hookflow`, `decision`,
   `gateway`, `provider`, `telemetry`, `transport`, `conformance`.
3. **One live stale count.** `CLAUDE.md:800` — "across `go.work` and all twelve
   modules" — is wrong; there are 14. The `11 modules green` lines at :216, :325, :402,
   :452, :487, :520, :572 are dated status records and must **not** be rewritten; same
   for ADR-0011's "eleven Go modules" Context line and `ci.yml:7`.

## Challenge

### C1 — Necessity: do we need the feature, or only the idea behind it?
- **Source:** 20 named top-level directories, single module.
- **Local:** every applicable one already exists under a local name. What is absent is
  the per-directory statement of intent.
- **Risk if wrong:** renaming `testbed/`→`test/`, `deploy/`→`deployments/`,
  `contracts/dev-event/schema/`→`api/` breaks every doc, CI and CLAUDE.md reference for
  zero behavioural gain. → **Adopt the idea, not the vocabulary.**

### C2 — Does adoption reverse an accepted decision? **(critical)**
- **Source:** single module; `/cmd`, `/internal`, `/pkg` at the repository root.
- **Local:** ADR-0011 (Accepted, 2026-07-31) considered exactly this and rejected it on
  three grounds, all of which still hold:
  1. GoReleaser builds from `cli` with its own `replace` graph and **the release path
     still has no test coverage** — "the testbed has NOT run" appears eight times in
     `CLAUDE.md`. Rewriting every import path underneath it stacks two hard-to-review
     risks.
  2. The module boundary is doing real architectural work, now additionally pinned by
     `TestOnlyTheRegistryImportsAdapters`.
  3. The cost the original review priced — no whole-repo build, no CI — is already paid
     by `go.work` + `ci.yml`.
- ADR-0011 names its own revisit condition: *"If the release path ever gains real
  coverage, collapsing becomes a cheap follow-up."* **That condition is not met.**
- **Risk if wrong:** import-path rewrite across 14 modules under an untested release
  path — a broken release, well over two days of rework.
  → **CRITICAL. Reject. Surface as an owner decision, do not act.**

### C3 — Is the source authoritative?
- **Source:** says of itself, "**`NOT an official standard defined by the core Go dev
  team`**", and directs readers to `go.dev/doc/modules/layout`.
- **Local:** already follows the official guidance — `cmd`/`internal` per module, no
  `src/`, modules rather than GOPATH.
- **Risk if wrong:** importing a contested pattern (`/pkg`) as though it were standard.
  → **Treat as a pattern catalogue, not a specification.**

### C4 — Existing overlap: would this create a third layout map that drifts?
- **Source:** one README per directory — N copies by construction.
- **Local:** already has two maps, and they have **already drifted** (defect 1). Adding
  8 verbatim READMEs would make three drift surfaces.
- **Risk if wrong:** the exact failure this repo keeps naming — "a check and a fix built
  on one classifier can still disagree". → **One canonical table
  (`docs/architecture.md` §Modules) plus a CI check that `go.work` matches it.** Module
  READMEs carry module-specific content only, never a restatement of the table.

### C5 — Maintenance burden: who owns the convention after the port?
- **Source:** community repo; nobody owns your copy.
- **Local:** a written convention with no enforcement rots — proven three times over in
  this very repo (2 undocumented modules, 1 stale count, 3 filename deviations, none of
  which any test or CI step would catch).
- **Risk if wrong:** we write a convention doc and it is stale within two ADRs.
  → **Every convention shipped here comes with a check, or is explicitly accepted as
  unenforced.**

### C6 — Dependency chain: what does this introduce?
- **Source:** nothing — docs and empty directories.
- **Local:** `.editorconfig` adds zero dependencies; a CI drift check is shell + grep.
  No module gains a `require`, so **no dependency-guard allowlist needs widening**
  (ADR-0023) and no `go mod tidy` cascade. → **Green.**

### C7 — Is there a naming convention to codify, or would we be inventing one?
- **Source:** specifies none; defers to `gofmt` / `staticcheck`.
- **Local:** measured across ~100 Go files and every non-Go asset — a near-uniform
  convention with exactly three deviations.
- **Risk if wrong:** codifying a rule the repo does not follow produces churn.
  Measurement says it does follow one. → **Codify what is already true.**

## Decision matrix

| # | Decision | Source's way | Our way | Hybrid | Risk | Choice |
|---|---|---|---|---|---|---|
| 1 | Module topology | single module, root `/cmd` `/internal` `/pkg` | 14 modules (ADR-0011) | — | **critical** | **local** — reject collapse; owner decision, revisit condition unmet |
| 2 | Directory vocabulary | `/test` `/deployments` `/api` `/configs` | `testbed/` `deploy/` `contracts/.../schema/` `deploy/managed/` | — | low | **local** — source accepts `/deploy`; renames are pure churn |
| 3 | `/pkg` | present | absent | — | low | **local** — source itself calls it contested |
| 4 | Per-directory README | every directory | 6 of 14 modules | README only where the module has no entry anywhere; module-specific content only | low | **hybrid** |
| 5 | Canonical layout map | N READMEs | 2 tables, both stale | one canonical table + CI drift check | low | **hybrid — the real improvement** |
| 6 | `.editorconfig` | present, `md`=tab | absent | adopt; `md`→spaces (measured), omit `.sh` (split) | low | **hybrid — adopt** |
| 7 | `.gitattributes` | `* -text` | absent | — | low | **reject** — a blanket opt-out is wrong for a repo shipping `.sh` and cross-compiling for Windows; `*.sh text eol=lf` would be the useful form, but line-ending policy is outside "layout" and belongs in its own change |
| 8 | File naming | unspecified | de-facto flat-lowercase / kebab | write it down; fix 3 deviations | low | **local — codify** |
| 9 | `internal/` per module | `/internal/app`, `/internal/pkg` | `cli/` only; guard tests elsewhere | — | low | **local — reject**; 13 modules of churn for enforcement already held by `guard_test.go` / `deps_test.go` / the registry test |

## Risk score

**Critical findings in the delivered plan: 0 → Low → Proceed.**

The single critical item (D1, collapsing the module topology) is **rejected and not
planned**. It is surfaced below as an owner decision, per this repo's rule that an audit
concern does not reverse an accepted decision without new evidence.

Residual risks in what *is* planned:

- Doc edits can themselves go stale → mitigated by the Phase 02 CI check.
- Renaming Go files is compile-safe (Go ignores filenames outside `_test` / `_GOOS`
  suffixes), but costs `git blame` continuity → Phase 06 is marked droppable.
- **Guardrail:** never rename a `_test.go` / `_unix.go` / `_windows.go` suffix.

## Owner decision surfaced, not taken

**Collapse the 14-module workspace into a single module?** ADR-0011 says no, and its
stated revisit condition — the release path gaining real test coverage — is still unmet
(`testbed/run-all.sh` has never run against a live stack). The source layout assumes a
single module, so full conformance is unreachable without reversing that ADR.
Recommendation: leave ADR-0011 standing; revisit if and when the testbed runs.

## Unresolved questions

1. Should `docs/architecture.md` §Modules or `CLAUDE.md` "Where things live" be the
   **canonical** table the CI check validates? The plan assumes `architecture.md`
   (user-facing, already cited); CLAUDE.md then points at it rather than duplicating.
2. Phase 06 (3 file renames) is cosmetic and costs `git blame` continuity on three
   files. Drop it?
3. A `.gitattributes` carrying `*.sh text eol=lf` is genuinely useful for a repo that
   cross-compiles for Windows, but is line-ending policy rather than layout. Separate
   change, or fold into Phase 05?
