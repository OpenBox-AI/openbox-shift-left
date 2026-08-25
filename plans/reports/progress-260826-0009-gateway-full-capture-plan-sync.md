# Plan sync — 260825-0027 full I/O capture / local gateway

Date: 2026-08-26 · Branch `feat/tool-content-capture` · Commits `dacbb46`, `d228762`, `0c961cc`
93 files, +11,887 / −111 · 39 new files · **not pushed**

Verification standing on every commit: 12/12 modules green under `-race`,
`windows/amd64` + `linux/arm64` vet clean, `go install` links, testbed scripts parse.

## Phase status

| # | Phase | Status | What is missing |
|---|---|---|---|
| 01 | Tool content capture | implemented | testbed dormant |
| 02 | Thinking capture | implemented | testbed dormant |
| 03 | Decisions, ADRs, probes | **artifacts done; 5 items are USER** | P0, probe A, P1 §1, ADR-0019 acceptance, backend filing |
| 04 | Gateway passthrough core | **complete, reviewed** | — |
| 05 | Capture, identity, account evidence | implemented **except req 5** | req 5 needs P0 |
| 06 | Gateway enforcement | implemented **except the refusal shape** | 2 constants need probe A |
| 07 | Local daemon, doctor, MDM | **implemented, all 7 reqs** | `--gateway` default revisit needs phase 08 evidence |
| 08 | Conformance and testbed | **assets written, DORMANT**; 3 CI gates live | needs a live stack to RUN |

Tests added: gateway 44, client 113, gatewaycheck 10, gatewayservice 14,
claude-code 191.

## What is shipped but NOT reachable

`openbox init --gateway` installs a **transparent proxy**. `gate.Decide`,
`capture.Capture` and `WriteRefusal` are written, tested and mutation-drilled, but
nothing calls them from `ServeHTTP` — so as shipped the gateway captures no
evidence and refuses nothing. Deliberate sequencing (the join is where a bug
refuses every model call on a developer's machine), now disclosed in ADR-0021,
which previously read as though both were live.

One design gap to fix before that wiring: `Capture` takes the response as an
argument, but the gate must run BEFORE forwarding. It needs splitting into a
request half and a response half; calling it twice would duplicate the fingerprint
and redaction work.

## The two defects worth remembering

**1. Outbound-byte assertions cannot see the receiving type.** Two span keys were
being silently dropped by core's `SpanData` on `Unmarshal`: `http_status` (core
spells it `http_status_code`) and `credential_fingerprint` (core has no such field
— zero matches across openbox-core). Account binding, ADR-0021 §6's whole purpose,
could never have fired. Every mutation drill, golden fixture and conformance case
here passed throughout, because all of them assert what this client SENDS.

Fixed: correct key name, plus the fingerprint on
`attributes["openbox.credential_fingerprint"]` (a real `SpanData` field that
survives ingest). `TestGatewaySpanKeysMatchCoreSpanData` pins every emitted key
against core's transcribed field list — a copy that can go stale, but one that
fails loudly rather than silently.

**Generalizes as:** this repo's rule is "asserting the struct is not asserting the
wire". One level further out: **asserting the wire is not asserting the reader.**

**2. A security assertion that could not fail.** The testbed's credential-leak
check queried `spans.request_headers` — a column that does not exist (the table has
only `attributes`/`data`/`metadata` JSON) — and because `tb_sql` discards stderr
and callers coerce `""` to `0`, it would have printed a green tick for "no
credential leaked" the first time anyone ran it. Fixed with
`tb_val_strict`/`tb_count_strict`, which keep stderr so a broken query is
inconclusive rather than a pass.

## Vacuous-check tally

Seven this session, every one caught by a mutation drill and none by reading:

| Where | Why it could not fail |
|---|---|
| capture header/body fixtures ×2 | secret-shaped literals were rewritten to `${OPENBOX_REDACTED_*}` on save, so the fixture was gone before the code ran |
| fingerprint ordering test | tested two functions independently; a pipeline ordering bug is invisible that way |
| path byte-identity test | measured: raw and rebuilt targets agree on all 12 adversarial inputs, so it discriminates nothing |
| phantom-flag CI gate | `\s` in an ERE — unsupported by BSD grep, so the pattern matched almost nothing |
| prevention-claim gate | first version banned the WORD "prevented", failing on the correct phrase "not prevented" |
| doctor non-loopback test | asserted the mismatch but never checked the reachability claim, pinning a wrong one |

**Working rule: a guard is not evidence until you have watched it fail.**

Fixtures are now assembled at runtime from low-entropy fragments; that trap is
specific to this environment and will recur.

## Phantom flags

Four written into this feature's docs before any gate existed. The worst,
`openbox gateway --config <path>`, would have made the supervised gateway fail to
start on every boot. Two CI gates now cover it: rendered units are parsed against
the CLI's declared flags, and docs are checked for flags shown in copyable
commands. A full sweep found zero remaining — the only hits were disclaimers.

## Blocked, and why code cannot close it

| Item | Needs | Unblocks |
|---|---|---|
| **P0** — does `ANTHROPIC_BASE_URL` redirect, per auth mode | a human + both auth modes; API-key half needs a key this machine lacks | phase 05 req 5, ADR-0021 §8, the tier's scope statement |
| **probe A** — a refusal shape that does not trip capability-rejection retry | an INTERACTIVE session: the "disabled for the rest of the session" signal needs ≥2 turns in one process | phase 06's 2 constants, ADR-0021 §9 |
| **probe A2** — ping-based approval hold | same | whether REQUIRE_APPROVAL can hold instead of refuse |
| **P1 §1** — org id from the OAuth bearer | gated behind P0 1b arriving | ADR-0021 §10's branch |
| **ADR-0019 acceptance** | owner signature | nothing in code |
| **Backend asks** | outward-facing, cross-repo | account binding matching server-side |
| **A live stack** | infrastructure | phase 08, and Track A's dormant assertions since ADR-0017 |

P1 §3 was run (it needs no credential, network or quota):
`oauthAccount.organizationUuid` and `emailAddress` are readable locally, which is
why phase 05 req 6 shipped.

**Highest leverage: P0 and probe A.** Two runs, ~30 minutes, per
`probes/RUNBOOK.md` — they close three of the seven rows above.

## Decisions made in-flight that an owner should confirm

1. **`--gateway` is OPT-IN.** ADR-0016's lesson argues for on-by-default;
   enforcement-by-default is inert without a policy, whereas this redirects live
   model traffic through a path never run against a real stack, with no Windows
   packaging. `TestGatewayIsOffByDefault` reads `init`'s help, so flipping it is a
   deliberate edit. **Revisit with phase 08 evidence.**
2. **`credential_fingerprint` and the `http_*` keys are NOT content-gated.** A
   privacy switch that removed the fingerprint would let an org opt out of being
   identified; without the `http_*` keys core classifies the span as something else
   and every `llm_completion` reader goes quiet.
3. **`account_email` egresses ungated**, like the DID. Documented in
   `docs/data-and-privacy.md` with the seven sibling fields deliberately excluded.
   **If ungated PII is unacceptable for an org, there is no switch today** short of
   not signing in. That is a posture decision, not an implementation gap.
4. **`--shutdown-grace` defaults to 30s** and must be chosen together with the
   supervisor's stop timeout (launchd 20s, systemd 90s). Whether a long completion
   may block a restart is policy.
5. **CONSTRAIN forwards** at the gateway, matching every other consumer here. The
   always-refuse decision covers a MISSING verdict, not one that says proceed.

## Unresolved questions

1. Does core want a `credential_fingerprint` field on `SpanData`, or is matching on
   `attributes["openbox.credential_fingerprint"]` acceptable as the contract? The
   attributes route works today; the field would be cleaner. **Backend decision.**
2. `Capture` needs splitting request/response before the gate can be wired. Confirm
   that split rather than a double call?
3. Should `--gateway` default on once phase 08 passes, or stay opt-in pending
   Windows packaging?
4. Do the other plan phase docs need the same phantom-flag sweep? The CI gate now
   covers `docs/` and `README.md`, not `plans/`.
5. Is ungated `account_email` acceptable, or is a separate opt-out key wanted?
