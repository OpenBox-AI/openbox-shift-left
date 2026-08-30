# Probe: Claude Code hook surface for status + failure/lifecycle events

Date: 2026-08-13 · Claude Code **2.1.229** (`/Users/phuongvu/.local/share/claude/versions/2.1.229`)
Gates: plan `260813-2314-dev-telemetry-and-content-posture` phase 02 step 1, phase 03 step 1.

Two independent sources, agreeing:

- **(A) Empirical.** A scratch project outside this repo, a capture hook registered for nine hook
  names, three real headless `claude -p` sessions. Only field NAMES/TYPES are recorded below — no
  session ids, no payload content (see `.gitignore:28-32` precedent).
- **(B) The installed binary's own Zod input schemas**, extracted from the 2.1.229 bundle. These are
  the authority for hooks that could not be produced on demand.

## Q1 (blocking) — does `PostToolUse` also fire for a failed call?

**No. Branch B1.** One `Bash` call ending `exit 3` produced exactly:

```
1 PreToolUse
1 PostToolUseFailure
0 PostToolUse          ← did not fire
1 Stop
1 SessionEnd
```

The binary's own hook table says the same: `PostToolUse` — "Run after **successful** tool";
`PostToolUseFailure` — "Run after tool **fails**". A successful session (probe 2) fired
`PostToolUse` and no `PostToolUseFailure`.

⇒ `PostToolUse` → `status:"completed"` is truthful. The stop-and-replan branch (B3) is not taken.

## Q2 — payload shapes (source B, cross-checked against A where produced)

| Hook | Fields | Produced live? |
|---|---|---|
| `PostToolUse` | `tool_name`, `tool_input`, `tool_response`, `tool_use_id`, `duration_ms?` | yes |
| `PostToolUseFailure` | `tool_name`, `tool_input`, `tool_use_id`, `error` (string), `is_interrupt?` (bool), `duration_ms?` | yes |
| `PermissionDenied` | `tool_name`, `tool_input`, `tool_use_id`, `reason` (string) | **no** — schema-only |
| `StopFailure` | `error` (enum, below), `error_details?` (string), `last_assistant_message?` | **no** — schema-only |
| `SubagentStart` | `agent_id`, `agent_type` | **no** — schema-only |
| `SubagentStop` | `stop_hook_active`, `agent_id`, `agent_transcript_path`, `agent_type`, `last_assistant_message?` | no |
| `Stop` | `stop_hook_active`, `last_assistant_message?`, `background_tasks?`, `session_crons?` | yes |

`StopFailure.error` enum (verbatim, 10 values):
`authentication_failed`, `oauth_org_not_allowed`, `billing_error`, `rate_limit`, `overloaded`,
`invalid_request`, `model_not_found`, `server_error`, `unknown`, `max_output_tokens`.

## Three corrections to the plan's assumptions

1. **`classifier_verdict` does not exist.** `PermissionDenied` carries `reason` (free text), not
   `denial_reason`, and no verdict field of any type — `classifier_verdict` appears **zero** times
   in the 2.1.229 bundle. Phase 03 R3's `*bool` tri-state has nothing to bind; the event ships with
   tool identity + `tool_use_id` only. The tri-state lesson is re-homed to `is_interrupt` (below).
2. **`Stop` carries no `stop_reason`** in 2.1.229 (empirically absent; absent from the schema).
That decision/0019 must say "the field does not exist on this hook", not
"deferred".
3. **`PostToolUseFailure.is_interrupt` (bool) exists** and is structural. It separates "the user
   interrupted" from "the tool failed" — both are `status:"failed"`, and without it a cancelled
   call is indistinguishable from a defect. Bound as `*bool` so absent stays absent.

`PermissionDenied` fires only "after auto mode classifier denies a tool call" (binary summary
string) — a `permissions.deny` rule denies without firing it (probe 3: `PreToolUse` only). It is
therefore wired from the schema and marked docs-only-verified, as phase 03 pre-decided.

## Q3 — do unknown hook keys break an older Claude Code?

**No — they are silently ignored.** The two other locally installed versions (2.1.227, 2.1.228)
already know all four new hooks, so the "older version" case could not be produced that way. The
underlying property was tested directly instead: a made-up key,
`"TotallyUnknownFutureHook"`, was added to the project settings and a session run.

```
exit 0 · session completed normally · the unknown hook never fired · no warning, no error
```

⇒ registering the four hook keys is safe on a version that does not implement them: the events are
simply absent (fail-open, INV-3). What an existing install does NOT get is the new events until
`openbox init` is re-run — a documentation item, not a failure.

## What this does not prove

Neither source is a live OpenBox run. That `PostToolUseFailure` reaches `/evaluate` as
`status:"failed"` is asserted by conformance on outbound bytes, not by this probe; the testbed has
still not run.
