# Phase 08 — ADR-0022, contract v1.6, ADR-0021 amendments

## Context links

- Parent: [plan.md](plan.md) · Proposal: `plans/visuals/260827-1439-three-lanes-one-pipeline.html`
- Scouts: [scout-01](scout/scout-01-gateway-service-lifecycle.md) · [scout-02](scout/scout-02-capture-contract-conformance.md)
- Touches: `docs/adr/ADR-0021-openbox-local-gateway.md`, `contracts/dev-event/`
- Depends on: [phase-01](phase-01-go-127-floor-raise.md) (D-GO-1 lands first, by
  validation ruling); run [phase-02](phase-02-jsonschema-validator.md) first so
  the library validator is what the new branches are stressed on. **Gates phases
  09–14** (namespaces + contract version).

## Overview

- Date: 2026-08-27 · Priority: P1 · Effort: 4h
- Implementation status: **done (2026-08-28)** · Review status: **reviewed** (advisory + 5 review angles; all substantive findings applied)
- Report: [verification-260828-phase-08](reports/verification-260828-phase-08-adr-contract-decision.md)
- Write the decisions before the code. Two new local services + two new producer
  namespaces = new components, which the repo rule says require an ADR.

## Key insights

- The repo's own rule: a new table/endpoint/service needs an ADR. Two new local
  services qualify; no control-plane surface is added.
- `turnActivityIDFor` (`client/payload.go:350–373`) already namespaces four
  producers. Adding two is a small, well-precedented edit — but the schema's
  nested `oneOf` (branch index 8, `TurnCompleted`) must gain matching branches, or
  every new event fails its own contract. **That exact mistake already shipped
  once** for the gateway (ADR-0021 records it).
- **Latent bug found while planning:** the Codex rollup sets `session_rollup`
  (`adapters/codex/mapper.go:273–275`) and no `turn_index`, yet the schema's two
  branches require `turn_index` **or** `gateway_request_id`, and `session_rollup`
  is absent from the schema entirely. **Validation ruled this in-scope for this
  phase** (2026-08-27): it owns that exact edit surface, and adding two branches
  beside a broken third is worse than repairing all three at once.
- ADR-0021 stays DRAFT on §9/§10 (probes), but §5 is now **reversed by owner
  ruling** and §8 has a measured answer. Amend, don't rewrite.
- **ADR-0022 also records the build-on decisions** (validation round 2): the
  three stage-B-relevant adoptions — goproxy (D-OSS-1), otlpreceiver (D-OSS-2),
  kardianos/service (D-OSS-3) — and the go 1.27 floor raise that retired the
  version-pin scheme (D-GO-1, executed by phase 01). It must state explicitly
  that goproxy is **neither Docker nor mitmproxy** — OD2 is intact: a native Go
  library compiled into our own binary, not a runtime dependency on another
  product's process.

## Requirements

1. `docs/adr/ADR-0022-native-telemetry-and-transport-lanes.md` — accepted, covering:
   lane tiers T1/T2/T3 as *claims*; the one-producer election; the `:otel:` and
   `:proxy:` namespaces; posture keys under the existing `content_capture`/`finops`
   gates; OD1(c), OD2, OD3, OD4 recorded as owner rulings with their dates; the
   D-OSS-1/2/3 adoptions and the D-GO-1 floor raise (pins retired) with the
   explicit goproxy-is-not-mitmproxy/Docker statement; the `transport/`
   own-module consequence; the sentinel-test list each later phase must satisfy.
2. Contract **v1.6** — additive only: `x-schema-version`, `x-changelog` entry, two
   new discriminator properties, two new `oneOf` branches (+ the `session_rollup`
   repair).
3. ADR-0021 amendments — §5 reversed (record the ruling, its safeguards, the
   one-command contract), §8 completed with the 2026-08-27 measurement, §10 branch
   named (detection-tier binding from asserted telemetry for OAuth; fingerprint
   refusal for API keys).
4. `contracts/dev-event/MAPPING.md` — new producer rows + a §7 entry listing what a
   live stack must confirm for the new lanes.

## Architecture

Discriminator design — **decided at validation, 2026-08-27**:

| producer | event field | `activity_id` |
|---|---|---|
| hook turn | `turn_index` | `<session>:turn:<n>` |
| gateway | `gateway_request_id` | `<session>:gateway:<id>` |
| Codex rollup | `session_rollup` | `<session>:usage:rollup` |
| **telemetry (new)** | `otel_request_id` | `<session>:otel:<id>` |
| **transport (new)** | `proxy_request_id` | `<session>:proxy:<id>` |

Symmetric with `gateway_request_id`: one self-describing field per producer, no
enum, no rename. Ids are bounded + charset-checked before use (mirror
`gatewayemit.usableRequestID`) because both originate upstream and reach a stored
key verbatim.

Rejected: a single `relay_request_id` + `producer` enum — two fields where one
suffices, and it would make `turnActivityIDFor` branch on a value rather than a
presence, which is how the gateway branch avoids ambiguity today.

## Related code files

- `client/payload.go:350–373` `turnActivityIDFor` (add two branches)
- `client/event.go:369–475` `DevEvent` (add two fields, `omitempty`, declared last)
- `contracts/dev-event/schema/dev-event.schema.json` — `x-schema-version` (~line 28),
  `x-changelog`, `oneOf[8]` nested branches
- `contracts/dev-event/MAPPING.md`, `COVERAGE.md`
- `client/turn_key_pin_test.go:48–51, 91–107` — extend pins, don't change existing
- `docs/adr/ADR-0021-openbox-local-gateway.md` §§5, 8, 10

## Implementation steps

1. Read `oneOf[8]` in full and confirm the branch count (2 at planning time) before
   editing.
2. Write ADR-0022. Record the four rulings verbatim with dates; record D-OSS-1/2/3
   + D-GO-1 with the OD2-intact statement and the `transport/` module consequence;
   state the T3 suppressibility limit and the OD1(c) truncation cost in the
   Consequences section.
3. Add `OtelRequestID` / `ProxyRequestID` to `DevEvent` (`omitempty`, last).
4. Add the two `turnActivityIDFor` branches **above** the `turn_index` branch,
   matching the gateway branch's early-return style + comment.
5. Schema: bump `x-schema-version` to `1.6`, add the `x-changelog["1.6"]` prose, add
   the two properties, add two `oneOf` branches; add the third branch for
   `session_rollup` and its property (the repair).
6. Extend `turn_key_pin_test.go` with pins for the two new namespaces and extend the
   collision test to cover all five shapes.
7. Amend ADR-0021 §§5, 8, 10.
8. Update `MAPPING.md` (producer rows + §7 live-stack list).

## Todo

- [x] confirm `oneOf[8]` shape in-file — 2 branches, as planned
- [x] ADR-0022 written and marked accepted (incl. adoptions + floor raise +
      OD2-intact statement)
- [x] `DevEvent` fields + `turnActivityIDFor` branches
- [x] schema v1.6 (2 new branches + `session_rollup` repair) — **and the
      `TurnStarted` repair, beyond the written scope: it required `turn_index`
      unconditionally, so the OPENING half of every non-hook turn also failed.
      Both halves now `$ref` one `$defs.turnProducer`.**
- [x] pin tests extended (6 shapes incl. subagent, no existing pin changed)
- [x] ADR-0021 §§5/8/10 amended (§9 remains the only TBD)
- [x] MAPPING.md + COVERAGE.md rows
- [x] `client` and conformance modules green — **BUT C1-C41 did NOT run: this
      sandbox denies every TCP bind. Acceptance criterion 2 is unverified until
      a host that can bind re-runs them.**

## Success criteria

- All five producer namespaces provably disjoint by test, existing byte pins
  unchanged.
- A hand-built event for each of the five producers validates against v1.6;
  an event carrying **two** discriminators fails.
- `session_rollup` event validates (it does not today).
- ADR-0022 states each ruling with its date, names the three adoptions and the
  floor raise, and says explicitly that goproxy keeps OD2 intact; ADR-0021 §5
  records the reversal.

## Risk assessment

| Risk | Mitigation | Signal it broke | Response |
|---|---|---|---|
| Changing an existing `activity_id` shape breaks core dedupe | Add branches only; never edit existing ones | An existing pin test in `turn_key_pin_test.go` or `approval_key_pin_test.go` goes red | **Stop** — reverting is mandatory; a changed id is a data-loss event, not a test failure |
| `session_rollup` repair turns out unnecessary (conformance never validated it) | Verify empirically in step 1 | Rollup already validates against v1.5 | Drop the repair, keep the fixture; note it in the phase report. The repair is in-scope by validation ruling, so a no-op finding must be recorded, not silently skipped |
| Two discriminators on one event slip through | Schema `oneOf` is exactly-one by construction; add a negative test | Negative test passes when it should fail | Adjust schema before proceeding — this is the invariant every later phase rests on |
| Contract bump breaks a golden fixture | v1.6 is purely additive; fixtures carry no new field | A golden fixture diff appears | Investigate before regenerating — an additive change must not move existing bytes |
| This phase runs before phase 02 and the new `oneOf` branches are only ever exercised by the hand-rolled validator | Sequencing recommendation in plan.md; phase 02 is floor-independent and cheap | Phase 02 later fails a case involving the new branches | Investigate as a found bug in whichever validator disagrees; do not re-bless |

## Security considerations

- The two new id fields are correlation ids, not content — same class as
  `policy_id`. They must NOT be derived from prompt or body text.
- Bound and charset-check both before they reach a stored key (upstream-controlled
  input reaching a key verbatim is the `usableRequestID` precedent).
- No new field is exempt from the content gate; neither id is content, so neither
  joins `contentMetadataKeys`.

## Next steps

Phase 09 (telemetry receiver). The transport lane (phase 11) follows the
telemetry lane by validation ruling — serial, not parallel.
