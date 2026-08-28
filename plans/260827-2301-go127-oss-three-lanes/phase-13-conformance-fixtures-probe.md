# Phase 13 — replay conformance, fixtures, and probe A's instrument

## Context links

- Parent: [plan.md](plan.md) · Depends: [phase-10](phase-10-telemetry-mappers.md), [phase-11](phase-11-transport-proxy-service.md)
- Scout: [scout-02 §D](scout/scout-02-capture-contract-conformance.md)
- Corpus: openbox-logger run `20260827T063932Z-225cac` (2,076 transport events,
  98 raw bodies, 463 transcript events, 15 OTel event types)

## Overview

- Date: 2026-08-27 · Priority: P1 · Effort: 8h
- Implementation status: pending · Review status: pending
- Break the D6 logjam for the capture half: prove the new lanes on **real recorded
  traffic**, stack-free — and hand probe A the instrument it has been missing.

## Key insights

- Six shipped features carry "implemented, unit-verified, testbed NOT run." The
  reason has always been a missing live stack. A real corpus now exists, so most
  claims no longer need one.
- A C-case asserts **outbound bytes** against a stub `/evaluate` over HTTP
  (`serveCapturing`, `adapters/claude-code/enforce_conformance_test.go:244–259`).
  New cases follow that shape exactly — never assert decision logic.
- **A fake at each end of a seam proves nothing about the seam.** The gateway's
  capture was dead in production while both sides' fakes stayed green. Every new
  lane needs one control test with no fake at either end.
- **Byte-identity is now a claim about the goproxy path** (validation round 2):
  the identity suite that phase 11's spike ran once must survive as permanent
  conformance — header order, no injected headers, `system` order, SSE chunk
  boundaries — asserted against the shipped `transport/` wiring, not the spike
  prototype, so a goproxy upgrade cannot silently reintroduce a mutation.
- Probe A's blocker was never analysis — it was the lack of an instrument to inject
  candidate refusal shapes into a real session. Phase 11's injection mode is that
  instrument.
- Testbed numbering: `35-telemetry.sh` and `50-lineage.sh` already exist. Use
  **`46-otel-lane.sh`** and **`47-transport.sh`** — the proposal's "50-telemetry.sh"
  would collide and confuse.

## Requirements

1. Sanitized fixtures committed from the real corpus: OTel records (all 15 event
   types), transport request/response pairs, and at least one oversized body that
   exercises OD1(c) truncation.
2. Replay conformance cases driving the real mappers and asserting outbound bytes,
   for both new producers, capture ON and OFF.
3. **Byte-identity + SSE streaming conformance on the goproxy path**: the phase-11
   identity suite runs against the shipped transport wiring in CI, including a
   streamed (chunked/SSE) fixture asserting chunk boundaries and no injected
   headers.
4. Control tests per lane: real command → real service → real spool, no fakes.
5. Volume soak (V5) at model-call cadence with full-body attach.
6. Dormant testbed scripts `46-otel-lane.sh`, `47-transport.sh` for the live-stack
   claims, written now and run when a stack exists.
7. Probe A runbook + injector wired to phase 11's injection mode.

## Architecture

Sanitization is a build step with its own test, not a manual pass: strip
credentials, emails, org ids, absolute home paths, and provider ids from the corpus,
then **assert the sanitizer worked** (fixtures must contain none of the sentinel
patterns). Fixtures are evidence and will be committed — an unsanitized fixture is a
credential leak into git history.

## Related code files

- new: `contracts/dev-event/conformance/testdata/valid/{otel_*,proxy_*}.json`
- new: `telemetry/replay_test.go`, `transport/replay_test.go`
- new: `transport/identity_conformance_test.go` (the permanent goproxy
  byte-identity + SSE suite, promoted from the phase-11 spike)
- new: `cli/cmd/openbox/telemetrycapture_test.go`, `transportcapture_test.go`
  (the no-fake controls, mirroring `gatewaycapture_test.go`)
- new: `testbed/46-otel-lane.sh`, `testbed/47-transport.sh`
- new: `plans/260827-2301-go127-oss-three-lanes/probes/RUNBOOK.md`
- reference: `adapters/claude-code/enforce_conformance_test.go:244–259`,
  `testbed/45-gateway.sh` (script conventions), `testbed/lib/{assert,sql,session}.sh`

## Implementation steps

1. Write the sanitizer + its assertion test; produce fixtures; review one by hand
   before committing.
2. Replay cases for the telemetry mapper: turn pair, bodies, `tool_decision`
   routing, engine-health, silence finding — capture ON and OFF for each.
3. Replay cases for the transport lane: byte-identity, span attributes, `:proxy:`
   ids, blind-tunnel produces nothing.
4. Promote the phase-11 spike suite into permanent conformance: byte-identity +
   per-chunk SSE against the shipped goproxy wiring, running in CI.
5. The two control tests (no fakes).
6. Volume soak: replay a full session at real cadence; measure spool growth, flusher
   latency, truncation rate; record the numbers in `plans/reports/`.
7. Testbed scripts, dormant, documenting at the point of failure what needs a stack.
8. Probe A runbook: candidate refusal shapes, how retries are counted from
   per-attempt telemetry, and the pre-decided outcomes — a qualifying shape fills
   ADR-0021 §9's two constants; none qualifying descopes refusal and the dormancy
   test becomes permanent.

## Todo

- [ ] sanitizer + assertion test
- [ ] fixtures committed (incl. an oversized body)
- [ ] telemetry replay cases, ON and OFF
- [ ] transport replay cases incl. byte-identity and blind-tunnel silence
- [ ] goproxy identity + SSE suite permanent in CI (promoted from the spike)
- [ ] two no-fake control tests
- [ ] volume soak + recorded numbers
- [ ] `46-otel-lane.sh`, `47-transport.sh` (dormant)
- [ ] probe A runbook + injector wiring
- [ ] full `-race` sweep across all modules + both cross-compiles

## Success criteria

- Every new mapping is proven on real recorded traffic, stack-free.
- Fixtures provably contain no credential, email, org id, or home path.
- Both control tests fail if their lane's emitter is unwired (verify by
  temporarily unwiring — a drill, not a claim).
- The goproxy identity suite is red if a header is injected or SSE buffers —
  drilled once by deliberately enabling a mutating goproxy option.
- Soak numbers recorded, including the measured truncation rate under OD1(c).
- Probe A can be executed by someone who did not write this plan.

## Risk assessment

| Risk | Mitigation | Signal it broke | Response |
|---|---|---|---|
| **Sanitized fixtures still carry a secret** | Sanitizer assertion test + one manual review; fixtures are git-permanent | A sentinel pattern found post-commit | Treat as a credential leak: rotate, rewrite history — do not just delete the file |
| Replay passes but live behaviour differs | Replay is explicitly capture-half only; live claims stay in the dormant scripts | — | None; the split is stated, not blurred |
| Control test passes with a fake sneaking in | Drill it: unwire the emitter and confirm red | Test stays green when unwired | The test is worthless until it fails; fix before merge |
| A goproxy upgrade reintroduces a byte mutation | The identity suite is permanent CI, not a one-time spike | Identity case red after a dependency bump | Treat as a blocking regression; pin the finding in the bump's review |
| **Assumption: probe A finds a qualifying refusal shape** | Pre-decided both ways in the runbook | Every candidate triggers client retry | **Adjust, don't stop**: refusal descopes to observe-only, dormancy test becomes permanent, ADR-0021 §9 records the negative result |
| Volume soak reveals the realtime flusher cannot keep up | Measure before shipping the default-on flip | Latency or spool growth unbounded | Adjust cadence/batching in phase 10; if unfixable, raise OD1 again with numbers |
| Testbed scripts rot while dormant | Compile-check them in CI (`bash -n`) | — | Cheap; do it |

## Security considerations

- Fixtures are the highest-risk artifact in this plan: real session traffic, headed
  for git. Sanitize, assert, review, and keep the oversized-body fixture synthetic
  where possible.
- The injection mode fabricates provider responses. Runbook must state it is for a
  throwaway session on a throwaway project, never a real one.
- Soak data is real content; keep it under `~/.openbox/` and out of the repo.

## Next steps

Phase 14 reconciles the documentation to whatever this phase actually proved.
