# Phase 01 — devconfig `ResolveRealtime` flag

## Context links

- Parent: [plan.md](plan.md)
- Pattern to copy: `ResolveFindings` / `ResolveFinops` in
  `adapters/common/devconfig/devconfig.go:256-291` (bool resolvers, env
  override + dev.json key, pinned via `devconfig.Pin()`)
- Dependencies: none (first phase)

## Overview

- Date: 2026-08-11 | Priority: P1 | Status: complete | Review: complete (code-reviewer, 2026-08-11)
- Add the opt-out gate for real-time flushing. Default **true** — the only
  default-on bool resolver, so the env/JSON semantics need care.

## Key insights

- Existing bool resolvers default false and treat env `1`/`true` as on. This
  flag inverts: absent ⇒ on; `OPENBOX_REALTIME=0`/`false` or dev.json
  `realtime_flush: false` ⇒ off. Use a `*bool` JSON field so "absent" and
  "false" are distinguishable.
- `devconfig.Pin()` (already deferred at the top of both adapters' `RunHook`)
  freezes the read — no extra work needed for consistency within one hook run.

## Requirements

1. `DevConfig` gains `RealtimeFlush *bool \`json:"realtime_flush,omitempty"\``.
2. `func ResolveRealtime() bool`: env `OPENBOX_REALTIME` wins if set
   (off values: `0`, `false`; anything else set ⇒ on), else dev.json value if
   present, else `true`.
3. Godoc comment states the default-on posture and the opt-out spellings.

## Related code files

- `adapters/common/devconfig/devconfig.go` (edit)
- `adapters/common/devconfig/devconfig_test.go` (edit)

## Implementation steps

1. Add struct field + resolver next to the other `Resolve*` bools.
2. Table-driven tests: unset ⇒ true; env 0/false ⇒ false; env 1 ⇒ true;
   JSON false ⇒ false; JSON true ⇒ true; env overrides JSON both directions.

## Todo

- [x] Struct field + `ResolveRealtime`
- [x] Tests
- [x] `cd adapters/common/devconfig && go test ./...`

## Success criteria

Resolver semantics as specified; module tests green.

## Risk / security

Low. Config-only. No secret I/O; flag controls timing of egress, not content.

## Next steps

Phase 02 consumes `ResolveRealtime()` in the hookflow trigger.
