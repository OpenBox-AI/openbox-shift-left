# CLAUDE.md

Working context for agents and contributors; user-facing documentation is
`README.md` and `docs/`. The repo is the developer-runtime half of OpenBox
governance: one static Go binary governing the agentic coding tools (Claude Code,
Codex) developers use, feeding the same pipeline the agent runtime already uses.

## Core principle: reuse, don't rebuild

Shift-left onboards the developer runtime onto OpenBox's existing pipeline rather
than a parallel one: a tool install registers as an agent (`kind=developer`) with
the session as a child record, events go through the same
`/api/v3/governance/evaluate` with the same auth, storage is the same tables.
Prefer reusing an existing endpoint or service over adding one. Dev sessions send
no `spans[]`: see the invariant below.

The shape is a provider-agnostic engine plus one thin adapter per tool behind a
normalized event contract, so adding a provider is an adapter rather than an
engine change. Identity is per tool and data-driven off `provider.Supported()`
-- the store, the agent, `doctor`'s rows, `uninstall`'s sweep -- but do not
overread that into the rest: `laneCapable` (`cmd/openbox/initlanes.go`),
`printGovernedScope` (`scope.go`), the two provider switches in
`cmd/openbox/main.go`, `doctor.go`'s managed-config loop and `uninstall.go`'s
hook-surface list still name their providers, deliberately, and a third tool
needs each of them read. An adapter is four things: its native hook shape, its
mapper, an `OutputContract`, its installer; everything else is the engine's.

## Where things live

`docs/architecture.md` §Layout is the authority and one CI step enforces it; do
not make this file a second one. Inside `internal/`:

| Path | What |
|---|---|
| `provider/`, `adapters/common/hookflow/` | the SPI, and the engine every adapter runs on |
| `adapters/common/devconfig/`, `adapters/common/git/`, `adapters/claude-code/`, `adapters/codex/` | shared config and posture; trailer, notes mirror, commit event; one thin adapter each |
| `client/`, `decision/` | core client (workload auth: Keycloak client assertion -> token; wire payload; verdict parsing); local secret detection |
| `gateway/`, `telemetry/`, `transport/` | the three model-call lanes. `gateway/internal/dialhook` keeps a nested `internal/` on purpose |
| `cli/` | behind the `openbox` commands: `activation`, `laneservice`, `atomicfile`. The command layer itself is `cmd/openbox/` |
| `conformance/`, `depguard/`, `actions/` | the event-contract suite; the dependency guards; commit-to-deploy lineage for CI |

## Working conventions

**Filenames.** Non-test Go filenames are flat lowercase with no separators
(`approvalhold.go`, `enforceevaluate.go`); an underscore is reserved for what the
toolchain reads (`_test.go`, `_unix.go`, `_GOARCH.go`), where renaming changes
what builds. Test files may separate words to name their subject
(`localhooks_quote_test.go`), and non-Go assets are kebab-case.
`managed_config.toml` and `requirements.toml` keep underscores because Codex
reads those exact names.

**Dependencies.** One `go.mod`, so a new dependency is one `go mod tidy`; the
`internal/depguard` allowlists are scoped by package subtree and adding to one is
a decision. `renameio` is `!windows`, hence **two** `atomicWriteFile` copies:
grep the pattern, not the importer.

**Credentials are plaintext, on purpose.** `~/.openbox/.env` is `0600` on macOS
and Linux and unprotected on Windows, and anything running as the developer,
including the governed agent, can read the key, so attestation proves origin
of config rather than tamper resistance. No document may imply otherwise. The
RSA workload private key and the `workload-token.json` bearer cache are
plaintext the same way, under the same boundary. So is the local trace
(`internal/trace`, `<ConfigDir>/trace/`): it keeps every body both before and
after redaction, whatever `content_capture` says, for 7 days, and never
egresses; only API keys, bearer tokens and private keys stay out of it.
`content_capture` and the redactor govern egress, not that file.

**Privacy posture.** A decision only a human can make (scope, privacy posture,
priority) is surfaced, never inferred. Content, usage and thinking capture are on
by default, opted out per key; prompt text, tool commands, file bodies, tool
output, thinking and a relayed call's bodies all egress under the one
`content_capture` key. Local secret detection redacts a body before it is
attached, and that ordering is the only in-transit control there is; detection is
keyword-driven, so an unlabelled high-entropy value below the floor is invisible
to it, and `docs/credentials-and-secrets.md` must stay true. The redactor also
rewrites developer files: check what this repo writes for `${OPENBOX_REDACTED_*}`,
and derive a base64 test fixture in code.

## Invariants a contributor would otherwise break

**`/evaluate` is the only decider.** Every gated class goes to the server; risk
is a property of the policy. `ApplyFailurePolicy` must run *after* the
evaluation: before it, it would synthesize a fail-closed deny that reads as
"already tightened" and suppress the round trip, so core never gets a chance to
accept the event and the attempt/unanswered/explicit-failure classification the
halt path depends on never happens. Deprecated keys (`tier2`,
`tier2_timeout_ms`, `require_verified_bundle`, `fail_closed`, `enforce`) stay
parseable so `openbox doctor` can warn; none of them is honoured.
**One store per field**: `.env` holds only secrets and `dev.json` only
coordinates (`agent_id`); relaxing `TestEnvFileIsNotACoordinateSource` reopens
the two-store bug where a stale value in one file silently reverts a corrected
one in the other on every install.

**A flag defaulting to true cannot express "said nothing".** Every *bool*
posture field `Update` carries (`ContentCapture`, `Tier2`, `Findings`,
`InstallGitHook`) must stay nil when a run says nothing about it, distinct from
an explicit `false`, or a written default becomes indistinguishable from a
choice. Check reads and writes separately and test the *second* invocation --
`initenforce_test.go`'s `TestInitWritesNoEnforceKeyEverAgain` is the shape to
copy (loop over two `init` runs, assert no stray key on either). A test that
runs `init` only once cannot catch this; like `usage.go`'s INV-2 allowlist, a
change making that test pass trivially is a defect.

**No event carries `spans[]`, and re-adding one is a regression.** The control
plane parses it and then discards it (persistence is gated on `hook_trigger`,
which this client never sets, because that routes a model turn onto the
approval-bypass path), and alignment feeds from `activity_input` on the
`ActivityStarted` half instead. For a model call `llm_completion` *is* the
activity. **`prompt_submitted`'s `signal_args` is the goal; every other
signal's `signal_args` is its payload**, and core's source-and-name gate is the
only thing keeping the two apart. **Thinking keeps its own `activity_output`
key.** **Every activity that *ran* carries exactly two rows** (`Workflow*` one
each; `SignalReceived` alone is unpaired). A tool blocked before it ran -- a
`PreToolUse` hook erroring, so nothing executed and no `PostToolUse*` could
fire -- produces the started row only, and so does any producer's record whose
Completed half was never accepted (core outage, timeout, 401, any explicit
non-acceptance): delivery rules are the same for every producer. Fabricating a
completion would be worse than either asymmetry. Check per `activity_id`, never
by parity.

**Delivery is all-or-nothing; ordering beyond `WorkflowStarted`-first is
best-effort.** One drainer per session, append order == delivery order, one
attempt per event plus exactly one retry for a transient failure
(`client.RetryableDelivery`: timeout, network, 5xx; never 401, 429 or other
4xx), made only if the pass has a full attempt left, else the event stays
queued unscored and the next pass starts it over. A gate's own transient
escalation failure requeues the observe copy instead of latching. An event
still unaccepted ledgers and latches its run (write-if-absent: whichever cause
reaches a run's first failure wins); appenders never wait on the drain lock. A
gate drains its own session's backlog within the slack its escalation budget
leaves, waiting at most `MaxStripeWait` (5s) for the session's stripe if
another drainer holds it; once it holds the stripe, the drain gets the FULL
remaining slack -- the cap bounds the wait for contention, not an uncontended
drain. An event the drain could not start stays queued, and this call's own
escalation may still reach core before it: **`WorkflowStarted` reaching core
before anything else of the run is the one guaranteed invariant.** An event the
drain sent but never heard back from within its budget is requeued for the 30s
drainers; core dedupes that resend on its idempotency key only if it finished
processing the first attempt before the resend arrives, so a resend racing a
still-in-flight first attempt can produce two rows. `enforce`/`fail_closed` are
parsed so `openbox doctor` can warn, not honoured
(`devconfig.ResolveEnforce()`/`ResolveFailurePolicy()` are hardcoded).

**Bounds have owners.** `MaxCommandLen` bounds a local decision request, never
egress; egress is `MaxRedactBody` then `capBody`, and `maxThinkingBytes` must stay
larger than `capBody`. Test a cap in the unit it claims: `capBody` measures bytes
and cuts runes, so it will not truncate a 64Ki-rune CJK value at all -- hence
`maxModelCallBodyBytes` in bytes. `contentMetadataKeys` must list every content
key, or an adapter writing one routes around the gate.

**The three model-call lanes must never share an `activity_id`.** Disjoint
namespaces (`:gateway:`, `:otel:`, `:proxy:`) stop dedupe absorbing one lane as a
duplicate of another; the election stops two lanes both emitting. `Lane` unset is
refused, and the `eventID` hash excludes the lane name because adding it would
move every shipped idempotency key. The election is **derived** from the tool's
env block and resolves per record; a nil `Elected` is a wiring defect, not a
setting; loopback is the discriminator, because electing a producer that does not
exist silences the one that does. The settings path reaches a daemon through its
unit, never a re-derivation: a daemon has no `$HOME`.

**Install ordering is a safety property.** Unit, start, prove it listens, then
write the env var; uninstall reverses it. Writing the var first points the tool at
a dead port, so every model call fails while `init` prints success -- and it is
why a startup election legitimately sees no routed lane. Any failure after
`WriteUnit` must remove the unit, and removal runs before the credential gate.

**Transport specifics.** `transport.New` clears the six proxy environment
variables *in the constructor*, because `net/http` caches the environment behind a
`sync.Once`. `ConnState` must close the one-shot listener or `Serve` blocks in its
second `Accept`, leaking a goroutine and fd per tunnel. Host matching folds ASCII
only (Unicode makes U+212A equal `k`); ALPN http/1.1. The CA is generated
without name constraints: containment is the per-provider intercept allowlist
(`internal/transport/hosttable.go`), not the certificate. `CA.CanIssueFor` keeps
a machine still holding an older name-constrained CA blind-tunnelling a host
outside that constraint instead of failing the handshake; `openbox init`
re-issues that legacy CA itself (`internal/transport/careissue.go`, before the
transport unit reinstalls), idempotent, and `doctor` names the finding until it
runs. On macOS, `openbox init` also activates a system-wide PAC and trusts this
CA in the System keychain (`cmd/openbox/systempac.go`,
`internal/cli/activation/sysmacos.go`): **trust the CA and read it back before
writing the PAC** (a distrusted leaf makes `; DIRECT` meaningless), and **record
every prior value before the first privileged write**, with a `Pending` marker,
so a killed run leaves something the next `init` can reconcile rather than
stranding a half-applied trust/PAC pair. Both orderings are invariants, not
preferences. The relay's cross-lane HALT latch resolves a session off
`sessionkey.ResolveProxy`'s carrier header (Claude Code's
`X-Claude-Code-Session-Id`; Codex's thread id off `x-client-request-id`, never
its `session-id` header, which is a prompt-cache key), so that header must stay
out of `credentialHeaders`' redaction list or the latch can never resolve a
session. The latch is keyed on the session's *current run* (`git.RunStore`), not
the session id itself, so a `/clear` or `--resume` starts unlatched even though
the carrier header is unchanged.

**Three shapes are pinned by tests.** `message.content` is bound as
`json.RawMessage` because it is a string on user lines and an array on assistant
ones, and a typed slice drops the line's token counts silently.
`kardianos/service` ignores `$HOME`, which is why `installUnitFn` and
`portOccupied` are seams (without them `go test ./...` installs a daemon and dials
the developer's own lanes) and why a lane unit carries `--settings`: a daemon
cannot re-derive that path. And `.env` parsing is godotenv at its defaults:
last-wins, with a parse error that echoes the offending line.

## Build and test

```bash
go build ./... && go vet ./...   # everything, from the root, one module
go test -race -count=1 ./...     # -count=1 is required: see internal/depguard
GOOS=windows GOARCH=amd64 go build ./... && GOOS=linux GOARCH=arm64 go build ./...
go test -run TestGovernanceEval -v ./cmd/openbox/   # the governance evals, by claim
```

`-count=1` is not optional: the conformance guard shells out to `go list`, whose
file reads never reach the test cache. Asserting a struct is not asserting the
wire, and asserting the wire is not asserting the receiving type. Count declared
tests against tests that produced a verdict: `httptest.NewServer` panics when it
cannot bind and a panic kills the binary, so `internal/client/memhttptest` serves
HTTP over in-memory pipes for hosts that deny it.
