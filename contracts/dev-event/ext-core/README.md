# EXT-core — developer-runtime event-type accept-list

The **one external dependency** that makes shift-left's emitted developer events
actually *accepted* by openbox-core instead of dropped on a `400 invalid
event_type`. This directory turns that dependency from an untracked local edit
("assumed-satisfied") into a **PR-ready, reproducible, verifiable artifact**
(STORY-SL-13, Phase-1 debt #1).

- **`openbox-core-dev-event-types.patch`** — the exact change, as a `git apply`-able
  unified diff (3 files, generated against openbox-core `HEAD`).
- **`dev-event-types.json`** — the canonical machine-readable type list. Single
  source of truth: the drift guard checks it against the SL-1 contract enum and
  against the patch, so the patch can never silently drift from what shift-left
  emits.
- **`apply.sh`** — convenience applier / idempotent `--check`.

## Scope: additive, no-migration

The change is **strictly additive** (architecture **D4** — *extend the event-type
enum, don't fork the schema*; **INV-8** — *additive/compatible with core's
accepted event types*). It adds 7 constants + accept-list entries + a lifecycle
mapping. It touches **no** table, migration, wire format, or existing Temporal
event semantics. The developer types ride the same
`GovernanceEventEntity`/`SpanEntity`/`SessionEntity` path as the runtime-agent
types (the reuse principle — see repo `CLAUDE.md`).

## The 3 edits

All three are reproduced verbatim in the patch. Applied against openbox-core `HEAD`:

1. **`internal/content/governance.go`** — add the 7 `EventType*` constants
   (`SessionStarted`, `PromptSubmitted`, `ToolCall`, `ToolResult`, `SessionEnded`,
   `CommitCreated`, `Deploy`) to the `// Event Types` block.
2. **`internal/api/governance.go`** — add the same 7 constants to the
   `isValidGovernanceEventType` switch. This is the gate that today returns
   `400 "invalid event_type: <T>"` (`Abort(c, 400, …)`); with them accept-listed
   the POST proceeds and returns `200`.
3. **`internal/services/activities/governance/storage_session.go`** — map
   `SessionStarted` → `handleSessionCreate` (create the session) and
   `SessionEnded` → `handleSessionTerminal` (mark it terminal); the remaining 5
   fall through to `handleSessionLookup` (resolve within an existing session).

## How to apply

```bash
./apply.sh --check /path/to/openbox-core   # dry-run
./apply.sh         /path/to/openbox-core   # apply
# or, by hand:
cd /path/to/openbox-core && git apply /path/to/openbox-core-dev-event-types.patch
```

`apply.sh` first checks whether the change is **already applied** (reverse-apply
probe) and reports that instead of failing.

## How to upstream

Open a PR against **openbox-core** with these 3 edits (the patch is the diff).
Frame it as additive + no-migration; cite architecture D4/INV-8. Once merged, the
"assumed-satisfied" caveat threaded through SL-3/SL-4/SL-6 (`400` fail-open drop)
is retired and the developer runtime is governed end-to-end on the **same**
`/api/v1/governance/evaluate` pipeline as the agent runtime.

## How to verify

`contracts/dev-event/acceptance/` holds a live core-acceptance test. With the
stack up and dev creds present it POSTs a minimal event of **each** of the 7 types
and asserts a **non-400** outcome; it **skips cleanly** offline so unit CI stays
green.

```bash
cd contracts/dev-event/acceptance
OPENBOX_URL=http://localhost:8086 \
OPENBOX_API_KEY=obx_… OPENBOX_AGENT_DID=did:aip:… OPENBOX_ED25519_SEED=<base64-seed> \
  go test -run Acceptance ./...
```

Against **stock** core (patch NOT applied) the same test fails with the mapped
diagnostic — *"core has not accept-listed the dev event types yet … apply
`contracts/dev-event/ext-core/`"* (reusing the SL-10 reason map) — so a `400`
reads as an action item, not a mystery.

## Drift guard

`contracts/dev-event/conformance` runs offline and fails if:

- the `event_type` set in `dev-event-types.json` ≠ the SL-1 contract enum in
  `../schema/dev-event.schema.json`, or
- any type in `dev-event-types.json` is missing from the patch.

This keeps the patch honest as the contract evolves — a type can never appear in
what shift-left emits without core being taught to accept it (INV-8).

## Stop condition

If openbox-core's accept-list mechanism moves (e.g. `isValidGovernanceEventType`
becomes a table/registry, or the lifecycle switch relocates), `apply.sh` /
`git apply` will fail to apply. Regenerate the patch against the current core and
note the drift — do not ship a stale patch (STORY-SL-13 stop condition).

### Regenerating the patch

```bash
cd /path/to/openbox-core
git diff HEAD -- \
  internal/content/governance.go \
  internal/api/governance.go \
  internal/services/activities/governance/storage_session.go \
  > /path/to/contracts/dev-event/ext-core/openbox-core-dev-event-types.patch
```
