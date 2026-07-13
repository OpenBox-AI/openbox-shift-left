# STORY-SL-16 — Opt-in transcript usage extraction (tokens/cost, metadata-only)

**Risk:** medium (privacy boundary — reads a content-bearing file; INV-2 is load-bearing. Off by default; gated on OD-FINOPS.)

## Source
- **PRD:** FR-2 (telemetry — tokens/cost/tool decisions), NFR-1 (privacy metadata-default).
- **Architecture:** INV-2 (metadata-only default; content strictly opt-in per org, OD4), INV-3 (never blocks).
- **Backlog:** review follow-up **SL4-TOKENS** — "Claude Code hooks expose no token/cost usage; deriving finops requires parsing `transcript_path` (content — privacy-gated). Blocked on a content-capture posture decision."
- **Session:** Phase-1 debt review (2026-07-13) item #4 — the one debt item shift-left can fully close on its own, once the privacy gate is ruled.

## User Value
Developer-runtime sessions gain **finops** (per-session token + cost totals) in the dashboard — the one telemetry axis Phase-1 lacks — without any prompt/output/file content ever leaving the machine: the adapter reads the transcript solely to extract usage *numbers*, behind an explicit, off-by-default opt-in.

## Inlined context (verified — builder need not re-read)
- **CC hooks expose no tokens/cost directly** (`adapters/claude-code/capabilities.go:25`, README "known limitations") — but the hook payload carries `transcript_path` (a JSONL file of the session's turns, each with a `usage` object: input/output/cache token counts; cost may be derivable or present). The adapter **does not read it today** (`hookevent.go` decodes only structural fields — INV-2).
- **The contract already has the fields, unused:** `client/event.go:60-67,116-117` — `Tokens{...} *Tokens` and `Cost{...} *Cost`, `omitempty`, "metadata only; absent when unknown". SL-1 schema has `tokens?`/`cost?`.
- **The privacy line (why this is gated):** `transcript_path` is a **content-bearing** file (prompts, tool outputs, file text). Extracting only integers is metadata, but *opening the file* crosses the OD4 metadata-only-default boundary — so it MUST be opt-in and prove no content escapes. Content-capture opt-in plumbing already exists (`DevConfig.content_capture`, `creds.go`; env override).
- **Best moment to read:** on `SessionEnd`/flush (off the Pre/PostToolUse hot path, NFR-2), consistent with SL-9's flush-time recording.

## Acceptance Criteria
- Behind an **explicit opt-in** (reuse/extend the content-capture gate, e.g. `DevConfig.finops` / `OPENBOX_FINOPS=1`; **default OFF**), on flush the adapter reads `transcript_path`, parses **only** `usage`/cost numeric fields, and populates `event.Tokens`/`event.Cost` on the SessionEnded (and/or per-ToolResult) event.
- **INV-2 is the load-bearing AC:** NO prompt/output/file/tool-input **content** is ever read into an event, `metadata`, `tool.*`, or span field — only integers/decimals. A conformance/fuzz test seeds a transcript with sentinel content strings and asserts **none** appear in any emitted event or on the wire.
- **Off by default = today's behavior:** with the flag unset, `transcript_path` is never opened; events carry no tokens/cost (byte-identical to current output).
- **INV-3 preserved:** transcript read is best-effort on the flush path — a missing/huge/malformed transcript logs and is skipped; never fails the flush, blocks a tool call, or writes stdout. Bounded read (cap size) so a giant transcript can't OOM.
- Emitted numbers validate against the SL-1 `tokens?`/`cost?` schema (conformance green).

## Nonfunctional Requirements
- **security/privacy:** metadata-only guarantee proven by test (sentinel-content-absent); opt-in + warned; no secret (INV-1). **Requires security review (Sam)** — this is a privacy-boundary change.
- **reliability:** transcript parse is fault-tolerant (partial/streaming JSONL, unknown schema versions) and bounded.
- **performance:** off the hot path (flush only); bounded read.

## Write Scope
- `adapters/claude-code/` — transcript reader/parser (new file), gate wiring, and population of `event.Tokens`/`event.Cost` on flush.

## Dependencies
- **Hard:** STORY-SL-4 (adapter/flush + `DevConfig` gate), STORY-SL-1 (tokens/cost schema).
- **Soft:** aligns with SL-9's flush-time recording (same path).
- **External:** none — self-contained (the transcript is local).

## Invariants
- **INV-2:** only numeric usage/cost extracted; no content in any event/metadata/span — proven by sentinel test. Content-capture gate governs access.
- **INV-3:** best-effort; never blocks/delays/errors a tool call; no stdout.
- **INV-1:** no secret; transcript content never egressed.

## Human Gates
| Gate | Question | Owner | Evidence Needed | Allowed Outcomes |
|---|---|---|---|---|
| G1_READY | **OD-FINOPS:** is reading `transcript_path` to extract usage NUMBERS ONLY (never content), behind an off-by-default opt-in, acceptable under OD4's metadata-only posture? | brian (product/security) | this story + the privacy boundary description | confirm / revise / block |
| G_SEC | Does the reader provably emit no content (INV-2), stay fail-open, and leak no secret? | Sam (security reviewer) | review + sentinel-content-absent test | approve / revise / block |
| G3_REVIEW | Correct usage parsing + off-by-default byte-identical to today? | brian | diff review + conformance | approve / revise |

## Validation
```bash
cd adapters/claude-code && go build ./... && go vet ./... && go test -race ./...
# flag ON: seeded transcript (usage + SENTINEL content) -> event carries tokens/cost, and NO sentinel substring in any
#          emitted event / metadata / wire body (INV-2); malformed/oversized transcript -> skipped, flush still exits 0 (INV-3).
# flag OFF: transcript never opened; events carry no tokens/cost (byte-identical to current).
```

## Stop conditions
- If OD-FINOPS is not confirmed → HALT: do NOT open `transcript_path`. The seam (flag + reader skeleton) may land dormant, but reading content-bearing data without the ruling is an INV-2 breach.
- If usage numbers cannot be extracted without also parsing content into memory in a way that risks egress → STOP and redesign the parser to project only numeric fields; never take a shortcut that reads content into an egress-reachable structure.
