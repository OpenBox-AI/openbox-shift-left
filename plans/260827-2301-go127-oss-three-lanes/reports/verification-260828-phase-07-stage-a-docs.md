# Phase 07 verification — stage-A docs reconciliation

**Date:** 2026-08-28 · **Host:** macOS 25.0.0 darwin/arm64, go1.27.0 ·
**Branch:** `feat/tool-content-capture` · Docs-only; no code.

## Verdict

Done. Four markdown surfaces edited, 45 relative links verified resolving, zero
`.go` files touched, the six fully-runnable modules still green.

**Two of the phase's own requirement premises were wrong** and are reported rather
than worked around — see [Premises that did not hold](#premises-that-did-not-hold).

**The detection limit was RE-MEASURED, not inherited** (requirement 4), and the
measurement changed what the docs say in three places — including one where the
docs claimed LESS protection than exists.

## Requirement 4 — the re-measurement

Run against the current detector, after phase 06's floor restoration. Every fixture
assembled at runtime (see [A hazard worth knowing](#a-hazard-worth-knowing)).

| Input shape | Redacted? | By |
|---|---|---|
| AWS id / GitHub PAT / Stripe / JWT, bare | yes | our nine format regexes |
| GitLab / Shopify / Twilio / DigitalOcean, bare | yes | gitleaks (222 rules) |
| a recognised keyword ADJACENT to the delimiter (`OPENBOX_API` + `_KEY`, `api` + `_key`, `access` + `_key`) | yes | `secret_assignment` |
| `OPENBOX_AGENT_PRIVATE_KEY=<b64>` | yes | gitleaks `generic-api-key` |
| `UNKNOWN_NAME=<b64>`, nested `{"key":"<b64>"}` | yes | entropy pass |
| **`AWS_ACCESS_KEY_ID=<value in no known format>`** | **no** | — keyword not adjacent to the delimiter |
| `DEPLOY_HEX=<hex64>` | no | no keyword; hex caps at 4.0 bits, floor is 4.5 |
| bare b64 with no assignment | no | assignment gate |
| git SHA, UUID, `data:` URI | no | correct — the low-false-positive posture |

**The existing table in `data-and-privacy.md` was correct**; every row reproduced.
What changed:

1. **The format layer grew from 9 rules to 231** (9 ours + 222 gitleaks, both counts
   verified: `grep -c '{category: "' decision/secrets.go` = 10, one of which is the
   generic value-group pattern; `grep -cE '^id = '` on the embedded pack = 222). A
   new table row names the newly-covered families.
2. **A NEW limit is documented: keyword adjacency.** `access_key=…` is caught,
   `AWS_ACCESS_KEY_ID=…` is not, because `_ID` sits between the recognised keyword
   and the `=`. This is the gap that caused phase 06's regression, and it was
   undocumented.
3. **A NEW false-positive class is documented** — see below.

## The docs claimed less protection than exists

`data-and-privacy.md` said "**Only the prompt is exempt**" from the scanner. That
has been false since 2026-08-26: the prompt goes through the mapper's redactor
(`adapters/claude-code/mapper.go:225`) and conformance **C42** asserts it on the
outbound bytes (`adapters/claude-code/content_conformance_test.go:449`). Corrected,
with both citations — and with the part that IS still true kept: the same shape is
live for Codex, whose mapper has no redactor at all.

Understating is the mirror of the failure this repo's rule names, and it is worth
catching for the same reason: a reader plans around a limit that is not there.

## A hazard worth knowing

The enforce-path redactor **rewrote three of this repo's own test files** during this
work, twice via `keyword=value` in a source literal and once via the entropy pass on
`myConstant := "<48 chars of base64>"` — a value position by the detector's own
definition. Each time the fixture became a placeholder on disk and the measurement
silently read the wrong answer; the first version of this phase's own measurement was
contaminated exactly that way and reported six false negatives.

Now documented in `data-and-privacy.md` as a false-positive class, because on the
enforce path this rewrites a developer's file. Every fixture in this phase's probes
is assembled from parts below the 24-char entropy floor.

**It then happened to THIS REPORT.** A markdown table row above listing
`api_key=<hex64>` and `access_key=<plain>` was rewritten on disk into placeholders —
the document describing the hazard was edited by it. Fourth occurrence in this work,
and the first outside Go source: the redactor scans any content body, so prose about
credential syntax is subject to it too. The row is rebuilt from split literals.

## What changed, by surface

| Surface | Change |
|---|---|
| `docs/data-and-privacy.md` | detection mechanism (two layers, 231 rules); table gains a gitleaks-families row + the adjacency row; `OPENBOX_AGENT_PRIVATE_KEY` reason corrected to its measured cause; new limit 1b; new false-positive class; "only the prompt is exempt" corrected; heading "and the one gap" → "and where it stops" |
| `docs/architecture.md` | the enforce-triad's redaction bullet states the two layers; **two new assurance limits** — detection reach with both misses, and the credential guard's direct-requires scope |
| `contracts/dev-event/COVERAGE.md` §3.4 | the Claude-Code-vs-Codex redaction asymmetry now records that D-OSS-4 **widened** it: 231 formats vs none. C42 cited. Not smoothed away, per the phase's instruction |
| `CLAUDE.md` | the credential guard's scope stated for the first time (ADR-0023, direct requires only, and what is no longer bounded); `decision/guard_test.go` added to the allowlist-test list |

Already true from earlier phases, verified rather than re-done: the `x/term` pin
paragraph (phase 01), the dependency story (phases 01/03/06), the secret-detection
paragraph (phase 06 + its correction), the ADR index entry for ADR-0023 (phase 05),
`.goreleaser.yaml`'s 11 → 12 (phase 01).

## Dependency inventory, verified against go.mod

**1 → 7** distinct external direct dependencies, across 5 of 12 modules
(kardianos arrived with phase 04, after this phase ran):

| Module | Direct external |
|---|---|
| `cli` | `golang.org/x/term v0.45.0`, `github.com/google/renameio/v2 v2.0.2`, `github.com/kardianos/service v1.3.0` |
| `adapters/common/devconfig` | `github.com/joho/godotenv v1.5.1`, `github.com/pelletier/go-toml/v2 v2.4.3` |
| `adapters/common/hookflow` | `github.com/google/renameio/v2 v2.0.2` |
| `contracts/dev-event/conformance` | `github.com/santhosh-tekuri/jsonschema/v6 v6.0.3` |
| `decision` | `github.com/zricethezav/gitleaks/v8 v8.30.1` |

Plus ~78 distinct indirect modules, overwhelmingly gitleaks' tree (viper, afero,
fsnotify, mholt/archives, lipgloss, wazero, go-re2). `x/term` was the pre-stage-A
"one dependency".

## Premises that did not hold

Reported rather than satisfied by invention:

1. **Requirement 3 says "`COVERAGE.md`: dependency and detection-coverage rows
   updated." COVERAGE.md has no dependency section at all** — it is
   `contracts/dev-event/COVERAGE.md`, "Provider coverage & mapping rules", and
   carries no dependency claim to update (`grep -niE "dependenc|external|go\\.mod"`
   → nothing). The detection half was updated. Inventing a dependency section in a
   provider-coverage document would put the fact where nobody looks for it;
   `CLAUDE.md` already owns it.
2. **Requirements 1 and 5 say to "correct" the credential-guard scope sentence.
   No such sentence existed** in `CLAUDE.md` or `docs/architecture.md` — the guard's
   scope was never documented, before or after ADR-0023. Added to both rather than
   corrected.

## Evidence

| Check | Result |
|---|---|
| `.go` files touched by this phase | **none** — markdown only |
| relative links in the 4 edited docs | **45 checked, 0 broken** |
| ADR-0023 file + index entry | both present |
| live-tree `x/term` pin instruction | **none** — the only mention is ADR-0015's historical record, which carries a "Superseded … pin only" marker |
| `.goreleaser.yaml` module count | 12 |
| dependency inventory vs `go.mod` | exact, table above |
| detection boundary | re-measured, 20 shapes, table above |
| the 6 fully-runnable modules | green |

## Unresolved questions

1. **`docs/data-and-privacy.md` cites conformance
   `TestContentCaptureCredentialCoverage` as its evidence for the table.** That test
   is listener-dependent, so I could not run it; my re-measurement drove
   `decision.Redact` directly instead. The table's rows are verified at the detector,
   not end-to-end through a flushed event. An unsandboxed run would close that.
2. **The adjacency limit is documented, not fixed.** Widening `secret_assignment` to
   allow word characters between keyword and delimiter would catch
   `AWS_ACCESS_KEY_ID=<anything>` — a genuine improvement — but it adds
   false-positive surface (`token_endpoint=https://…`, `secret_name=my-configmap`) on
   a path that rewrites files. It deserves its own soak and decision, not a bundled
   change.
3. ~~**Phase 04 will need a small follow-up here**: kardianos in the dependency
   inventory, and the two-files-vs-one log-path deviation if branch (b) lands.~~
   **Closed 2026-08-28**: phase 04 landed on branch (b). `CLAUDE.md` now records
   `kardianos/service` (7 external direct dependencies, up from 1), the
   two-files-vs-one reason the unit body stays ours, and the three D-OSS-3
   consequences — path capture and its test seam, `Install()` refusing an existing
   unit, and `launchctl` start/stop staying ours.
