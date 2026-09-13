# CLAUDE.md

Working context for agents and contributors; user-facing documentation is
`README.md` and `docs/`. The repo is the developer-runtime half of OpenBox
governance: one static Go binary governing the agentic coding tools (Claude Code,
Codex) developers use, feeding the pipeline the agent runtime already uses.

## Core principle: reuse, don't rebuild

Shift-left onboards the developer runtime onto OpenBox's existing pipeline rather
than a parallel one: a tool install registers as an agent (`kind=developer`) with
the session as a child record, events go through the same
`/api/v1/governance/evaluate` with the same auth, storage is the same tables.
Prefer reusing an existing table, endpoint or service over adding one. Dev
sessions write no `spans` rows at all, and send no `spans[]`: see the invariant.

The shape is a provider-agnostic engine plus one thin adapter per tool behind a
normalized event contract, so adding a provider is an adapter rather than an
engine change. An adapter is four things: its native hook shape, its mapper, an
`OutputContract`, its installer; everything else is the engine's, which was once
copy-pasted per adapter and drifted on the enforcement path.

## Where things live

`docs/architecture.md` §Layout is the authority and one CI step enforces it; do
not make this file a second one. `.claude`, `.fab7` and the plan directory are
local working records, git-ignored. Inside `internal/`:

| Path | What |
|---|---|
| `provider/`, `adapters/common/hookflow/` | the SPI, and the engine every adapter runs on |
| `adapters/common/devconfig/`, `adapters/common/git/`, `adapters/claude-code/`, `adapters/codex/` | shared config and posture; trailer, notes, attestation; one thin adapter each |
| `client/`, `decision/` | core client (payload, AIP signing, verdicts); local secret detection |
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
reads those exact names. This diverges from generic Go guidance on purpose.

**Dependencies.** One `go.mod`, so a new dependency is one `go mod tidy`; the
`internal/depguard` allowlists are scoped by package subtree and adding to one is
a decision, though only four subtrees are guarded. `renameio` is `!windows`,
hence **two** `atomicWriteFile` copies: grep the pattern, not the importer.

**Credentials are plaintext, on purpose.** `~/.openbox/.env` is `0600` on macOS
and Linux and unprotected on Windows, and anything running as the developer,
including the governed agent, can read the signing key, so attestation proves
origin of config rather than tamper resistance. No document may imply otherwise.

**Privacy posture.** A decision only a human can make (scope, privacy posture,
priority) is surfaced, never inferred. Content, usage and thinking capture are on
by default, opted out per key; prompt text, tool commands, file bodies, tool
output, thinking and a relayed call's bodies all egress under the one
`content_capture` key. Local secret detection redacts a body before it is
attached, and that ordering is the only in-transit control there is; detection is
keyword-driven, so an unlabelled high-entropy value below the floor is invisible
to it, and `docs/credentials-and-secrets.md` must stay true. The redactor also
rewrites developer files: check what this repo writes for `${OPENBOX_REDACTED_*}`, and
derive a base64 test fixture in code.

## Invariants a contributor would otherwise break

**`/evaluate` is the only decider.** Every gated class goes to the server; risk
is a property of the policy. `ApplyFailurePolicy` must run *after* the
evaluation: before it, under `fail_closed`, it synthesizes a HALT that reads as
"already tightened" and suppresses the round trip, denying every gated call
without asking. Deprecated keys (`tier2`, `tier2_timeout_ms`,
`require_verified_bundle`) stay parseable so they can warn; `tier2` is not
honoured. **One store per field**: `.env` holds only secrets and `dev.json` only
coordinates; relaxing `TestEnvFileIsNotACoordinateSource` reopens the stale-copy
bug that reverted a corrected DID on every install.

**A flag defaulting to true cannot express "said nothing".** `Enforce` is a
`*bool` and must stay nil when a run says nothing about it; `flagPassed` makes the
distinction. Check reads and writes separately and test the *second* invocation:
fifteen green tests missed this because each ran `init` once. Like `usage.go`'s
INV-2 allowlist, a change making that test pass trivially is a defect.

**No event carries `spans[]`, and re-adding one is a regression.** The control
plane *parses* it and then **discards** it (persistence is gated on
`hook_trigger`, which this client never sets, because that routes a model turn
onto the approval-bypass path), and alignment feeds from `activity_input` on the
`ActivityStarted` half instead. So a span is neither stored nor needed, and for a
model call `llm_completion` *is* the activity. **`prompt_submitted`'s
`signal_args` is the goal; every other signal's `signal_args` is its payload**,
and core's source-and-name gate is the only thing keeping the two apart -- so a
build carrying the projection must not reach a developer before that gate is
running. And **thinking keeps its own `activity_output` key**.
And **every activity that *ran* carries exactly two rows** (`Workflow*` one
each; `SignalReceived` alone is unpaired): every in-path row was single-sided
for the life of the feature, because the live pairing check filters to *tool*
types. A tool blocked before it ran -- a `PreToolUse` hook erroring, so nothing
executed and no `PostToolUse*` could fire -- produces the started row only,
and fabricating a completion would be worse than the asymmetry. Check per
`activity_id`, never by parity.

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
only (Unicode makes U+212A equal `k`); the CA is name-constrained, ALPN http/1.1.

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
./test/run-all.sh                # end to end, needs a local OpenBox stack
```

`-count=1` is not optional: the conformance guard shells out to `go list`, whose
file reads never reach the test cache. Asserting a struct is not asserting the
wire, and asserting the wire is not asserting the receiving type. Count declared
tests against tests that produced a verdict: `httptest.NewServer` panics when it
cannot bind and a panic kills the binary, so `internal/client/memhttptest` serves
HTTP over in-memory pipes for hosts that deny it.
