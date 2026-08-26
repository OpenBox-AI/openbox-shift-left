# Docs reconciliation — the gateway, and the 2026-08-26 fix series

Operation: `/ak-docs update`. Scope confirmed with the owner: user route + architecture/privacy
corrections + contract docs + `CLAUDE.md`. `docs/testbed/e2e.md` deliberately out of scope.

Trigger: ADR-0021 shipped (contract v1.5) and the docs route had never caught up; the working tree
also carried ~1800 lines of fixes, **committed mid-session** by a parallel session (`3e4527f`
→ `5192ce5`), so every claim below now describes committed code rather than a dirty tree.

## What was false, and is not any more

| Claim | Where it was | Evidence it contradicted |
|---|---|---|
| "No daemon, no proxy" | `README.md` headline, quickstart step 4, How-it-works | `gateway/proxy.go` + `gatewayservice.WriteUnit` (launchd/systemd) |
| "OpenBox does not proxy … traffic to its model provider" | `README.md` limits, `architecture.md` §Egress | the gateway does exactly that |
| "There is still no daemon and no socket" | `architecture.md`, `upgrading-to-inline-evaluation.md` | same — `architecture.md` contradicted its own gateway section 150 lines later |
| "the observe path … still carries no tool commands or file bodies" | `upgrading-to-inline-evaluation.md` | ADR-0019 P1 (v1.3), and the README's own index entry for that file said the opposite |
| "With `content_capture: false` there are no span rows at all" | `architecture.md` | gateway span's `http_*` + fingerprint ship with capture OFF |
| adapter-facing `span.request_body/response_body` "not an egress channel … no adapter has ever populated either" | `MAPPING.md` §1, §3 | `gatewayObservedSpan` reads both |
| contract **v1.2** | `MAPPING.md` header | `x-schema-version: 1.5` |
| Prompt row silent on redaction | `data-and-privacy.md` | now redacted on Claude Code (`1974c3e`) |

## What was missing

- Whole feature absent from `README.md`, `getting-started.md`, `COVERAGE.md`, `CLAUDE.md`:
  `openbox gateway`, `--gateway`, `--remove-gateway`, `--gateway-addr`, `--gateway-upstream`;
  `~/.openbox/gateway.log`, `~/.openbox/gateway-prior-env.json`, both unit paths.
- `MAPPING.md`: no `gateway_request_id`, no field homes for the six new span fields, no §7 items.
- No statement anywhere that `--gateway` is Claude-Code-only, that removal needs no credential,
  that there is no Windows packaging, or that a displaced `ANTHROPIC_BASE_URL` is restored.

## Newly documented limits (all from the fix series, verified in source)

- A **content-encoded body is not captured** — a marker is stored; redaction cannot inspect it.
- A relayed call whose **transport fails still leaves a record** (span, no status) — the request
  already reached the provider, so "no response" must not mean "no evidence".
- Part of the gateway span is **ungated by design**: method, URL (query dropped), status,
  credential fingerprint.
- A gateway-observed turn **contributes nothing to goal alignment** — raw provider body, not the
  shape core's extractor parses. Silent gap, not corruption.
- Tool-input egress cap moved 8 KiB → 64KB, so the "first 64KB" claim in three docs is now true
  rather than aspirational. `MaxCommandLen` is local-only; egress bounds are `MaxRedactBody` then
  `capBody`.

## Finding surfaced, not fixed

**The Codex adapter egresses prompt text unredacted even with `secret_detection` on.** Its mapper
has no `RedactContent` field at all (`adapters/codex/mapper.go:185`, `hookrun.go:105`); its only
local redaction is the enforce path's `apply_patch` body. The prompt is the *only* content class
Codex sends, so that is 100% of Codex content unscanned. Identical shape to the Claude Code bug
fixed in `1974c3e`. Documented honestly in `data-and-privacy.md` and `COVERAGE.md` §3.4; a
background task is queued for the fix. No product code touched (docs operation).

## Files changed

`README.md`, `docs/getting-started.md`, `docs/architecture.md`, `docs/data-and-privacy.md`,
`docs/upgrading-to-inline-evaluation.md`, `contracts/dev-event/MAPPING.md`,
`contracts/dev-event/COVERAGE.md`, `CLAUDE.md`. No doc exceeds `docs.maxLoc` (800).

## Validation

- All three repo docs CI gates pass locally: tier vocabulary, prevention-claim, phantom-flag
  (15 flags used across docs, all declared in `cli/cmd/openbox/*.go`).
- Every relative link in `README.md`, `docs/*.md`, `contracts/dev-event/*.md` resolves.
- Three new in-page anchors resolve against their headings.
- Every symbol cited in prose exists (`turnActivityIDFor`, `usableRequestID`,
  `gatewayObservedSpan`, `TestGatewaySpanContentGatedOffTheWire`, `stripContent`, `capHeaders`,
  `capBody`); `openbox doctor` verified to actually call `gatewaycheck.Inspect`.
- `go build ./gateway/...` green (a concurrent session is mid-edit there; not touched).
- Not run: the repo's Go tests — no product code changed.

## Unresolved

1. `docs/testbed/e2e.md` still omits `testbed/45-gateway.sh` from §4 layout and §5 matrix
   (out of the confirmed scope; the gap is real).
2. A concurrent session is adding gateway **verbose mode** (`gateway/verbose.go`, arrival/outcome
   log lines). No CLI flag is wired yet. If one lands, `getting-started.md`'s gateway
   troubleshooting rows and the `openbox gateway` line in README's Commands table want it.
3. `COVERAGE.md` §1's lifecycle matrix is still v1.2-era per-type; the gateway is stated as a
   non-adapter producer above the matrix instead of inside it, because a hook matrix cannot
   describe a producer with no hooks. Revisit if a second gateway event type appears.
4. `MAPPING.md` §7 items 25–30 are written against a testbed run that has never happened. They
   describe what a run must confirm, not what is confirmed.
