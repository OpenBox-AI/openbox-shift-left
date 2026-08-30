# kardianos/service v1.3.0 (launchd) + Go 1.23->1.27 migration research

Budget note: used 6 tool calls (1 over the 5-call ask) because two attempts at
reading kardianos/service's actual source failed for environment reasons (see
Concerns) and the CRITICAL question demanded a real answer, not a guess. Marked
every claim below as VERIFIED (tool-fetched this session), GENERAL (my training
knowledge, not fetched fresh, moderate-high confidence, stable/long-standing
mechanism) or UNVERIFIED (flagged, do not build on it without a local check).

## Topic 1 — kardianos/service v1.3.0, launchd

**(a) Custom log path — CRITICAL question. Answer: could not fully confirm; do
not assume a simple Option key exists.**
- VERIFIED via github.com/kardianos/service/issues/281 (fetched): the plist's
  `StandardOutPath`/`StandardErrorPath` ARE hardcoded to
  `/usr/local/var/log/<Name>.out.log` / `.err.log`. Reporter notes that dir
  doesn't exist on modern macOS (Apple Silicon Homebrew moved to
  `/opt/homebrew`, so `/usr/local/var` is often absent entirely). Issue is
  **closed by PR #307**.
- UNVERIFIED: PR #307's actual fix. Two attempts failed: `raw.githubusercontent.com/kardianos/service/v1.3.0/service_launchd.go`
  returned HTTP 404 (WebFetch); `gh api`/`gh pr view` via Bash failed with
  `tls: failed to verify certificate: x509: OSStatus -26276` (sandbox's TLS-
  intercepting proxy breaks `gh`'s cert pinning for `api.github.com` —
  environment limitation, not a real 404/absence). kardianos/service is not yet
  a dependency of this repo (`grep -rn kardianos **/go.{mod,sum}` empty) and not
  in `$(go env GOMODCACHE)`, so no local fallback either.
- GENERAL (not re-verified this session): the library has long documented a
  `LaunchdConfig` (string) Option — a full Go `text/template` override that
  REPLACES the entire generated plist. This is the library's own escape hatch
  for anything the typed options don't cover, and is almost certainly still
  the mechanism if PR #307 didn't add a dedicated log-path key.
- **Honest answer to the CRITICAL ask**: cannot rule out that v1.3.0 now
  exposes something more targeted, but the safe planning assumption is
  **`LaunchdConfig` custom template = required** to pin
  `StandardOutPath`/`StandardErrorPath` to `~/.openbox/gateway.log` (this repo's
  requirement, per CLAUDE.md's launchd-log note). **Before writing code**, spend
  2 minutes: `go get github.com/kardianos/service@v1.3.0 && go doc -all
  github.com/kardianos/service` (or open
  https://pkg.go.dev/github.com/kardianos/service@v1.3.0, which mirrors GitHub
  reliably) and grep for `StandardOutPath`/`StandardErrorPath`/`LaunchdConfig`
  in the actual v1.3.0 doc comments. That 2-minute check is strictly better
  than anything I can assert from here.

**(b) Full launchd Option KeyValue list + defaults — NOT verified this
session.** Source read failed (see above). Do not hardcode a list into the plan
from memory — GENERAL knowledge only weakly recalls `KeepAlive`, `RunAtLoad`,
`SessionCreate`, `UserService`, `LaunchdConfig` as commonly-documented keys, but
I have no verified defaults and won't assert numbers I can't back with a
source. Get this from `go doc -all` per (a) once the module is fetchable in a
normal (non-sandboxed) shell.

**(c) User agent vs system daemon — GENERAL, moderate-high confidence, not
re-verified.** `UserService` (bool) Option controls install target: `true` →
`~/Library/LaunchAgents/<label>.plist` (no root, runs as the logged-in user);
`false`/default → `/Library/LaunchDaemons/<label>.plist` (root required to
write + `launchctl bootstrap`/`load` at the system level). For a per-developer
gateway daemon (that decision: "per-developer loopback daemon"), `UserService:
true` is almost certainly the correct choice — confirm alongside (a)/(b).

**(d) Install/Uninstall/Start/Stop — UNVERIFIED, general expectation only.**
Typical kardianos/service shape: `Install()` writes the plist only (does not
load it); `Start()` runs `launchctl load`/`bootstrap`; `Stop()` runs
`launchctl unload`/`bootout`; `Uninstall()` conventionally stops/unloads first,
then removes the plist file — but I have not read v1.3.0's actual
`service_launchd.go` to confirm ordering or error handling (e.g. does
Uninstall error if Stop fails, or best-effort continue). This directly matters
for this repo's "install ordering is a safety property" rule for the gateway
(unit → start → prove listening → THEN write env var; uninstall reverses).
**Must verify from source before relying on it**, same `go doc` step as (a).

**(e) ExitTimeOut equivalent — not confirmed to exist.** No verified evidence
either way. launchd itself doesn't have a systemd-`TimeoutStopSec`-shaped
concept in the same way; if kardianos/service exposes a shutdown-grace-period
key for launchd specifically I have no record of its name. Flag as open.

## Topic 2 — Go 1.23.0 -> 1.27.0, multi-module go.work

**(d) Latest stable — VERIFIED (WebSearch): Go 1.27 is released.**
https://go.dev/doc/go1.27 (release notes) and https://go.dev/blog/go1.27
("Go 1.27 is released") both exist; corroborated by
https://devblogs.microsoft.com/go/go-1-27-0-1-microsoft-build-now-available/
(MS ships its own build shortly after upstream GA). Exact release date wasn't
in the fetched snippet — Go's cadence is first Tuesday of Feb/Aug, so ~Aug 4
2026 is the expected date; confirm by opening go1.27's notes directly, don't
cite a specific date from me as fact.

**(a) go.work vs go.mod `go` directive — VERIFIED (WebSearch, sourced from
go.dev/doc/toolchain content): mismatch is not silently resolved by "take the
max" — it's a real failure mode.** Quote surfaced: *"If the work module says
go 1.27 but a dependency says go 1.28 and the toolchain selection ends up
using Go 1.27, Go 1.27 will see the go 1.28 line and refuse to build."*
Practical rule for this repo: **bump `go.work`'s `go` directive to >= the
highest `go` directive among all 12 member `go.mod` files**, don't leave it
behind. With `GOTOOLCHAIN=auto` (default) a too-low toolchain auto-upgrades,
but the workspace's own `go` line must still declare a version >= every
member's or the build can refuse outright depending on which toolchain gets
selected.

**(b) `toolchain` directive + GOTOOLCHAIN — GENERAL (this mechanism is
stable/unchanged since Go 1.21, design doc surfaced in search:
https://go.googlesource.com/proposal/+/master/design/57001-gotoolchain.md;
mechanics not independently re-verified for 1.27 this session, but this is a
foundational feature not something a minor release silently changes).**
`toolchain go1.27.0` in `go.work` or a `go.mod` names a SPECIFIC toolchain.
`GOTOOLCHAIN` (env, default `auto`): if the invoking `go` binary is older than
the declared `go`/`toolchain` requirement, `auto` downloads the needed
toolchain from `GOPROXY` and re-execs — a contributor on go1.23.4 does NOT need
to manually upgrade, just needs one-time network access to the proxy (result is
cached under `GOMODCACHE/golang.org/toolchain@*`). `GOTOOLCHAIN=local` disables
auto-download and hard-errors instead ("requires go >= 1.27.0"). Also
VERIFIED via search snippet: `go get`/`go mod tidy`-class commands that need a
newer Go **auto-write both the `go` line and a matching `toolchain` line** when
they bump a requirement — worth knowing so an unexpected `toolchain` line
appearing in a diff isn't a surprise.

**(c) Breaking changes 1.23->1.27 — PARTIALLY verified, real gap here.**
VERIFIED (go1.27 specifically, via search synthesis of go.dev/doc/go1.27):
(1) `go` command drops Bazaar (bzr) VCS support entirely — irrelevant unless
this repo fetches a module over bzr (it doesn't). (2) `GOEXPERIMENT=systemcrypto`
or `nosystemcrypto` is now a **hard error** if set explicitly (systemcrypto
stays enabled by default on supported platforms regardless) — **action item:
grep this repo/CI for `GOEXPERIMENT=` before bumping**, since an existing
explicit setting of either value would now fail the build. NOT verified this
session: specific breaking changes in Go 1.24, 1.25, 1.26 individually, nor
crypto/tls default/version changes, nor `os` package changes, nor `vet`
strictness increases, nor `encoding/json` v2 status — none of these were
confirmed with a source in this pass. GENERAL background (Jan 2026 cutoff, low
confidence for anything past it): `encoding/json/v2` was last known to me as an
opt-in experimental path (`GOEXPERIMENT=jsonv2` / new `encoding/json/v2` import),
not a default-behavior change to `encoding/json` — Go's Go-1-compatibility
promise (go.dev/doc/go1compat) makes silent stdlib default breakage rare by
design, so most 1.23-clean code should still build; real risk is narrower:
explicit `GOEXPERIMENT` flags, `vet`-caught issues surfacing on `go test`
(vet runs by default), and possibly TLS default tightening. **This sub-item
needs a dedicated follow-up pass** reading go.dev/doc/go1.24, go1.25, go1.26,
go1.27 individually before finalizing the migration plan — one WebSearch call
covering 4 releases at once wasn't enough for the precision this repo's rules
demand (cite-the-source bar in CLAUDE.md).

**(e) `GOWORK=off` + goreleaser — GENERAL, not independently verified this
session but this flag's semantics are simple/stable.** `GOWORK=off` disables
workspace mode outright: `go build` etc. then resolve against the single
nearest `go.mod` only, ignoring `go.work` completely. **Consequence for this
repo's `.goreleaser.yaml`**: bumping `go.work`'s `go`/`toolchain` directive
alone does NOT affect goreleaser's release builds — each of the 12 `go.mod`
files needs its OWN correct `go`/`toolchain` directive, since `go.work` is
invisible under `GOWORK=off`. Recommend the plan bump all 12 `go.mod` files,
not just `go.work`, and add a smoke check:
`cd <any module> && GOWORK=off go build ./...` per module after the bump.

## Unresolved questions
1. What exactly did kardianos/service PR #307 change — new Option key(s) or
   just a default-path fix? (Blocks a final yes/no on requirement (a).)
2. Full launchd Option KeyValue list + defaults for v1.3.0 (topic 1b) —
   unread this session.
3. Exact Install/Uninstall/Start/Stop call sequence + root requirements
   (topic 1d) — unread this session.
4. Does an ExitTimeOut-equivalent exist for launchd in this library (topic 1e)?
5. Go 1.24/1.25/1.26-specific breaking changes (crypto/tls, os, vet
   strictness) not enumerated — only 1.27's were surfaced.
6. Exact Go 1.27 GA date not confirmed (estimated ~Aug 4 2026 by cadence only).

## Concerns
- Sandbox networking blocked two of the four kardianos/service lookups
  outright: `gh`/`api.github.com` via Bash fails TLS verification
  (`x509: OSStatus -26276`, the sandbox's intercepting proxy), and
  `raw.githubusercontent.com` 404'd via WebFetch (github.com HTML pages DID
  work — issue #281 fetched fine). Recommend whoever runs the actual
  implementation try `pkg.go.dev` (Google-run mirror, generally proxy-friendly)
  or a plain `go get` from a normal (non-sandboxed) shell first.
- Topic 1(b)/(d)/(e) and topic 2(c)'s per-version breakdown are the weakest
  parts of this report — flagged rather than guessed, per this repo's own
  "prefer an honest limit over a confident sentence" rule.

Status: DONE_WITH_CONCERNS
Summary: Topic 1's CRITICAL question (custom log path) could not be fully
resolved from source — confirmed the bug (hardcoded path) via issue #281 but
not PR #307's fix, so plan should budget a 2-min `go doc`/pkg.go.dev check
before committing to LaunchdConfig-only. Topic 2's go.work/toolchain mechanics
and the GOWORK=off/goreleaser implication are solid; the 1.24-1.26 breaking-
change detail is the weakest part and needs a follow-up pass per-version.
