# Review: thinking capture (phase 02, contract v1.4)

Date: 2026-08-25. Reviewer: code-reviewer. Scope: commit `6c2e504` "feat(capture):
carry the turn's thinking, amending the allowlist (contract v1.4)" — the code was
already committed by the time this review ran (HEAD had moved to `be2f72b`, a
docs-only commit); working tree is clean, so `6c2e504`'s diff against its parent
`17ddc02` is the reviewed change. No scope changes proposed.

## Code Review Summary

### Scope
- Files reviewed (45 changed, +1005/-153): `adapters/claude-code/{usage.go,mapper.go,usage_test.go,content_conformance_test.go}`,
`client/{event.go,payload.go,turnspan.go,golden_test.go}`, `client/testdata/golden/activity_turn_completed_content.json`,
`contracts/dev-event/{schema/dev-event.schema.json,MAPPING.md,COVERAGE.md,conformance/testdata/**}`, `{that decision,that
decision}`, `docs/{data-and-privacy.md,architecture.md,testbed/e2e.md}`, `README.md`, `CLAUDE.md`,
`testbed/{20-capture.sh,35-telemetry.sh}`.
- Lines analyzed: ~1150 (diff) + surrounding context in `usage.go` (594 lines), `payload.go` (867), `mapper.go`, `turnspan.go` (176) read in full.
- Review focus: correctness, security/privacy-doc accuracy, and the 6 acceptance criteria in the delegation prompt.
- Updated plans: `plans/260825-0027-openbox-gateway-full-capture/phase-02-thinking-capture.md` (Review status line only).

### Overall Assessment
High-quality, thoroughly self-verified change. All 6 acceptance criteria hold under independent re-verification, including re-running (from scratch, not trusting the commit message) both mutation drills the plan claimed were run manually. No critical, high, or medium issues found. Three low-severity nits, all cosmetic/doc-precision, none affecting behavior or privacy posture.

### Critical Issues
None.

### High Priority Findings
None.

### Medium Priority Improvements
None.

### Low Priority Suggestions

1. **Doc wording slightly undersells the aggregation.** `README.md:221` and
   `docs/data-and-privacy.md:13` both say thinking is captured "one block per
   turn." In fact `appendThinking`/`aggregateTurnWindowInto`
   (`adapters/claude-code/usage.go:459-462`) concatenate **every** `thinking`
   block across the whole turn window (a window is "~52 usage lines per Stop
   firing" per the file's own comment at line 229) into one string. The wire
   shape is genuinely one *field* per turn (matches "one message per turn" used
   for the reply two lines above in the same doc), so this isn't a privacy
   misstatement — capture/redact/cap all still apply to the full concatenation —
   but "one block" reads as "one segment of reasoning," which understates volume.
   Suggested fix: "the turn's thinking blocks, concatenated" or similar.

2. **Guard-test comment overstates its own check.** `usage_test.go:267` says the
   collection bound "must stay STRICTLY LARGER than the wire cap," but the
   assertion at line 278 (`maxThinkingBytes < 4*wireCapRunes`) only requires
   `>=`, and the actual constant (`usage.go:157`, `4 * 65536`) sits exactly at
   that boundary, not strictly above it. The `>=` relationship is the
   mathematically correct one (I traced it: at exact equality, the collector's
   byte budget and `capBody`'s rune budget agree on the same cut point for the
   worst case of all-4-byte runes — see Concerns below for the trace), so the
   code and constant are fine; only the word "strictly" in the comment is loose.
   No functional impact. Cosmetic fix only.

3. **No test for the exact-fit boundary of `appendThinking`.** `usage.go:177-199`
   is exercised for: over-budget with a multi-byte rune straddling the cut
   (`TestTurnWindow_ThinkingBoundaryDoesNotSplitARune`), way-over-budget
   (`TestTurnWindow_ThinkingAccumulationIsBounded`), and empty/first-block
   (`TestTurnWindow_LiftsThinkingBlocksInFileOrder`). Missing: `len(block) ==
   room` exactly (fills to precisely `maxThinkingBytes`, no truncation, no
   off-by-one). I hand-traced this case and it is correct (`room :=
   maxThinkingBytes - len(acc) - len(sep)`; `if len(block) > room` is
   strict-greater, so equality keeps `block` whole and the result lands at
   exactly `maxThinkingBytes`, never over) — this is a coverage gap, not a bug.

### Positive Observations

- **Both mutation drills independently re-run from scratch, not just re-reading
  the commit message.** I edited `client/payload.go:436` (`capBody(ev.Content.Thinking)`
  → `ev.Content.Thinking`) and separately `adapters/claude-code/mapper.go:456`
  (`m.redact(w.Thinking)` → `w.Thinking`), one at a time, each followed by
  `go test -run TestFinops_NoContentOnWire` and a `git checkout --` revert.
  Cap removed: fails with "activity_output.thinking is 70084 runes, want <=
  65536" — matches the commit message's "70,084 runes" exactly. Redaction
  removed: fails with the raw `${OPENBOX_REDACTED_AWS_KEY}` credential visible in the
  posted body. Both drills confirm the sentinel is genuinely load-bearing, not
  cosmetic. Working tree confirmed clean (`git status --short`) after both
  reverts.
- **Exhaustive leak-path check.** Grepped every `ev.Content.*` read site in
  `client/payload.go` (5 sites: `thinking`@436, `signal_detail`@581,
  `tool_input`@686, `tool_output`@741, `prompt`@802) and `client/turnspan.go`
  (`turnAssistantSpan` reads only `ev.Content.Output`, never `.Thinking`,
  confirmed at line 112/122). Each content field has exactly one write site to
  exactly one wire key — no generic/reflective copy path exists that could
  route `Thinking` into `signal_args`, `activity_input`, or the span's
  `response_body`. `buildPayload`'s dispatch (`payload.go:182-186`) also
  confirms `turnActivityOutput` (and thus `thinking`) is only ever called for
  `EventTurnCompleted`, never `EventTurnStarted` — matches the "completed half
  only" doc claim.
- **`message.content` as `json.RawMessage` really does insulate token counts**,
  traced by hand against Go's `encoding/json` partial-decode semantics: even
  when `thinkingFrom`'s inner `[]thinkingBlock` unmarshal hits a type mismatch
  (e.g. a block's `thinking` field is a non-string), that failure is contained
  to the RawMessage sub-decode and correctly degrades to `""` (INV-3) — it
  cannot fail the outer `turnLine` unmarshal that carries `Usage`, because
  `Content` never rejects any valid JSON value. Verified this holds for string,
  null, absent-key, object, and malformed-array shapes, matching
  `TestTurnWindow_StringMessageContentStillCounts`.
- **Sidechain partition verified airtight at both layers**: parser-level
  (`TestTurnWindow_PartitionsSidechainOut`, `TestTurnWindow_ThinkingRespectsSidechainPartition`)
  and wire-level (`SENTINEL_SIDETHINKING` added to the always-forbidden
  `sentinels` list in `usage_test.go:47-56`, so a main-thread turn carrying a
  subagent's reasoning would fail `TestFinops_NoContentOnWire`, not just a
  narrower test).
- **Full independent verification, not just trusting CLAUDE.md's claim**: ran
  `gofmt -l .` (clean, repo-wide), then `go build`/`go vet`/`go test -race
  -count=1` in all 11 workspace modules individually (all green, fresh — no
  cache), plus `GOOS=windows GOARCH=amd64` and `GOOS=linux GOARCH=arm64` builds
  for the two touched modules (`client`, `adapters/claude-code`) — both clean.
- **Docs are unusually precise for what they claim.** The new thinking
  paragraph in `docs/data-and-privacy.md:195-211` correctly relies on context
  established one paragraph above ("`finops: false` also removes it, since...
  turn events exist only under usage capture") rather than re-stating it —
  a reader combining both paragraphs gets the accurate joint-gating picture. The
  golden fixture (`client/testdata/golden/activity_turn_completed_content.json`)
  pins `thinking` and the span's `response_body` as separate strings in separate
  places, which is exactly the property a future careless merge would break.

### Recommended Actions
1. (Optional, non-blocking) Reword "one block per turn" → "the turn's thinking,
   concatenated" in `README.md:221` and `docs/data-and-privacy.md:13`.
2. (Optional, non-blocking) Soften "STRICTLY LARGER" → "at least as large" in
   `usage_test.go:267` to match the actual `>=` check.
3. (Optional, non-blocking) Add one `appendThinking` unit case for
   `len(block) == room` exactly.

None of these block merge; all are polish.

### Metrics
- Build: 11/11 modules build clean; `go vet` clean on all 11; `gofmt -l .` empty.
- Test: 11/11 modules pass `go test -race -count=1 ./...` (fresh, no cache). All
  new/changed thinking tests pass individually with `-v`: `TestTurnWindow_LiftsThinkingBlocksInFileOrder`,
  `TestTurnWindow_StringMessageContentStillCounts`, `TestThinkingCollectionBoundExceedsTheWireCap`,
  `TestTurnWindow_ThinkingAccumulationIsBounded`, `TestTurnWindow_ThinkingBoundaryDoesNotSplitARune`,
  `TestTurnWindow_ThinkingRespectsSidechainPartition`, `TestFinops_NoContentOnWire`
  (incl. its new redact/cap subtest), `TestContentCaptureConformance` C40+C41.
- Cross-compile: windows/amd64 and linux/arm64 clean for `client` and `adapters/claude-code`.
- Mutation-test verification: 2/2 drills independently reproduced (cap removal,
  redaction removal), both correctly turn the sentinel red.
- Linting issues: 0.
- Plan TODOs: 15/15 checked in `phase-02-thinking-capture.md`; all verified true
  against code, not just taken on faith (redaction placeholder, span isolation,
  sidechain partition, contract bump, docs rows, and both mutation drills all
  independently re-checked above).

## Acceptance criteria verdict (from the delegation prompt)

1. **≥1 thinking block captured per turn, concatenated in file order** — HOLDS.
   `aggregateTurnWindowInto` (`usage.go:459-462`) calls `appendThinking` per
   line in file order across the whole window; tested directly.
2. **Capture OFF ⇒ no thinking anywhere on wire; byte-identical otherwise** —
   HOLDS. Gate is `mapper.go:450` (`if m.CaptureContent`); client's `stripContent`
   (`payload.go:833-843`) nils the whole `Content` struct as a second,
   independent gate. C41 and `TestFinops_NoContentOnWire`(a) assert this on
   real POSTed bytes. Schema/COVERAGE/MAPPING all state v1.4 is purely
   additive; no field was removed/renamed/retyped — confirmed by reading the
   full diff, not just the changelog prose.
3. **Sentinel is non-trivial (both drills must fail it)** — HOLDS, personally
   re-verified both from scratch (see Positive Observations).
4. **Sidechain partition cannot double-count** — HOLDS, verified at parser and
   wire level (see Positive Observations). The partition test (`tl.IsSidechain
   != sidechain { continue }`, `usage.go:443-445`) runs before either numbers or
   thinking are touched, so a line is counted on exactly one side or the other,
   never split or duplicated.
5. **No public-contract break beyond the v1.4 additive bump** — HOLDS. Schema
   `const` bumped 1.3→1.4 with one new optional `content.thinking` string
   property; every existing conformance fixture's `schema_version` field was
   bumped in lockstep (consistent with how 1.1/1.2/1.3 were each rolled out —
   not a new pattern this diff invented). All 11 modules' tests, including the
   conformance suite, pass against the new schema.
6. **No regression in token accounting; `message.content` RawMessage handles
   every shape** — HOLDS. Traced by hand (see Positive Observations) plus
   `TestTurnWindow_StringMessageContentStillCounts` covering string/null/array
   siblings in one window.

## Unresolved questions
None — every item in the delegation prompt's "SPECIFIC THINGS TO ATTACK" list
was checked against running code/tests, not just against the diff's prose.

Status: DONE
Summary: All 6 acceptance criteria verified against running code/tests (incl. independently re-running both mutation drills from scratch); full 11-module build/vet/test/gofmt/cross-compile suite re-run clean. Only 3 low-severity, non-blocking doc/comment/test-coverage nits found.
Concerns: none blocking.
