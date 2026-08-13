# Verification — inline policy evaluation

Phase: [phase-08](../phase-08-verification.md) · Date: 2026-08-13 · Branch: `feat/inline-policy-evaluation`

## Verdict: **implemented and unit-verified; NOT verified against a live stack**

The operator waived the stack run (recorded in
[phase 1's finding](finding-260813-dedupe-and-ceilings.md)) and will verify
manually. Every claim below is split by evidence strength. **Nothing in the
"testbed" column has run.**

## What each claim actually rests on

| Claim | Evidence | Strength |
|---|---|---|
| Every gated class is evaluated inline; the verdict is applied | C14 drives the real `RunHook` against a live `/evaluate` stub and asserts an `Edit` is denied with 1 hit | **strong** — real hook path, real HTTP, not a mock decider |
| A previously-unescalated class reaches the server | same; plus `testbed/30-enforce.sh` §A2 | strong (unit) / **unrun** (testbed) |
| Core unreachable ⇒ `fail_closed` decides, both branches | C4/C6 (deny) and C2/C3 (proceed) at the binary level | **strong** |
| A fail-open call is RECORDED, not silently allowed | `testbed/30-enforce.sh` §C | **unrun** |
| Hook always writes a verdict before the provider's ceiling | `TestEnforceBudgetStaysUnderTheDeclaredCeiling`, per adapter, stated against the SPI declaration and the installed constant | **strong** for the arithmetic; the kill itself is not exercised |
| A secret in a Write body never reaches `/evaluate` | C18 asserts on the bytes POSTed, and that a redacted body WAS attached | **strong** |
| `content_capture:false` attaches nothing, any class | C19, four classes | **strong** |
| Local redaction still applies with core unreachable | `testbed/30-enforce.sh` §C | **unrun** |
| Exactly one `ActivityStarted` per gated call | client-side suppression is unit-tested incl. the timeout path; the stored-row count is `30-enforce.sh` §A2 | **unrun** for the count that matters |
| Posture carries decision provenance | `TestPostureReportsDecisionProvenance` + `30-enforce.sh` §D | strong (unit) / unrun (wire) |
| **A raw-rego org is enforced** — the headline fix | `testbed/30-enforce.sh` §A publishes a raw-rego deny through the backend and asserts the call is denied | **UNRUN — the headline claim of ADR-0017 is not empirically proven** |
| No tier vocabulary in user docs | CI gate, verified with a negative control (reintroducing "Tier-2" in `architecture.md` fails it) | **strong** |
| Windows | cross-compile only | **not verified** |
| Codex enforcement | ceiling read from source; no Codex session driven | **not verified at runtime** |

## What ran

- **All 11 modules: build, vet, `go test -race`** — green, repeatedly, through
  every phase.
- **Cross-compile** `GOOS=windows/amd64` and `GOOS=linux/arm64` — green.
- **`FuzzRedact`** — green. Note: it was defined in `regoparity_fuzz_test.go` and
  would have been deleted with the rego parser. It covers code that still runs on
  every gated call with a body, so it was moved to `secrets_fuzz_test.go`
  deliberately. The two rego-path fuzzers are gone with what they parsed.
- **The CI tier gate** — verified in both directions.

## What did NOT run

`testbed/run-all.sh`. No local OpenBox stack was reachable at any point: `docker ps`
showed only unrelated containers and all four endpoints (`:8086`, `:3000`, `:8181`,
`:3233`) refused connections.

The testbed **scripts are updated** for the new model and parse (`bash -n` clean),
but updated-and-unrun is not evidence. `30-enforce.sh` was substantially rewritten:

- §A now publishes a **raw-rego** policy through the backend and asserts the deny —
  the case that used to fail open locally, and the one that justifies this change.
- §A2 asserts a `Write` is decided by the server and stored exactly once.
- §C gained the fail-**open** branch and a redaction-survives-outage case.
- §D replaced the stale-bundle/`dev sync` section with posture provenance and an
  assertion that `dev sync` now fails loudly.

## Two defects found and fixed during the work

Both were found by tests, not by review, and both are the reason converting the
conformance suites was worth more than deleting them:

1. **A live data race** on the observe-copy delivery flag, reachable today on any
   Bash/MCP call whose escalation times out. Fixed standalone in `eb53827` before
   the plan proper began.
2. **A fail-closed org would have denied every gated call without ever asking.**
   `ApplyFailurePolicy` ran before the evaluation; once the local step stopped
   producing verdicts, it synthesized an immediate HALT which then read as
   "already tightened" and suppressed the round-trip. Caught by C1. Fixed in the
   deletion commit.

## Unresolved

1. **The raw-rego enforcement claim is unproven.** It is ADR-0017's headline
   argument and the strongest reason the change is worth its costs. `30-enforce.sh`
   §A is written to prove it and has not run.
2. **The stored `ActivityStarted` count under universal escalation is still
   unobserved** — the same gap phase 1 recorded. Client-side suppression is
   well-tested; what core actually stores is not.
3. **The lost-200 double-store window is open and irreducible client-side.** It now
   applies to every gated call rather than to shell and MCP only. Closing it needs
   server-side dedupe on developer events — a backend ask, not work this repo can
   do.
4. **Codex has no runtime evidence at all** for the enforce path under this change.
   Its ceiling is read from its own installer; no Codex session was driven.
5. **Approval identity for non-shell/MCP classes is invocation-scoped**, so an
   approval for a `Write` cannot be matched after a retry and the call is re-asked.
   Pinned by `TestUngatedClassesKeepInvocationScopedIdentity`. Fixing it means
   changing `activity_id`, which is this product's event identity — its own
   decision.
