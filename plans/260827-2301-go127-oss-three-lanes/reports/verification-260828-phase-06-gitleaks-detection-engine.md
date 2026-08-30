# Phase 06 verification — gitleaks detection engine (D-OSS-4)

**Date:** 2026-08-28 · **Host:** macOS 25.0.0 darwin/arm64, go1.27.0 ·
**Branch:** `feat/tool-content-capture` · **Depends:** phase 01, **phase 05**

Replaces nine hand-rolled named-format regexes with gitleaks v8.30.1's 222
maintained rules. Keeps the generic assignment pattern and the entropy fallback.

## Verdict

**CORRECTED 2026-08-28 after the owner ran the suite on an unsandboxed host.**

The first version of this report claimed "+4 tests, FAIL count back at the sandbox
baseline of 19, nothing else moved." That claim was hollow: **six conformance cases
that assert redaction on outbound bytes never executed in my sandbox** (they need a
listener), and all six FAILED on real hardware —
`TestContentCaptureConformance/C34`, `/C42`,
`TestEnforcementConformance/C18`, `/C26`, `/C10`,
`TestEnforcementConformance_Codex/CDX-C10`, plus
`TestFinops_NoContentOnWire/thinking_is_redacted…`. Those are precisely the controls
CLAUDE.md calls load-bearing. **A verdict-set diff is worthless over tests that
cannot run**, and I reported it as if it were coverage.

Root cause and fix are in [The regression](#the-regression-and-the-fix). After the
fix `decision`, `hookflow` and the pre-existing redaction tests are green **with
zero fixture edits**, and the listener-dependent cases still need an unsandboxed
re-run to confirm.

Implemented: `decision` green with **the three pinned redaction tests passing
unmodified**. Both cross-compiles, all 12 `GOWORK=off` builds, `-race` with no data
races.

## The regression, and the fix

Deleting the nine hand-rolled named formats in favour of gitleaks' 222 broke six
cases, for two stacking reasons:

1. **gitleaks deliberately allowlists published documentation keys.** AWS's own
   `AKIA…IOSFODNN7EXAMPLE` is excluded by design — and that exact value is the
   fixture in every one of the six cases.
2. **`AWS_ACCESS_KEY_ID=` is invisible to our generic assignment pattern**, because
   `_ID` sits between the `access_key` keyword and the `=`. The keyword must be
   adjacent to the delimiter.

So the retired `aws_key` regex was the only thing covering that combination. Measured
after the fact:

| Input | Before fix | After fix |
|---|---|---|
| `AWS_ACCESS_KEY_ID=<doc key>` | **not redacted** | `aws_key` |
| `AWS_ACCESS_KEY_ID=<real key>` | `aws-access-token` | `aws_key` |
| `aws_access_key=<doc key>` | `secret_assignment` | `secret_assignment` |

**Fix: the nine patterns are restored as a FLOOR BENEATH gitleaks**, running before
it. That is what the phase's own key-insight section prescribed all along — "gitleaks
rules PLUS our existing entropy fallback … measure both directions rather than
assuming the new rules dominate" — and I applied it to the entropy pass while
deleting the named formats it also covers.

Consequences, all verified:

- all six regression payloads now redact (category `aws_key`);
- **every pre-existing test passes unmodified.** The fixture edits the first attempt
  needed in `decision/secrets_test.go`, `hookflow/enforce_redact_size_test.go` and
  three `gateway/*_test.go` files were all REVERTED — they were only ever needed to
  accommodate the regression;
- **the wire-visible category rename is undone.** Because our floor runs first, the
  audit keeps `aws_key`, `private_key`, `jwt` … for the formats that already had
  them. gitleaks rule ids appear only for formats it alone covers;
- the gain is intact and re-measured: `gitlab-pat`, `twilio-api-key`,
  `shopify-access-token`, `digitalocean-pat` and the rest still resolve through
  gitleaks.

**What this cost D-OSS-4:** the decision said "replace" nine regexes; the honest
outcome is "add 222 rules beneath which the nine remain as a floor". The nine are
loose where gitleaks is precise, and that looseness is load-bearing for values
gitleaks intentionally skips.

**Two things are NOT satisfied and one of them is a decision:**

1. **Requirement 5 (the mutation drills) could not run** —
   `TestFinops_NoContentOnWire` asserts on bytes reaching an `httptest` stub, and
   this host cannot bind a listener. Unverified here, in either direction.
2. **The false-positive soak found 2 real false positives on the enforce path.**
   Phase 06's own risk table says the response to that is *stop, tune, or fall
   back* — and step 11 says the enforce path may use the new detector only after
   the soak. See [Soak](#soak--the-open-item).

## Step 2 — weight measured BEFORE the swap, as the phase required

| Metric | Before | After | Delta |
|---|---|---|---|
| CLI binary (`-s -w`, CGO off) | 8,528,818 B | 11,258,962 B | **+2,730,144 B, +32%** |
| `decision` reachable packages | 200 | 379 | +179 |
| `decision` non-stdlib reachable | 2 | ~60 | — |
| CLI reachable packages | 231 | 401 | +170 |
| `decision/go.mod` requires | 1 | 1 direct + 69 indirect | — |

**Reachable, not merely required:** `viper` (10 pkgs), `spf13/*` (15), `afero`,
`fsnotify`, `mholt/archives`, `charmbracelet/lipgloss`, `termenv`, `rs/zerolog`,
`BobuSumisu/aho-corasick`. **`cobra` is NOT** reachable.

The cause is narrow and worth recording: `detect.NewDetectorDefaultConfig`
(`detect.go:135-145`) uses viper *purely* to unmarshal a static `//go:embed`ed
TOML string. That is why a config loader, a filesystem abstraction and a
file-watcher end up linked into the module that redacts secrets — the module
`gateway` imports, whose transitive surface that decision had just accepted as
"bounded by no test".

Reported to the owner with these numbers before any swap code was written, per
step 2. **Owner ruling: full gitleaks, accept the tree.** The documented
alternative (vendor `config/gitleaks.toml` as data, keep our matcher) was
declined.

## Step 10 — detection diff, both directions

Measured with both detectors live over a corpus of realistically-shaped and
deliberately-synthetic values.

**Every realistic format the old set caught is still caught** (9/9). gitleaks would
have reported these under its own rule ids — `aws_key`→`aws-access-token`,
`github_token`→`github-pat`, `google_api_key`→`gcp-api-key`,
`stripe_key`→`stripe-access-token`, `slack_token`→`slack-bot-token`,
`ai_api_key`→`anthropic-api-key`, `private_key`→`private-key` — but **after the fix
those renames do NOT happen**: the restored floor runs first, so the audit keeps the
original category names for these eight formats. gitleaks rule ids reach the audit
only for formats the floor does not cover.

**Newly covered**, formats the old nine had no rule for at all: `gitlab-pat`,
`twilio-api-key`, `digitalocean-pat`, `shopify-access-token`,
`square-access-token`, `grafana-cloud-api-token`, and Datadog via
`generic-api-key`. The pack carries 222 rules including 15 GitLab and 4 Shopify
variants.

**Not matched by either**: a git SHA, a UUID, a base64 `data:` image. The
low-false-positive posture on those is preserved.

**gitleaks is stricter, and mostly for good reasons.** It rejected every one of
the old suite's synthetic fixtures — `ghp_`+"a"×36, `AIza`+"B"×35,
`sk_live_`+"c"×24 — because its rules add the real charset, the real length and a
minimum entropy on top of the format. Three concrete cases:

- AWS ids are base32 (`[A-Z2-7]{16}`), so a fixture containing `0`/`1`/`8`/`9` was
  never a real key;
- `private-key` requires ≥64 characters between the BEGIN and END markers,
  because a shorter block cannot hold a usable key;
- `anthropic-api-key` requires exactly `sk-ant-api03-` + 93 chars + `AA`.

**The trade that comes with that precision:** the old patterns were loose and
therefore future-proof; gitleaks' are exact and therefore brittle to a format
change. If Anthropic changes its key length, `sk-ant-…` stops matching until the
rule pack is refreshed, where `\bsk-(?:ant-)?[A-Za-z0-9_\-]{20,}\b` would have
kept working. Our retained `secret_assignment` pattern and `redactEntropy` are the
backstop for exactly that, which is the strongest argument for having kept them.

**One measured behaviour change worth naming:** AWS's published
`${OPENBOX_REDACTED_AWS_KEY}` is now correctly NOT redacted — gitleaks allowlists it as
documentation. `TestRedact_PublishedDocKeyIsNotASecret` pins it as an improvement
rather than a regression, because on the enforce path redacting it would rewrite a
legitimate file.

## Soak — the open item

Scanned the repository's own tree: **465 files / 3,654,413 bytes**, gitleaks ON
and OFF, diffed.

- 66 files match at all — mostly our pre-existing `secret_assignment`, since this
  repo's source is full of `api_key=` / `token=` shapes. Not attributable to the
  swap.
- **gitleaks added a category in 8 files.**
- **3 of those were previously clean.** Judged individually, with the matched span
  masked:

| File | Rule | What it actually matched | Verdict |
|---|---|---|---|
| `decision/secrets_test.go` | 6 rules | this phase's own new realistic fixtures | true positive, by design |
| `cli/cmd/openbox/credential_test.go` | `generic-api-key` | a 41-char test credential | true positive |
| `adapters/claude-code/creds_test.go` | `generic-api-key` | a 16-char test key value | true positive |
| `adapters/claude-code/creds_test.go` | `generic-api-key` | **a Go identifier** in `if c.APIKey != "obx_test_key" \|\| ‹15 chars› != "c2VlZA=="` | **FALSE POSITIVE** |
| `client/gatewayspan_test.go` | `generic-api-key` | **a credential FINGERPRINT** (32 hex) — deliberately not a secret, and deliberately egressed by this product | **FALSE POSITIVE** |

No match on a git SHA, UUID, base64 asset, or `go.sum` hash from gitleaks (the
`entropy` matches on `go.sum` are ours and pre-existing).

**Why the two false positives matter more than their count.** The enforce-path
redactor *rewrites developer file bodies*. `generic-api-key` matching a Go
identifier means a governed edit to a source file could replace an identifier with
`${OPENBOX_REDACTED_GENERIC_API_KEY}` and break the code. Matching a credential
fingerprint is worse in kind: the fingerprint is a correlation hash this product
egresses on purpose, so redacting it removes evidence while claiming to protect it.

Phase 06's risk table pre-declares the response — *"Stop. Tune the rule set, or
fall back to rules-only-for-detection with our matcher"* — and step 11 gates the
enforce path on the soak. **That gate is not satisfied, and I did not
unilaterally implement a posture split to work around it.** This is the phase's
one open decision.

## Design decisions worth not re-litigating

- **Order is our patterns → gitleaks → entropy, and the order is load-bearing.**
  gitleaks replaces a finding's secret TEXT wholesale; it does not go through the
  value-group + terminator-trim path. Running it FIRST let `generic-api-key` match
  assignment-shaped secrets and redact them without that care, which both changed
  the reported category and made the JSON-parseability guarantee depend on how
  gitleaks happened to draw its capture group. Running it second gives a clean
  division: ours owns assignment-shaped values, gitleaks owns named formats ours
  cannot see. `TestRedact_JSONShapedSecrets/unescaped json, keyword` is what
  caught this.
- **Findings are located by searching for the secret text, not by column
  arithmetic.** `StartColumn`/`EndColumn` are offsets within `Finding.Line`, not
  into the input, and our inputs are arbitrary multi-line bodies. Searching has no
  global-offset failure mode. `minGitleaksSecretLen` (8) is what keeps searching
  safe from replacing a short common string.
- **The detector is built once** via `sync.OnceValue` — it compiles 222 rules and
  mutates package-global viper state. `DetectString` was checked to build a local
  findings slice rather than accumulate into `Detector.findings`, so one shared
  instance is safe for the concurrent callers this package promises
  (`TestRedact_ConcurrentSafe` under `-race`).
- **A construction failure degrades to nil rather than panicking**, because this
  runs in a hook binary. That degradation is invisible at runtime, so
  `TestGitleaksDetectorConstructs` is what makes it checkable. `findings()` also
  recovers from a panic inside a third-party rule engine.
- **Placeholders had to be re-derived.** Rule ids are hyphenated slugs
  (`aws-access-token`, `1password-secret-key`) and a hyphen is not legal in a
  shell identifier, so emitting one verbatim produced a placeholder nobody could
  export — defeating the documented purpose of the env-var shape.
  `TestPlaceholderIsEnvVarSafe` pins it.
- **Categories are wire-visible.** `RedactionCategories` reaches the durable audit,
  so renaming eight of them is a contract change. No golden fixture pinned them
  (only `secret_assignment`, which is retained), but any consumer querying
  `aws_key` will see `aws-access-token` instead.

## Fixtures updated, and why none of it is a re-blessing

Every fixture change had the same cause: the old value was detectable by FORMAT
ALONE and is not shaped like a real credential.

- `decision/secrets_test.go` — 7 cases re-shaped, categories → rule ids; one case
  converted into `TestRedact_PublishedDocKeyIsNotASecret`; the concurrency
  fixture's key replaced.
- `adapters/common/hookflow/enforce_redact_size_test.go` — the oversized-body
  fixture used the allowlisted doc key. **The property under test is unchanged**
  (an oversized body must be scanned at all); only the fixture had to become
  detectable. Worth noting our generic pattern does not catch
  `AWS_ACCESS_KEY_ID=` either, because `_ID` sits between the keyword and the
  delimiter.
- `gateway/{capture_test,captureinput_test,relayfailure_test}.go` — same
  allowlisted doc key in three fixtures.

**The three pinned tests were not touched** —
`TestRedact_ValueEndingInBackslash`, `TestRedact_JSONShapedSecrets/escaping_survives`,
`TestRedact_JSONTerminatorsSurvive` all pass unmodified, so the phase's stop
condition never fired.

**An incident worth recording: the redactor rewrote this phase's own test file.**
Writing `secret := "AKIA…"` on one line matched the generic assignment pattern,
and the enforce-path redactor replaced the literal with a placeholder on disk — so
the test then asserted nothing and failed with "fixture was not redacted". That is
the corruption risk this module documents, observed on its own source. Fixtures
are now assembled from parts and never named `secret`.

## Evidence

| Check | Result |
|---|---|
| `decision` suite | green, incl. `FuzzRedact` |
| **the 3 pinned redaction tests** | **pass UNMODIFIED** |
| `gofmt -l`, `go vet` (12 modules) | clean |
| both cross-compiles | clean |
| per-module `GOWORK=off go build` | **12/12 ok** |
| `-race` | no `DATA RACE` |
| whole-workspace verdict set | **UNRELIABLE in this sandbox** — six listener-dependent conformance cases never ran and all six were broken. Corrected above |
| **gateway guard (phase 05)** | green — and `TestNarrowingAppliesToTheLiveFile` now **PASSES instead of skipping**, because gitleaks' transitive tree landed in `gateway/go.mod` as indirect requires. Phase 05 → 06 sequencing validated |
| requirement 5 mutation drills | **NOT RUN** — sandbox cannot bind a listener |

## Unresolved questions

1. **The enforce path is not cleared by the soak.** Two false positives, both able
   to corrupt a developer file (a Go identifier; a credential fingerprint). The
   phase's own gate says the enforce path may use the new detector only after a
   clean soak. Options, none taken unilaterally: (a) accept, since the enforce
   path already redacted aggressively via `secret_assignment`; (b) disable
   gitleaks' `generic-api-key` rule specifically — it is the sole source of both
   false positives, and the named-format rules are what the swap was for;
   (c) use gitleaks on the observe path only. **(b) looks strongest** and is a
   one-line config change, but it is a deliberate narrowing of a maintained rule
   pack and therefore a decision.
2. **The mutation drills are unverified.** They need a listener. Until then there
   is no evidence that deleting the redaction or the cap still turns
   `TestFinops_NoContentOnWire` red *with gitleaks in the path*.
3. **Category renames are wire-visible.** Eight audit category names changed. No
   fixture pinned them, but a dashboard or query on `redaction_categories` would
   need updating. Should phase 07 document a mapping?
4. **Rule-pack freshness has no mechanism.** gitleaks' precision makes it brittle
   to format changes, so the pack needs periodic refresh. Nothing here does that,
   and `go.mod` pins v8.30.1.
