# Scout 01 — the seams each replacement lands on

Read-only survey of the exact code D-OSS-4…8 touch. Grounds the phase steps.

## D-OSS-4 · gitleaks → `decision/secrets.go`

**Seam is ONE unexported method.** `decision/redact.go:87,116` call
`d.scanner.Redact(text) (redacted string, categories []string, changed bool)`.
Nothing else in the module reaches the detector. `Redactor`, `Decide`,
`RedactText`, `Decision`, `DecisionRequest` are all **unchanged** — the module's
public API does not move.

**But the replacement half cannot come from gitleaks, and the plan must say so.**
`secrets.go:161-224` does *value-group structural* redaction, not masking:

- replaces only the VALUE, preserving key + quotes (`valueGroup`, 170-180);
- trims trailing structural terminators (`\`, `}`, `]`) back OUT of the
  placeholder (209-212) — `TestRedact_JSONTerminatorsSurvive` +
  `TestRedact_ValueEndingInBackslash` pin both directions, and shipping one
  without the other has already caused a real leak (CLAUDE.md);
- idempotent: never re-redacts an existing placeholder (185);
- emits content-free category names, never the secret (INV-2, 218).

Gitleaks reports *findings* over fragments (and its `--redact` only masks its own
report output); it does not structurally rewrite a JSON body in place. So
**"full detect engine" resolves concretely to: gitleaks supplies rules + matching
+ entropy; our replacement/ordering/idempotence logic stays.** That is the
boundary of what the library provides, not a narrowing of the decision.

Consumers of `decision`: `gateway/` (allowlisted), adapters, client paths — see
§Guard below.

## D-OSS-5 · santhosh-tekuri → `contracts/dev-event/conformance/`

**Lowest-risk swap in the plan.** Public API is two functions
(`ValidateDevEvent(raw []byte, contentCaptureEnabled bool) error`,
`LoadSchema()`), and the only importers are **tests**:
`adapters/claude-code/conformance_test.go`, `adapters/codex/conformance_test.go`,
`client/acceptancetest/vocabulary_test.go`. No production code path.

**The `x-content-gated` pass may not need a custom vocabulary at all.**
`validator.go:147-154` already walks the raw schema map as a **separate pass**,
deliberately independent of structural validation (`validator.go:38` states the
reason: so a posture violation is never conflated with a structural mismatch
during `oneOf` branch trials). That walk reads `schema["x-content-gated"]` off
the plain `map[string]any` — it does not hook the validator. So the swap is:
**delete the structural validator (211 LOC), keep the content-gate walk as-is,
call santhosh-tekuri for structure.** Custom-vocabulary support becomes a
nice-to-have rather than a precondition. (Researcher 01 confirms independently.)

## D-OSS-3 · kardianos → `cli/internal/gatewayservice/unit.go`

plist assembled by string concatenation, `unit.go:73-88`: `Label`,
`ProgramArguments`, `KeepAlive`, `RunAtLoad`, `ExitTimeOut`, plus hand-rolled
`xmlEscape`. Load-bearing: `StandardOutPath`/`StandardErrorPath` →
`~/.openbox/gateway.log`, because launchd sends stdio to `/dev/null` and a
silently-not-recording daemon looks healthy (ADR-0021 lesson). Researcher 02 is
verifying whether kardianos can target a custom log path without a full custom
template — **if it cannot, that is a plan-shaping result, not a detail.**

## D-OSS-6 · go-toml → `adapters/common/devconfig/toml.go`

`TopLevelTOMLKeys` (57 LOC), single consumer `adapters/codex/posture.go:122`
`codexMandated`. Bug confirmed at `toml.go:39`: `inTable = true` on any line whose
first char is `[`, then everything after is skipped — a multi-line string or
wrapped array containing such a line hides all later top-level keys, so a
mandated Codex install can read as unmandated.

## D-OSS-7 · godotenv → `adapters/common/devconfig/envfile.go`

`ParseEnvFile` (64), `unquote` (118), `WriteEnvFile` (141). Replace the parse side
only; the write side keeps 0600 and the `TestEnvFileIsNotACoordinateSource`
invariant (a DID in `.env` must stay ignored).

## D-OSS-8 · renameio → atomic write sites

`hookflow/duration.go:60`, `hookflow/turncursor.go:124`, `hookflow/findings.go:253`,
`gatewayservice/env.go`. All `CreateTemp → write → Close → Rename`, **no `f.Sync()`
and no dir fsync** (read at `duration.go:45-64`). Note `hookflow/advisory.go:121`
is a *different* pattern (`O_APPEND`, atomic by POSIX) — leave it alone.

## Guard · `gateway/guard_test.go:231-243`

The go.mod half iterates every line beginning `github.com/` and fails any module
not in the 2-entry allowlist; it does **not** skip `// indirect`. `gateway`
imports `decision`, so gitleaks + its whole transitive tree land in
`gateway/go.mod` as indirect requires and the test goes red on each. Separate
pre-existing gap: the scan only matches `github.com/` prefixes, so
`golang.org/x/…` and `go.opentelemetry.io/…` requirements are invisible to it
today.

## Unresolved

1. Can kardianos set a custom StandardOutPath without a full custom template?
   (researcher 02)
2. Does gitleaks' finding shape expose offsets good enough to drive value-group
   replacement, or do we match on `Secret` substrings? (researcher 01)
