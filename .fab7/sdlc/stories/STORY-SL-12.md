# STORY-SL-12 — Dashboard Activity label (emit `activity_type`)

**Risk:** low (one additive pass-through field on the wire; no new endpoint, no privacy surface, no control-flow change)

## Source
- **Live pilot observation (brian, 2026-07-13):** the openbox-fe dashboard shows **Activity = "Unknown"** for every developer-runtime event (sessions + events + Event Type render fine; "otherwise it works").
- **Constraint (brian):** fix in **shift-left only** — do NOT alter openbox-fe or openbox-backend.
- **Cross-repo trace:** see [[dashboard-devruntime-display-gaps]] (memory) — verified across openbox-core, openbox-backend, openbox-fe.

## Root cause (verified, all three repos)
The dashboard is the shared **Temporal agent-runtime UI**. Its "Activity" column reads `event.activity_type || signal_name || workflow_type || operation || "Unknown"` (`openbox-fe verify/trust-tab.tsx:168-174`). `activity_type` is a **pass-through** column: openbox-core stores `payload.activity_type` **verbatim for any accepted event_type** (`storage_event.go:264`, unconditional-on-non-nil), and openbox-backend returns the row raw. Our client never set it → NULL → the FE falls through to `"Unknown"`. Setting it on the wire fixes the column with **no FE/backend change**.

> **Input/Output** stay empty by design (metadata-only, INV-2/OD4) — NOT in scope; documented RUNBOOK §6.4.

## Acceptance Criteria
- The `/evaluate` payload carries `activity_type`, **always non-empty** (empty ⇒ the UI shows "Unknown").
- Derived from fields that **survive the adapter's spool round-trip** (`EventType` + `Tool.Name`) — a `json:"-"` field would be lost between the hot-path spool and the flush `Emit`:
  - tool events (`ToolCall`/`ToolResult`) → the specific tool name (`Edit`/`Bash`/`mcp__…`);
  - lifecycle (`SessionStarted`/`PromptSubmitted`/`SessionEnded`) and `Deploy` → the `event_type` string.
- Identifier-class only — a tool name or an event type — **never content** (INV-2). No secret (INV-1).
- No behavior change to Emit's fail-open/verdict handling (INV-3); additive to SL-1/SL-3's payload.

## Write Scope
- `client/` — `event.go` payload struct + `payload.go` `activityLabel()` derivation (the single place; covers every adapter + the git action uniformly). No adapter/git-action change needed.

## Dependencies
- **Hard:** SL-3 (client payload). **Soft:** SL-4 (spool round-trip is why the derivation avoids a `json:"-"` field).
- **External (assumed-satisfied):** [EXT-core] `activity_type` pass-through (already present in stock core — no edit required, unlike the event-type accept-list).

## Invariants
- **INV-1:** no secret on the wire. **INV-2:** identifier/enum only, never content. **INV-3:** no control-flow change.

## Human Gates
| Gate | Question | Owner | Evidence | Outcomes |
|---|---|---|---|---|
| G3_REVIEW | Is `activity_type` derived only from non-content identifiers, always non-empty, and correct across the spool round-trip? | brian | diff review + `activity_type=Bash`/`SessionStarted` delivered to a mock `/evaluate` (E2E) + a live dashboard showing real labels | approve / revise |

## Validation
```bash
cd client && go build ./... && go vet ./... && go test ./...   # activity_type derivation (tool→name, lifecycle/Deploy→event_type, never empty)
cd cli && go test ./...                                        # TestHookEndToEndSmoke asserts activity_type on the delivered wire body (spool→flush)
# live: run a real CC session against the local stack → dashboard Activity shows Edit/Bash/SessionStarted (not "Unknown")
```

## Stop conditions
- If a future core stops passing `activity_type` through (rejects/strips it) → capture status+body, surface verbatim, route to EXT-core; do not mask.
