# Phase 01 verification — Go 1.27 floor raise (D-GO-1)

**Date:** 2026-08-27 · **Host:** macOS 25.0.0 darwin/arm64, local toolchain
**go1.27.0** · **Branch:** `feat/tool-content-capture` · **Baseline:** `c704841`

Declaration-only change. Claim under test: floor moves 1.23.0 → 1.27.0 across
`go.work` + 12 modules with **zero behavioral diff**.

## Verdict

Implemented and verified to the limit this host allows. **540 tests pass and 19
fail with byte-identical names before and after the raise.** All 19 failures are
sandbox-caused (no process on this host may bind a TCP or unix listener), not
regressions. `govulncheck` went from the audit's "reachable stdlib vulns in all
12 modules" to **clean in all 12**.

Two phase requirements are **NOT verified** here and neither is verifiable
without a relaxed sandbox — see [Not verified](#not-verified).

## Step 0 — the four-release impact list, measured not read

Researcher-02 could only enumerate 1.27's changes and explicitly flagged
1.24/1.25/1.26 as an unverified gap. Rather than close it by reading, the
directive-gated surface was **measured**: one probe program built twice, once
under `go 1.23.0` and once under `go 1.27.0`, printing this repo's actual
dependencies. Source + captured output:
`scratchpad/dirprobe/{main.go,at-1.23.txt,at-1.27.txt}` (not committed).

Authoritative local source for the change list was
`/usr/local/go/doc/godebug.md` as shipped with go1.27.0 — the compat record with
every GODEBUG default and the release that changed it — cross-checked against
<https://go.dev/doc/go1.27>.

| Behavior this repo depends on | 1.23 → 1.27 | Consequence |
|---|---|---|
| `json.Marshal` on invalid UTF-8 → substitutes U+FFFD, returns nil error | **unchanged** | `adapters/claude-code/usage.go:175` and `adapters/common/hookflow/enforce.go:206` both document this as load-bearing. Go 1.27 re-implemented `encoding/json` on v2; v1 semantics are preserved through the v1 API, so the dependency survives |
| mid-rune split → U+FFFD, no error | **unchanged** | the `capBody` rune-boundary guarantee holds |
| duplicate JSON object keys → last wins, no error | **unchanged** | v2's stricter default applies to `encoding/json/v2`, not the v1 API |
| invalid UTF-8 on unmarshal → U+FFFD, no error | **unchanged** | — |
| Ed25519 seed → signature bytes | **byte-identical** | the AIP signing path and `activity_id` identity are untouched |
| `net/url.Parse` of unbracketed IPv6 / `host:1:2` | **now rejected** (`urlstrictcolons` 0→1, Go 1.26) | the only behavior that moves. See below |

**`urlstrictcolons` is the single change, and it has no regression path here.**
`http://::1/` previously parsed as host `::1` port `1`; it now errors.
Bracketed IPv6 (`http://[::1]:8138`) and IPv4 still parse. In this repo:
`gateway.DefaultAddr` is `127.0.0.1:8788` (IPv4); `gateway/config.go:56` returns
an error on a parse failure, so an unbracketed IPv6 upstream now fails at
startup with a legible message instead of misparsing — the safe direction;
`cli/internal/gatewaycheck` uses its own `hostPort()` helper, not `url.Parse`,
so the change does not reach the loopback decision at all; and
`check_test.go:309` already brackets `::1` deliberately. No test changed.

Not applicable, checked and empty: no `GOEXPERIMENT=` anywhere (an explicit
`systemcrypto`/`nosystemcrypto` is a hard error on 1.27); no `rand.Seed` call
(`randseednop`); no `tls.Config` literal anywhere (`tlssha1`, `tlsmlkem`,
`tlssecpmlkem` have no configured surface); every `ed25519.GenerateKey` call
passes `nil` or `crypto/rand.Reader`, so `cryptocustomrand=0` changes nothing.

Also newly relevant, and it bit during this phase: Go 1.27 added
`x509sslcertoverrideplatform`, which makes `SystemCertPool` honour
`SSL_CERT_FILE` on Darwin. It is go-directive gated — `govulncheck`, whose own
module predates 1.27, ignored the variable until `GODEBUG=x509sslcertoverrideplatform=1`
was set explicitly. Harmless here, but it is a live example of the gating.

## What changed

| Surface | Change |
|---|---|
| `go.work` | `go 1.23.0` → `go 1.27.0`; added `toolchain go1.27.0` (workspace only — open question 1's default) |
| 12 × `*/go.mod` | `go 1.23` / `go 1.23.0` → `go 1.27.0`, both spellings normalised |
| `cli/go.mod` | `x/term` v0.34.0 → **v0.45.0**, pin rationale deleted; `x/sys` v0.35.0 → v0.47.0 (x/term's own requirement, not drag) |
| `cli/go.sum` | superseded v0.34.0/v0.35.0 lines removed |
| `.github/workflows/{ci,release}.yml` | `"1.26"` → `"1.27"`; ci.yml's comment argued "the floor stays at 1.23 to hold the x/term pin" — both halves now false, so it was reconciled |
| `CLAUDE.md` | pin paragraph deleted, replaced by the 1.27.0 floor statement |
| `.goreleaser.yaml` | comment "all 11 modules" → 12 |
| `install.sh` | **`MIN_GO_MINOR=23` → 27** + 2 comments |
| `README.md`, `docs/getting-started.md` | "Go 1.23+" → "Go 1.27+" (3 places) |
| `docs/adr/ADR-0015` | superseded marker, **pin only**; reasoning left as written history |

`install.sh` and the two docs were **not in the phase's file list**. They are the
same class of claim as CLAUDE.md's paragraph, and `MIN_GO_MINOR` is not merely
cosmetic: it gates the source-build fallback, so a stale 23 admits a toolchain
whose own version no longer matches what the modules declare. To be accurate
about the severity — under the default `GOTOOLCHAIN=auto` a go1.23 toolchain
reading `go 1.27.0` fetches 1.27 and builds fine, so the hard failure needs
`GOTOOLCHAIN=local` or an unreachable proxy. The reason to fix it is that the
gate states a false minimum, which is enough on its own.

ADR-0015 got a marker rather than a rewrite — it is a decision record, and its
plaintext-credential and dependency-budget reasoning still stands. Phase 08's
ADR-0022 is the forward-looking record; the marker cites D-GO-1 by name so it
carries no dangling link.

No `.go` file was touched. `plans/**` left alone — stateful history.

## Evidence

Env note: this host cannot write `~/Library/Caches/go-build`, `$GOPATH/pkg/mod`
or `$GOPATH/pkg/sumdb`, and Go's TLS fails through the sandbox proxy. All runs
used redirected `GOPATH`/`GOCACHE`/`GOMODCACHE` plus
`SSL_CERT_FILE=/etc/ssl/cert.pem`. **Checksum verification was left fully on** —
redirecting the sumdb cache rather than disabling `GONOSUMDB`/`GOFLAGS`.

| Check | Result |
|---|---|
| `gofmt -l` | clean |
| `go vet` (12 module patterns) | clean — includes 1.27's new mandatory `stdversion` analyser |
| `go build` (workspace) | clean |
| `GOOS=windows GOARCH=amd64 go build` | clean |
| `GOOS=linux GOARCH=arm64 go build` | clean |
| **per-module `GOWORK=off go build ./...`** | **12/12 ok** — the only check exercising what the release path resolves |
| `govulncheck ./...` per module | **clean 12/12** |
| full suite, `-race` | no `DATA RACE`; same 6 modules red as without `-race` |
| **verdict-set diff, HEAD vs working tree** | **identical: 540 PASS / 19 FAIL / 1 SKIP, same names** |

The verdict-set diff is the phase's central criterion. Method: a detached
worktree at `HEAD` (go 1.23.0, x/term v0.34.0) and the working tree
(go 1.27.0, v0.45.0), each run `go test -v ./...` per module, verdict lines
extracted to a sorted `module⇥verdict⇥test` set, then `diff`ed. Scripts:
`scratchpad/{passset2,skiplist,suite}.sh`. **No test's expectations were
edited**, so the phase's stop-condition never fired.

A first pass compared only failure names and looked clean at 18 tests. That was
weak: a panic aborts its whole test binary, so tests after it never ran **in
either tree** and the comparison silently excluded them — only 435 of 1031
declared test functions produced a verdict. Re-run with `go test -skip` excluding
just the panicking listener tests, executed coverage rose to 560 verdicts and the
identity still held. Recorded because the first number was the misleading one.

## Not verified

1. **Masked credential input over a PTY.** The phase's security note requires it
   because `x/term` moved v0.34.0 → v0.45.0 and that package drives
   `term.IsTerminal` + `term.ReadPassword`. This host denies PTY allocation
   (`OSError: out of pty devices`), so the check could not run at all.

   **The note's premise turns out to be false in the pleasant direction, and it
   was measured rather than assumed.** `diff -rq` between the two versions in the
   module cache: exactly four files differ — `go.mod`, `go.sum`, `terminal.go`,
   `terminal_test.go`. `term.go`, `term_unix.go` and `term_windows.go` are
   **byte-identical**, and `term.go` is where both `IsTerminal` and
   `ReadPassword` are defined. The changes are confined to `term.Terminal`, the
   VT100 line-editor type; `cli/internal/prompt/prompt.go:28` is the repo's only
   x/term importer and uses `term.IsTerminal` and `term.ReadPassword` and nothing
   else. Underneath, x/sys v0.35.0 → v0.47.0 leaves `zsyscall_darwin_arm64.go`
   and `termios_unix.go` byte-identical, with the Windows/ioctl changes additive.
   So the binary links literally the same masked-input code.

   Also green: `cli/internal/prompt` unit tests, and both cross-compiles
   including windows/amd64 — the platform x/term exists for.

   **The PTY check is still outstanding**, and should run before this reaches
   `main` — not because meaningful risk remains, but because the report said it
   would, and a stated check that silently never runs is the failure this product
   exists to prevent.
2. **Everything requiring a listener or `launchctl`.** 19 tests fail and 6 test
   binaries panic-truncate for this reason, identically in both trees. Includes
   the enforce-path conformance cases (C1–C41) that reach a real `/evaluate` stub
   over HTTP, the whole gateway install/listen suite, and `gatewaycheck`.
   Unaffected by this phase.

   Correcting an earlier reading of this: the C-numbered cases live in
   `enforce_conformance_test.go` / `enforce_test.go` / `conformance_parity_test.go`
   and **do not import the `contracts/dev-event/conformance` module at all**. The
   module's only importers are `adapters/claude-code/conformance_test.go`,
   `adapters/codex/conformance_test.go` and
   `client/acceptancetest/vocabulary_test.go`, none of which needs a listener. So
   "C1–C41 pass unmodified" is a whole-suite regression tripwire that is
   insensitive to a validator swap, and phase 02's real acceptance surface does
   run on this host.
3. `api.anthropic.com` DNS is blocked here, so `TestNonLoopbackTargetIsFlagged`
   fails on resolution rather than on logic.

## Notes for later phases

- **The sandbox blocks the plan's verification spine.** No TCP/unix listener may
  bind; no PTY; no `launchctl`. Phases 02 (C1–C41), 04, 09, 11, 12 and 13 all
  rest on one of those. Module fetching *does* work with the env above, so the
  OSS-adoption phases are not blocked on dependencies — only on proof.
- **Finding, spun off, not fixed here:** `cli/go.mod` declares a direct require
  on the `decision` module with a comment explaining why, but **no file under
  `cli/` imports it** — ADR-0017 deleted the local evaluator that did.
  `go mod tidy` correctly demotes it to `// indirect`; that was reverted to keep
  this phase declaration-only.
- `toolchain go1.27.0` sits in `go.work` only, leaving member modules free
  (open question 1's stated default). Under `GOWORK=off` each module's own
  `go 1.27.0` directive governs, and `GOTOOLCHAIN=auto` fetches 1.27 for a
  contributor on an older toolchain.

## Unresolved questions

1. **Should the PTY masked-input check gate this phase?** It is the one required
   check that could not run. Options: run it manually on an unsandboxed shell
   before merge (recommended), or accept the cross-compile + unit evidence and
   defer to the testbed.
2. **Does the sandbox get relaxed?** Phase 02 turned out NOT to need it (see
   Not-verified §2). Phases 04/09/11/12/13 do, and cannot be verified at all
   without it. Owner has asked for the sandbox to be relaxed; as of this report
   loopback binds, PTY devices and `launchctl` are all still denied. A `git push`
   is the other half of the answer: CI runs the same suite on ubuntu, where
   listeners bind, so the 19 sandbox-blocked tests get real evidence there.
3. `x/sys` moved v0.35.0 → v0.47.0 as x/term's requirement. Recorded as expected
   closure rather than drag; flag if the release diff should be reviewed.
