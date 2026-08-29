# Verification — phase 12: one command in, one command out; producer election

Date: 2026-08-29 · Phase: [12](../phase-12-one-command-and-election.md) · Branch: `feat/tool-content-capture`

**Status: implemented, REVIEWED, one critical found and fixed; unit-verified bind-free,
14 modules green under `-race`, both cross-compiles green, `cli` green under
`GOWORK=off`. No socket, no stack, no testbed.**

Three phase requirements were deliberately NOT implemented as written. All three are
owner decisions surfaced in §6, not omissions. §7 is what code review found.

---

## 1. What was built

| | |
|---|---|
| `cli/internal/atomicfile/` | the atomic writer, extracted from `gatewayservice` unchanged. Three lanes now rewrite the settings file; a second copy of this helper is the shape this repo names as its original sin |
| `cli/internal/activation/` | the shared settings-env mechanism: per-lane managed/original record at `~/.openbox/activation.json` (0600), before/after SHA-256, refuse-on-conflict at REMOVAL, plus the derived producer election and the per-lane key sets |
| `cli/internal/laneservice/` | the supervisor unit renderer + kardianos install/uninstall, parameterized by a `Spec`. Three specs: gateway, telemetry, transport |
| `cli/cmd/openbox/initlane.go` | the install/removal choreography every lane runs: unit → start → PROVE listening → env, rollback-removes-unit on any later failure |
| `cli/cmd/openbox/initlanes.go` | `--full` / `--remove-all`, the purge, the dry-run plan, the partial-failure report |
| CLI flags | `--full --remove-all --telemetry --remove-telemetry --transport --remove-transport --telemetry-addr --transport-addr --lane-verbose --force-restore` |
| `doctor` | the elected producer **and why**, plus a per-lane block (unit / configured / reachable / log) |

`gatewayservice` and `initgateway.go` now delegate to the shared pieces. **Their exported
surface is unchanged and no test of theirs was edited** — `git diff --stat -- '*_test.go'`
is empty, which is the phase's own stop-signal check.

## 2. Evidence by strength

**Measured, this host.**

- **495 test verdicts across `cli`, 0 failures, 0 invisible**; 20 skips, every one naming
  the host capability it needs (socket bind). New packages: `activation` 23 declared / 28
  verdicts, `laneservice` 10 declared / 19 verdicts — verdicts exceed declared because of
  subtests, and the check that matters is that no declared test produced NO verdict.
- 14 modules green under `go test -race`.
- `GOOS=windows/amd64` and `GOOS=linux/arm64` `go vet` green across all 14 modules.
- `GOWORK=off go vet ./...` green in `cli` — the release path. No new module was added, so
  the missing-`replace` trap from phase 11 does not recur here.
- **21 mutation drills RUN, 21 red on deletion.** Readiness gate; lane supervisor identity;
  first-writer-wins; the removal conflict refusal; election precedence; the
  `OTEL_LOG_RAW_API_BODIES` subtraction; per-lane key scoping; the CA existence check; the
  install rollback; the CA purge; NO_PROXY merge-vs-replace; the elected-but-absent warning;
  unit deletion on removal; the shared-spool preservation; the base-URL-defeats-transport
  rule; the routed/candidates split; the NOT-IN-PATH line; the `--full` retire check; the loss-reason split; the live election gate; the nil-gate suppression.

  **Two of the nineteen needed redoing, and both failures were mine, not the code's.** One
  was GREEN because its test never reached the mutated line (§3). One reported RED because
  the mutation did not COMPILE — a build failure is not a red test, so every drill now
  vets before it is believed.
- Conformance: 19 declared, 19 verdicts, 0 failures, **0 invisible** — unchanged, which is
  the claim that matters. This phase moves no wire bytes.

**Reasoned from source, not executed.** That the tool reads these env key names. See §5.

**Not attempted.** Any real launchd/systemd install, any real listener, any live stack.

## 3. The drill that was green first, and why it matters more than the seventeen that passed

`TestEachLaneIsAddressedByItsOwnSupervisorIdentity` was written to catch a hardcoded
gateway identity in `loadUnit`/`unloadUnit`. Hardcoding it back left the test **GREEN**.

The reason generalizes. On darwin the install path reaches only
`launchctl bootstrap gui/<uid> <plist path>`, which names the unit by PATH and is therefore
per-lane no matter what the code does; the LABEL is used by `bootout` alone, which nothing
but a removal or a rollback calls. The test stopped after the install, so the mutated line
was never executed — and the assertion it did make (`strings.Contains(joined, "ai.openbox.telemetry")`)
passed off the plist path, which contains the label as a substring.

Fixed by driving install **and** removal, matching on `gui/501/ai.openbox.telemetry` rather
than the bare label, and adding a platform-independent assertion on `identityOf` — because
the `GOOS` switch means one run can only ever exercise one of the two branches.

**The transferable rule: a test that passes for a reason other than the one it names is
indistinguishable from one that works, and only a mutation shows the difference.** This is
the same family as this repo's "asserting the struct is not asserting the wire".

## 4. The election: derived, not stored

The phase specified a `model_call_producer` field in `dev.json` resolved with the tri-state
`*bool`/`flagPassed` pattern. **It is derived from the settings file instead**, and the
`dev.json` field was written, tested green, and then reverted.

The input is the tool's own env block — the one place that decides where a model call
actually goes. A lane observes a call only if the client is routed to it, so "who is routed"
and "who may emit" are the same question:

| lane | routed when |
|---|---|
| transport | `HTTPS_PROXY` or `HTTP_PROXY` is a **loopback** URL |
| gateway | `ANTHROPIC_BASE_URL` is a **loopback** URL |
| telemetry | `CLAUDE_CODE_ENABLE_TELEMETRY` truthy **and** an OTLP endpoint is loopback |

then precedence transport > gateway > telemetry — **corrected by one rule that a pure
ranking gets wrong.**

**ROUTED and IN PATH are different facts.** ADR-0022 ranks transport above gateway because
an in-path relay observes real bytes; that answers "what should an org install". The election
answers "what will actually see THIS call", and a base URL pointing anywhere other than the
provider defeats the relay — a loopback URL is not proxied at all (`NO_PROXY` carries
127.0.0.1, which this package writes), and any other host is blind-tunnelled because it is
not the provider's. So with both in-path lanes configured, the **gateway** is the emitter and
naming transport would attribute every turn to a lane that saw none.

The count was never at risk — exactly one lane emits in every one of these states. What was
at risk is the ATTRIBUTION, which is the election's other job. `Election` therefore carries
both `Routed` (what the settings point at, which is what doctor reports as *configured*) and
`Candidates` (what can actually see the call), and doctor prints a **NOT IN PATH** line for
the difference. Collapsing them would either hide a lane the developer installed or promise
observation that is not happening; `TestDoctorReportsAConfiguredLaneThatIsNotInPath` and
`TestABaseURLTakesTransportOutOfThePath` pin both directions.

**Why not the stored field.** It is a second store of derivable state, and its drift is
silent in the worst direction: remove the transport lane without rewriting the field and
telemetry stays quiet forever, so the machine reports **no model calls at all** while
looking perfectly configured. Every removal path would have had to remember to rewrite it.
The derived form has nothing to keep in sync — `TestTheElectionReadsTheSettingsFile` asserts
exactly that: removing transport hands the election back to telemetry with no other write.

**Loopback is the discriminator, and that is a correctness property.** An org relay in
`ANTHROPIC_BASE_URL`, a corporate proxy, and a company OTel collector are all things a
developer machine legitimately has; reading one as an OpenBox lane would elect a producer
that does not exist and silence the one that does. The opposite error — a developer's own
loopback proxy read as our transport lane — costs telemetry's turn events and is announced
in its log. `TestSomebodyElsesRemoteEndpointIsNotOurLane` pins the direction.

**Only telemetry asks.** The two in-path lanes are excluded structurally: the client reaches
one of them or the other. `doctor` and the telemetry daemon call the SAME resolver, which is
the duplicate-hook-engine lesson applied — a check and the thing it checks must not be able
to disagree.

**The election's own worst failure is ELECTED BUT ABSENT, and doctor names it.** Because the
input is the routing rather than what OpenBox installed, a developer's own loopback proxy —
or a stale key left by hand — elects a lane that does not exist. Every other lane then
correctly stands down, so the machine emits **nothing at all** while each individual doctor
line still reads as fine. `TestDoctorNamesAnElectedLaneThatIsNotThere` pins the one line that
puts "elected" and "nothing listening" together; deleting it is drill 12.

Consequence stated rather than hidden, and printed at install: the tool reads settings at
**session start**, so a session already open keeps producing from the lane it started with.
The count stays right; the lane is stale until that session ends.

## 5. The seam this phase cannot close from inside the repo

`activation/keys.go` names 13 telemetry keys and 5 transport keys. Every test around them
asserts JSON **we wrote**; the consumer reads these names and silently ignores what it does
not recognize — the same failure as `http_status` vs `http_status_code`, where every golden
fixture stayed green while the field vanished before storage.

So the names and values are copied verbatim from the set that produced the sibling lab
repo's corpus (`openbox-logger` run `20260827T063932Z-225cac`,
`src/openbox_logger/settings.py` `TELEMETRY_ENV`), and `TestTelemetryKeysAreTheProvenSet`
pins them as a **literal list** so a rename is a decision. A misspelling here yields a green
suite and a receiver that never gets a record — which OD4 would then report as telemetry
silence, i.e. as a finding against the developer, for our typo. **Live confirmation is
phase 13's.**

**One deliberate subtraction: `OTEL_LOG_RAW_API_BODIES` is NOT written.** The logger sets
it; it makes the client dump every raw request and response body to disk. Phase 10 deferred
body ingestion pending the confinement-root decision, so this product reads none of them.
Writing the key would create a directory of unredacted prompts and completions on the
developer's machine that nothing consumes — a liability with no corresponding evidence. The
pin test fails if it comes back.

## 6. Two deviations from the phase file — owner decisions, not omissions

**(a) The gateway's env module was NOT ported onto the shared mechanism** (phase
requirement 3, and the success criterion "the gateway's pre-existing tests pass with no
edits" implied it). The port is a zero-behavior-change refactor of the only socket-verified
lane in this repo, carrying the phase's own stop-condition risk, and it serves neither half
of the stated outcome. `--remove-all` composes the existing `removeGateway` instead. The
mechanisms are now two, not three: `gatewayservice` for the one shipped key, `activation`
for everything after it. "Two stores per field" is not violated — `ANTHROPIC_BASE_URL`'s
prior value has exactly one store (the legacy file), the new keys' originals have exactly
one (the record), and the bug class needs two *writable* stores for one field.

**What is NOT covered as a result:** the gateway's own env writes do not get the record's
per-lane scoping, the SHA-256 evidence, or the removal conflict refusal. A machine that
installed the gateway before this change is handled by the legacy file unchanged.

**(b) The `dev.json` election field was not shipped** (phase requirement 4) — §4.

**(c) `--remove-all` does NOT delete the spool**, against requirement 2's text, because
that text contradicts the phase's own Security section: "never delete anything outside
`~/.openbox/` and the managed keys". Two independent reasons, either alone sufficient. The
spool resolves from `os.UserConfigDir()` (`~/.config/openbox/…`), which is outside
`~/.openbox`. And it is **shared with the hook path** — `--remove-all` removes lanes, not
hooks — so deleting it would destroy undelivered governed tool-call evidence belonging to a
component that is still installed and still running. This repo's stated direction of error
for exactly this shape is over-keep, never over-delete. The spool is NAMED in the output and
in the dry-run plan, with the path, so a developer expecting a full teardown knows what
survived. `TestRemoveAllKeepsTheSharedSpool` pins it.

**(d) Smaller, named:** `--force` was already taken by the flag that moved to `openbox auth`,
so the removal escape hatch is `--force-restore`. Deep per-lane doctor blocks and OD4's
silence finding stay deferred (phase 10 already deferred the latter; it needs the daemon's
scheduling).

## 7. What code review found: one critical, and how it survived 19 drills

**The telemetry daemon resolved the election once, at its own startup**, and baked the
answer into a static `Policy{Elected bool}`. `setupLanes` installs telemetry BEFORE
transport, so on every fresh `--full` the daemon booted correctly elected — nothing
else was routed yet — froze that answer, and kept emitting after transport took the
election from it. Both lanes then described the same model call, and the disjoint
namespaces guarantee core stores both rather than rejecting one: **every token count
doubles, silently**, while `doctor` reports a clean single elected lane because it
re-resolves from the file and has no channel to a sibling daemon's memory. The reverse
costs everything instead of doubling it: install telemetry while a stronger lane is
routed, remove that lane later, and a process frozen at "not elected" is silent
forever — and the ELECTED-BUT-ABSENT warning does not fire, because something IS
listening.

**Why 19 drills missed it.** Every test covered the pure election function, or ONE
lane's install and removal in isolation. None built a second daemon's already-running
snapshot and then changed the routing under it. A mutation drill can only be red on a
line some test executes; the defect was in a line every test agreed with.

**Fixed live, not by bouncing the daemon.** `Policy.Elected` is a `func() bool`
resolved per record; nil suppresses, which makes the zero value's guarantee structural
rather than conventional. The reviewer leaned toward the surgical fix — restart
telemetry's unit whenever an install would flip the election — and that was rejected
for the reason §4 already rejected a stored election: **a snapshot of derivable state
is a second store with a sync obligation**, and the restart form covers only the paths
`init` controls, leaving a hand-edited settings file or an MDM deployment stale.
`TestElectionIsAnsweredPerRecordNotAtConstruction` flips the answer under a mapper
that is already built; two drills are red on deletion.

**The transferable rule:** a defect that every test agrees with is invisible to
mutation testing. Drills prove a test is load-bearing; they say nothing about a case
no test constructs. Phase 13 should exercise the SEQUENTIAL-INSTALL case, not only
steady state.

Also fixed from the review: the non-deterministic flag name in the claude-code-only
error (a Go map iteration, now an ordered slice — verified deterministic over 6 runs
of the real binary); the gateway's supervisor identity existing as two independent
literals; two comment/test tightenings. A stray, INCOMPLETE `transport/go.sum` diff —
a partial artifact of a `GOWORK=off` resolve the sandbox denied part-way — was
reverted rather than committed.

## 8. What a bind-capable host and a live stack must still confirm

1. A real `launchctl`/`systemctl` install of the telemetry and transport units, and that
   both come up. Every unit test here routes the write through the path-explicit writer,
   because kardianos ignores `$HOME` on darwin.
2. **That the 13 env keys are the ones the installed client actually reads** — §5. This is
   the single highest-consequence unverified claim in the change.
3. That a real session after `--full` produces `:otel:` events, and that exactly one lane's
   turn events arrive for a given model call.
4. That `--remove-all` on a real machine leaves launchctl, the settings file and
   `~/.openbox` in the state the snapshot recorded.
5. The sequential-install case end to end: `--full` on a fresh machine, then a real
   session, asserting exactly one lane's turn events arrive. §7's defect lived there.
6. The 20 `cli` tests that SKIP on this host for want of `bind`. One string they assert
   (`"NOT elected"`) was found by inspection to be about to break and was repaired; there is
   no guarantee inspection found the others.

## Unresolved questions

- **Deviations (a) and (b) in §6 are owner calls.** Both were taken under `--yagni` with the
  reasoning above; either can be reversed. (a) costs a refactor of shipped code; (b) would
  reintroduce a second store of derivable state.
- Should `--full` retiring an installed gateway be silent-and-automatic (current: it retires
  and prints), or refuse and ask? Current behavior is a superset — transport observes
  everything the gateway did, in path — but it does remove a daemon the developer installed.
- `--remove-all` now KEEPS the spool (§6(c)) and prints its path. If the owner wants a true
  full teardown, that needs either a separate flag or a `--remove-all` that also removes the
  hooks — both are decisions, and `init` has no interactive surface to confirm on.
