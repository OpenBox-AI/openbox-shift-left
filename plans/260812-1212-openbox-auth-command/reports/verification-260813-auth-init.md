# Verification record — `openbox auth` + `openbox init`

**Date:** 2026-08-13 · **Host:** macOS (darwin/arm64), Go 1.23 · **Branch:**
`feat/dev-runtime-auth-and-init`

What this feature's claims rest on, split by the strength of the evidence. The
repo's rule is that reading code is not evidence and unit tests are not evidence
that a hook works, so the three columns are kept apart rather than blended into
"verified".

## Verified by automated test

| Claim | Evidence |
|---|---|
| dotenv parse/write: comments, `export`, both quote styles, whitespace, CRLF, duplicate-key error, base64 padding, real 32-byte seed round-trip | `devconfig/envfile_test.go` — table-driven, pure Go, so it holds on every OS |
| `.env` written 0600 under a 0700 parent, atomically, no temp file left behind | `envfile_test.go` (mode assertions skipped with a stated reason on Windows) |
| Writer preserves keys it did not author; refuses to overwrite a file it cannot parse | `envfile_test.go` |
| Migration is non-destructive, idempotent, never overwrites a newer file, surfaces an unreadable legacy file | `devconfig/migrate_test.go` |
| Read-side fallback to the legacy config location, so an upgraded binary is not silently ungoverned | `devconfig/paths_test.go` |
| `OPENBOX_HOME` must be absolute; `Home()` creates nothing on a read | `paths_test.go` |
| Secrets resolve env → `.env`; coordinates resolve env → `dev.json` → default | `devconfig_test.go` |
| **A DID in `.env` is ignored** (the two-store tripwire) | `TestEnvFileIsNotACoordinateSource` |
| A `.env` alone is insufficient — fails with the no-DID error | `TestCredentialFileAloneIsNotSufficient` |
| Both deprecated key names still read, from env and from file, warning once to stderr | `TestResolveCredentials_DeprecatedPrivateKeyNames` |
| Missing-credential error names `openbox auth` and the keychain read commands | `TestResolveCredentials_MissingSecret` |
| Prompter: masking, blank-keeps-current, CRLF trimming, non-TTY fail-fast, no secret echoed | `cli/internal/prompt/prompt_test.go` |
| Blank agent id short-circuits the DID and both secret prompts | `TestBlankAgentIDShortCircuitsTheCredentialPrompts` (asserts on prompts *not* shown) |
| Prompt order is the documented order | same test, via `Scripted.Prompts` |
| `obx_key_` rejected in the api-key field, without echoing the credential | `TestOrgKeyInTheAPIKeyFieldIsRejected` |
| Signing key validated as base64 of exactly 32 bytes; DID shape validated | `TestPrivateKeyValidation`, `TestDIDShapeValidation` |
| **`auth` writes no posture** — every posture field byte-identical before/after | `TestAuthNeverTouchesPosture` |
| Secrets and coordinates land in different files, neither leaking into the other | `TestSecretsAndCoordinatesGoToDifferentFiles` |
| A second `auth` run overwrites the first (the update `init` could never do) | `TestSecondRunOverwritesTheFirst` |
| Env-shadow warning names the right file per field | `TestEnvShadowWarningNamesTheRightFile` |
| Summary masks both secrets; the fingerprint is of the derived **public** key | `TestSummaryMasksSecrets`, `TestFingerprintIsOfThePublicKeyNotTheSeed` |
| No flag accepts a secret value (INV-1), asserted against the source | `TestNoAuthFlagTakesASecretValue` |
| stdin automation path; short read fails; swapped values caught | `TestStdinAutomationPath`, `TestStdinShortReadFails`, `TestStdinWrongOrderIsCaught` |
| `auth` registers via `devinit.Register` and installs nothing | `TestRegisterWritesCredentialsButInstallsNothing` |
| Rotation: both endpoints, key-then-identity order, DID preserved | `TestRotateWritesNewCredentialsAndPreservesTheDID` |
| Rotation writes **nothing** on an unusable reply; says the key is already invalid | `TestRotateWritesNothingWhenTheIdentityReplyIsUnusable`, `TestRotateSaysTheKeyIsAlreadyInvalidWhenIdentityFails` |
| Rotation refuses a changed DID and an org key in the runtime slot | `TestRotateRefusesAChangedDID`, `TestRotateRefusesAnOrgKeyInTheRuntimeSlot` |
| No implicit rotation — without `--rotate`, no rotate endpoint is called | `TestNoRotateFlagCallsNoRotateEndpoint` |
| `X-API-Key` vs `Bearer` selection | `TestRotateUsesXAPIKeyForAnOrgKeyAndBearerOtherwise` |
| The two 404s do not read alike | `TestTheTwo404sDoNotReadAlike` |
| **`--enforce=false` round-trips**: present-and-false in the file, still false on re-read | `TestEnforceOptOutRoundTrips` |
| A bare `init` writes an enforcing posture | `TestBareInitWritesAnEnforcingPosture` |
| Turning enforce off is announced even from an absent field | `TestTurningEnforceOffIsAnnouncedEvenFromAnAbsentField` |
| Each of the 7 moved flags errors naming `auth`; removed flags error explaining why | `TestEveryMovedFlagErrorsNamingAuth`, `TestRemovedFlagsError` |
| `init` with no credentials refuses and installs nothing | `TestInitWithoutCredentialsRefusesAndInstallsNothing` |
| `init` never reaches the registrar | `TestInitDoesNotRegisterEvenWithAnOrgKey` |
| Codex rejects `--scope local`; a bare codex install resolves global and says so | `TestCodexRejectsLocalScope`, `TestCodexBareInitResolvesGlobalAndSaysSo` |
| `--scope global` touches no project file | `TestInit_SaysWhichSessionsAreGoverned/global…` |
| `printGovernedScope` states the coverage gap, and **no install output claims ambient coverage** | `TestPrintGovernedScopeStatesTheTruth`, `TestNoInstallOutputClaimsAmbientCoverageAfterAProjectScopedInstall` |
| `--local-hooks` still works, warns once, to stderr only | `TestLocalHooksIsADeprecatedAliasForScopeLocal` |
| `CredentialRef` carries no credential-shaped field | `TestCredentialRefCarriesOnlySafeFields` (walks the struct) |

All 11 modules: `go build`, `go vet`, `go test -race` green on macOS/arm64.

## Verified by running the real binary on this host

Driven over a real PTY (`pty.fork`), against temp `OPENBOX_HOME` directories.

| Claim | Observed |
|---|---|
| Interactive register path is 4 prompts, then stops | `Organization`, `Backend URL`, `Core URL`, `Agent id` — then the org-credential error. No DID, api-key or signing-key prompt. |
| Both URLs prefill with the hosted defaults | `[https://api.openbox.ai]`, `[https://core.openbox.ai]` |
| Non-register path prompts DID + both secrets, **masked** | nothing echoed after `API key (obx_…):` |
| Summary masks correctly | `obx_live…a91f (27 chars)`, `SHA256:E545QOZL… (public key)` |
| Confirm gate defaults to No | `Write these credentials? [y/N]` |
| The two files, on disk | `.env` is `-rw-------`, holds only the two secrets + header; `dev.json` holds only coordinates. Cross-contamination greps clean both ways. |
| Migration is non-destructive **against a real legacy config** | picked up the host's actual `~/Library/Application Support/openbox/dev.json`, copied it, left the original byte-identical |
| `init` at default scope | governs the cwd, names it, states the gap, no "ambient" claim |
| `init --help` | 25 lines, 7 flags |
| `--secret-backend` | errors naming `openbox auth` |
| `approve list` reads the control token from `.env` | reached the network (401 from a fake URL = the token was read and sent) |
| Missing-credential error | names both `openbox auth` and the `security find-generic-password` escape hatch |

## Verified by CI configuration (and proven to bite)

`GOOS=windows GOARCH=amd64 go build` and `GOOS=linux GOARCH=arm64 go build` over all
11 modules, added to `.github/workflows/ci.yml`.

Both pass. More importantly the Windows step was **proven to catch its regression
class** rather than assumed to: temporarily adding `syscall.Umask` to `devconfig`
produced `undefined: syscall.Umask` on the Windows target, and removing it restored
green. A cross-compile step nobody has watched fail is decoration.

## NOT verified — and why

| Claim | Status |
|---|---|
| **The testbed suite** — `auth` → `init` → real session → events arrive; the negative ungoverned-directory assertion | **NOT RUN.** No local OpenBox stack was reachable from this host: `localhost:3000` and `localhost:8086` both refused, and no OpenBox containers were running. The scripts are updated and parse (`bash -n` clean), but they have not executed. |
| Windows at runtime | **Build-verified only.** No automated suite runs there; `install.sh` is bash. |
| `--scope global` activation | **Not verifiable here at all** — it needs a managed-settings deployment in a real fleet. |
| Linux at runtime | Not exercised on this host. The dotenv codec and resolution are pure Go and unit-tested, so those hold; hooks and the session flow are not. |
| Real rotation against the live backend | Only against `httptest`. The DTO-drift guard (`privateKey` absent from `AgentIdentityResponseDto`) is exactly the thing a live run would settle. |
| Real registration against the live backend | Only against a fake registrar and an `httptest` server. |

**The feature is therefore NOT end-to-end verified.** Everything above the line is
real; the live-stack column is empty and should not be filled in from the fact that
the unit tests are green.

## Found by running it, after the code was already green

Three defects that every test passed over. Recorded because they are the argument
for driving the binary rather than trusting a green suite.

1. **The install output contradicted itself.** The closing block said "Governance is
   ambient from here" three lines above "THIS PROJECT ONLY". Both halves were true —
   the *mechanism* is ambient, the *coverage* is one directory — but the wrong half
   is the one a hurried reader believes, and it is exactly the overstatement this
   plan's risk table names. The test asserted on `printGovernedScope` alone, which is
   a different function, so it passed. Fixed, and the assertion widened to the whole
   install output.

2. **The testbed's isolation was already broken on macOS, and this work would have
   made it worse.** `env.sh` claimed that redirecting `XDG_CONFIG_HOME` kept every
   write inside `testbed/.state`. Go's `os.UserConfigDir()` returns
   `$HOME/Library/Application Support` on darwin and **never consults
   `XDG_CONFIG_HOME`** — so on a Mac the spool, bundle, enforcement log, advisories,
   findings cursor, pending approvals, stale markers and session registry all
   resolved to the developer's REAL config directory. Discovered the honest way: a
   probe hook run during this verification wrote into
   `~/Library/Application Support/openbox/` (cleaned up afterwards). Every one of
   those paths has an explicit env override, so `env.sh` now pins all of them, and
   the five phases that derived the same paths independently were repointed at the
   exported variables.

3. **Two testbed paths broke on `approver.json` moving.** `approvals-auto.jsonl`
   derives from the approver config's directory (`approveauto.go:101`), which moved
   to `OPENBOX_HOME`; `99-teardown.sh` cleaned pending approvals from the old
   location. Both would have failed or silently cleaned nothing on the first live
   run — the kind of break only a run finds.

A doc claim was wrong for the same reason as (2): `README.md` named
`~/.config/openbox/` as the runtime-state directory, which is the Linux path only.
Both it and `docs/data-and-privacy.md` now name all three, and state that
`OPENBOX_HOME` does not relocate them.

## Code review — findings and dispositions

A `code-reviewer` pass over the whole change set. Every finding below was
independently reproduced before being fixed; the two regression tests added for the
criticals were then **proven to bite** by reverting each fix and watching them fail.

### Fixed

| Sev | Finding | Fix |
|---|---|---|
| **Critical** | **A plain `init` re-run silently reverted `--enforce=false` to true.** `--enforce` defaults to *true*, so its value alone cannot distinguish "asked to enforce" from "said nothing" — and `o.Enforce` was assigned unconditionally, so every run wrote `enforce:true`. Reproduced on the real binary. This reintroduced the exact bug class that decision calls load-bearing, one layer up (flag parsing rather than JSON marshaling), and `WouldDowngradeEnforce` cannot catch it because it is only ever handed `&true`. | `o.Enforce` stays **nil** unless `--enforce`/`--no-enforce` was actually passed (`flagPassed`, which existed for exactly this and was only used for the mutual-exclusion check). Nil is what makes the default work in both directions: absent ⇒ resolver default (on) on a first install; absent ⇒ tri-state merge preserves the user's choice on a re-run. |
| **Critical** | **The reuse-path message misreported whose credentials it found.** `.env` holds one key pair with no namespace, but the message interpolated the *current* run's `--org`/`--provider` — so `auth --provider codex --org beta` on a machine holding acme/claude-code credentials printed "Already initialized for org=beta provider=codex" and returned without a network call. The deleted keychain namespaced by `<org>/<provider>`, so the old message was accurate; that decision's single file made it a false statement about identity. | The message now says only what it can know: this machine has credentials, reusing them, nothing registered — plus that a machine holds ONE identity and how to see or replace it. The one-identity limit is now disclosed rather than implied. |
| **High** | **Unquoted engine path in the local-hook command.** A `$HOME` containing a space — an ordinary account named for a person — produced a command the shell splits, silently breaking every hook in the project. Latent while project scope was an opt-in testing flag; **live** now that that decision made it the default. The plugin bundle and the Codex installer both quote; this was the one path that did not, and `TestLocalHooksMirrorPluginBundle` compares hook *lists*, not command strings. | Quoted, via one helper. **And** `hasLocalHookCommand` made quote-insensitive — without that, a re-init would not have recognised entries written by earlier versions and would have appended a duplicate handler, firing every hook twice. |
| **High** | **`--env-file` ignored on the register path.** It reached the direct-write and rotate paths but not registration — the one path that *mints* credentials. So `auth --env-file /custom` with a blank agent id created a real agent and wrote its once-shown key to the default location while reporting the custom one. Unrecoverable by definition. | Threaded through `devinit.Options.EnvFile`; tested. |
| **High** | **Installers resolved the dev.json *write* target through the read path.** `DefaultConfigPath()` prefers an existing *legacy* file over a not-yet-created new one, so with migration failed (explicitly non-fatal) an install would write into the pre-that decision directory. Worked only by coincidence of ordering. | Both installers use `DevConfigWritePath()`. |
| **High** | **Apostrophe round-trip corruption in the dotenv codec.** The writer escaped an embedded `'` shell-style (`'"'"'`); the reader strips one outer quote pair and unescapes nothing. `it's` read back as `it'"'"'s`, silently, and every subsequent write re-serializes *every* key, so one hand-added apostrophe got mangled further each run. | The write **refuses** a value containing a quote or newline, naming the key. Neither secret can contain one, so this only fires on a human-added key — and for a file holding the only copy of credentials, a clear error beats silent corruption. Supporting it properly needs an escaping scheme on both sides, which the format does not warrant. |
| **Medium** | **Piped secrets + no `--yes` could not work.** `readStdinSecrets` drains stdin through a `bufio.Scanner`, whose buffering consumes the trailing confirmation line before the prompt's own reader sees it — surfacing as "unexpected end of input" instead of a question. Failed safe, but confusing. | Refused up front with the fix named (`--yes`), which is what automation wants anyway. |
| **Medium** | **`--env-file` accepted a relative path.** Unlike `OPENBOX_HOME`, which rejects one precisely because a hook's working directory is whatever project it runs in. `auth --env-file creds.env` inside a repo would drop a plaintext API key and signing key into the source tree. | Refused, with the reason. |
| **Medium** | **The `-h` banner still claimed "governance is ambient".** The identical sentence had been deliberately removed from `init`'s completion output for contradicting the project-scope default — but survived in `usage()`, a different function on a different stream, so the test that guards the install output could not see it. | Rewritten to separate mechanism from coverage. |
| **Medium** | **`--dry-run` printed "enforce: true (… off by default = observe-only …)"** — asserting both states of the same fact in one line. Plus stale `ResolveEnforce`/`ResolveFinops`/`ResolveTier2`/`ResolveFindings` doc comments in both adapter facades, and `devinit`'s package doc and `Options.Enforce` comment. | All corrected, with the right decision record cited. |
| **Medium** | **`adapters/codex/creds.go` claimed "a config read error never turns enforcement on (INV-3 fail-safe)".** That inverted with the default: a read error resolves to the default, which is now on. | Corrected, and the reasoning stated — an unreadable config *should* enforce, or corrupting a file becomes a way to disable governance. What INV-3 still guarantees is that a *failure* never blocks a tool call. |
| **Medium** | **My own doc overstated verification.** `docs/getting-started.md` listed "macOS, Linux — exercised end to end against a real stack (`testbed/`)" in the table headed *What is not verified*, contradicting this very report. Exactly the failure `CLAUDE.md` forbids, written by me. | Corrected to say the suite has not run, with an explicit "no platform is end-to-end verified for this flow yet". |
| **Medium** | Two `README`/`data-and-privacy` claims named `~/.config/openbox/` as the runtime-state directory — Linux-only. Found by a probe writing into the real macOS config dir. | Both name all three platforms and state that `OPENBOX_HOME` does not relocate them; the `--bundle` help text too. |

### Test gaps closed

- **A two-invocation `init` sequence.** Every enforce test ran `init` exactly once, so the Critical above passed 15 green tests. `TestPlainReInitDoesNotRevertAnEnforceOptOut` opts out, re-runs plainly twice, and asserts the opt-out survives.
- **`TestBareInitWritesAnEnforcingPosture` was replaced.** It asserted a literal `enforce:true` on disk — the *implementation*, and the very spelling that caused the bug. `TestBareInitEnforcesWithoutWritingTheField` asserts the resolved posture and that nothing was written.
- **`TestRegisterWithoutAnOrgKeyExplainsWhatIsNeeded` was vacuous** — its comment promised "the error must say which kind" while the test discarded stderr and checked only the exit code. Now asserts the credential name, the key *kind*, and the alternative.
- **Migration ordering had no command-level test.** `migrate.go`'s doc names the stake (writing before migrating resets enforce, capture and the signing pins), but only the function was tested. `TestInitMigratesLegacyPostureBeforeWritingOverIt` stages a legacy config with a tuned posture, runs the real `init`, and asserts all three survived.
- **The apostrophe case** and **`--env-file` on the register path**, both now tested.

Both criticals' tests were verified to fail when their fix is reverted.

### Accepted, not fixed

- **`install_git_hook` has the same non-nil pattern** via `provider.ConfigUpdate`, which always sets it. With no `--no-install-git-hook` flag, a plain re-init reverts a previously-enabled ambient hook. Pre-existing, narrower blast radius (off by default), and outside this plan's scope — but the same class, and worth its own fix.
- **The brittle `func(c DevConfig) *bool { b := c.Field; return &b }` accessor survives on `InstallGitHook` and `FailClosed`.** Both still default false, so it is latent rather than live — and it is the next domino if either default flips.
- **`openbox doctor` reports nothing about credential presence**, despite `.env` now being the most foundational fact about whether a machine can govern anything. Pre-existing; a genuine usability gap under the new layout.

## Manual acceptance checklist

Unrun rows stay unrun. Fill in who and when, not just a tick.

| # | Case | macOS (who/when) | Linux | Windows |
|---|---|---|---|---|
| 1 | Fresh install: `auth` (register) → `init` → drive a session → events arrive | | | |
| 2 | Fresh install: `auth` with an existing agent id, pasting DID + both credentials | | | |
| 3 | Re-run `auth` with different values; second value wins | | | |
| 4 | `auth --rotate` against the live backend; DID preserved, old key rejected after | | | |
| 5 | `auth` with `OPENBOX_API_KEY` exported: writes, warns the env var wins | | | |
| 6 | Migration: legacy `dev.json` in place, first `auth` run migrates it, original untouched | | | |
| 7 | `init` at default scope governs cwd; a session in a second directory produces zero events | | | |
| 8 | `init --scope global`: no project file written, snippet printed | | | |
| 9 | `init --enforce=false` → re-read → still false → `doctor` agrees | | | |
| 10 | `init` with no credentials: exits non-zero, installs nothing | | | |
| 11 | `init --provider codex` resolves global and says so; `--scope local` errors | | | |
| 12 | Approver: `init --role approver`, then `approve list` with nothing exported | | | |
| 13 | Stranded keychain recovery: read values out by hand, paste into `auth`, session works | | | |
| 14 | `.env` mode is 0600 (unix) / documented no-op (Windows) | | | |
| 15 | Windows: `auth` prompts and masks in cmd.exe and PowerShell | — | — | |
| 16 | Windows: `OPENBOX_HOME` resolves to `C:\Users\<you>\.openbox\` | — | — | |

Rows 15-16 are the ones nothing else covers: `x/term`'s
`GetConsoleMode`/`SetConsoleMode` path is the single most likely place for a
Windows-only defect, and no cross-compile can exercise it.

## Unresolved

- **The testbed has not run.** Until it does, "events arrive after `auth` → `init`"
  rests on unit tests and a mock backend. `testbed/run-all.sh` against a live local
  stack is what settles it, and `20-capture.sh`'s new negative assertion is what
  settles the scope gap.
- **`org` is not persisted.** The plan's field table said `org` → `dev.json`, but
`DevConfig` has no org field and adding one changes a documented config contract
(which `CLAUDE.md` says needs a decision record). It is used to derive an agent
name at registration time and passed per run via `--org` / `OPENBOX_ORG`, exactly
as `init` did. Cost: a re-run of `auth` does not remember the org. Worth a
decision record if that friction matters.
- **that decision's `*bool` rationale was half wrong and is now corrected.** The plan
(and that decision's first draft) claimed `resolveBool` never reaches its default
argument. A probe showed it does — the key-presence map in `resolveBoolWithSource`
already handles the plain-bool accessor. The real and sufficient reason for the
type change is the **write** side: `omitempty` drops an explicit `false`, which
would have made `--enforce=false` silently un-appliable. That decision now records
the correction and the general rule (check both directions separately).
- **A flaky `decision` test.** `TestWriteEpochPin_ConcurrentWritesNeverLowerTheFloor`
  failed once under `-race` while the machine was loaded, then passed 8/8. The
  `decision` module has no changes in this branch, so it is pre-existing, not a
  regression — but the epoch pin is the rollback floor for signed bundles, so a real
  lost update there would matter. Filed separately.
