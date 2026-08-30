# Two issues found verifying that decision against live OpenBox data

Date: 2026-08-14 · Repo: openbox-shift-left @ `3ec812f` · Found while checking the first
real session's events after that decision shipped.

Both are written to be pasted as GitHub issues. Neither has been filed.

---

## Issue 1 — `openbox init` cannot replace its own hook entry when the engine path changes, so a stale engine keeps running forever

**Severity: high.** Silent, self-inflicted double-counting of every governed tool call. No
warning anywhere, and `openbox doctor` does not surface it.

### What happened

A project had `openbox init` run twice over its lifetime, the second time from a context whose
`HOME` pointed somewhere else (here: a Claude Code session scratchpad). The result is
`.claude/settings.local.json` holding **two** OpenBox registrations:

```
PreToolUse:
  ENTRY 1  /private/tmp/.../<session-id>/scratchpad/homedir_for_plugin/.claude/plugins/openbox-observe/bin/openbox hook claude-code PreToolUse
           …/openbox rewake claude-code
  ENTRY 2  "/Users/<me>/.claude/plugins/openbox-observe/bin/openbox" hook claude-code PreToolUse
           "…/openbox" rewake claude-code
```

Entry 1's binary was **18 hours older than that decision merge**. Entry 2 was current. Every
hook fired twice, once per engine, for weeks-old code and current code simultaneously.

### Why `init` does not clean it up

`writeLocalHooks` merges additively and matches on the **exact command string**
(`adapters/claude-code/localhooks.go:82` → `hasLocalHookCommand`, `:133-147`). A different engine
path is a different string, so the stale entry is not recognized as ours — it is treated as a
foreign hook a developer added and is preserved. Re-running `openbox init` appends the correct
entry beside it and reports success.

That preservation is correct behaviour for a *genuinely* foreign hook and is asserted by
`TestReInitAddsTheNewHooksExactlyOnce`. The bug is that an OpenBox entry at a stale path is not
foreign — it is ours, and it is wrong.

### Observed impact (live data, 66 events, one session)

| Symptom | Evidence |
|---|---|
| Every tool call stored **two** `ActivityStarted` rows | same `activity_id`, same `tool_use_id`, different `event_id`, same millisecond |
| Prompt stored twice | two `prompt_submitted` rows, identical `signal_args` |
| `status` on only ~6 of 23 `ActivityCompleted` | the pre-that decision engine does not send the field |
| `SessionStarted` posture described a world that no longer exists | carried `bundle_sha256`, `bundle_version: "no-policy"`, `staleness: "skipped_no_token"` — fields the current adapter never populates, so that event provably came from the stale engine |

The two `ActivityStarted` rows differ in a way that pins the version gap exactly: for a `Read`,
one carries `input.content` and the other does not — the pre-that decision engine escalated
only shell/MCP, the current one gates every class. For `Bash`, both carry `command`, because
shell always escalated.

**Downstream:** `.total` increments twice per call, so Tool Health SUCCESS% is meaningless, and
latency percentiles and per-tool call counts are inflated ~2×. An operator would read this as
that decision status work being broken. It is not — it is two engines.

### Proposed fix

Make `init` recognize its own entries **regardless of path**, and replace rather than append.
A handler is ours when its command contains the engine's argv shape (`hook claude-code <Event>` /
`rewake claude-code`) — that is already a distinctive marker no foreign hook would carry.

- On merge: drop any existing handler whose command matches the OpenBox argv shape but whose
  engine path differs from the one being installed, then append the current entry.
- Report it: `init` should print what it replaced. A silent swap of a governing binary is the
  same class of problem as the silent duplicate.
- Genuinely foreign hooks stay untouched — the existing test must keep passing unchanged.

### Test to add

`TestReInitReplacesAnOpenBoxEntryAtAStaleEnginePath`: seed settings with an OpenBox entry at
path A plus one real foreign hook, run `writeLocalHooks` with path B, assert exactly one OpenBox
handler per event, all at path B, and the foreign hook still present.

### Also worth considering

`openbox doctor` cannot currently see this. A check that reads the project's
`settings.local.json` and warns when more than one OpenBox engine is registered would have caught
it in seconds. Optional, but this failure mode is invisible without it.

---

## Issue 2 — `decision_authority` never reaches the control plane, though that decision says posture carries it

**Severity: low-moderate.** Not a data-loss bug; a governance product describing evidence it does
not actually send.

### The claim

That decision §"Policy provenance as evidence" argues that deleting the bundle removes what
posture used to report about policy, and that the replacement is deliberately smaller:

> Posture therefore carries **who decides** (`decision_authority: control_plane`) and **what
> happens when they cannot be reached** (`failure_policy: fail_open | fail_closed`).

The whole paragraph is about what the **control plane** is told — its argument is that an
endpoint self-reporting a bundle hash is weaker evidence than the control plane's own record.

### What actually ships

- `Posture.DecisionAuthority` / `.FailurePolicy` exist (`adapters/common/devconfig/posture.go:109,114`)
  and are set on every session (`:153-156`).
- `openbox doctor` prints both — "decided by" / "if unreachable" (`cli/cmd/openbox/doctor.go:101-102`). ✅
- **`Posture.Metadata()` — the only path onto the wire — omits both** (`posture.go:247-257`). The
  string map lists `adapter`, `adapter_version`, `provider_version`, `bundle_version`,
  `bundle_policy_id`, `bundle_sha256`, `staleness`, `bundle_integrity`, `provider_managed`. No
  `decision_authority`, no `failure_policy`.

Confirmed in the live `SessionStarted` metadata: neither key present.

Precisely how bad: `fail_closed` **is** reported, as a boolean in `Flags()`, so the failure-policy
*information* does reach the control plane under a different name. `decision_authority` is absent
outright. So the local view is complete and the remote view is not — the inverse of what that
decision argues for.

### Second-order: the map still lists four dead keys

`bundle_version`, `bundle_policy_id`, `bundle_sha256`, `bundle_integrity` (and `staleness`) survive
in `Metadata()`. Harmless today only because nothing populates them post-that decision —
`effectivePosture()` sets only adapter/provider fields, so they are `""` and the `if v == ""` guard
drops them. But they are live code paths for a deleted subsystem, and the live data above shows
exactly what they look like when an older binary is in play.

### Proposed fix

1. Add `decision_authority` and `failure_policy` to `Metadata()`'s string map.
2. Delete the four bundle keys and `staleness` from it, and from `Posture` if nothing else reads
them. That decision's own reasoning applies: "a control that cannot engage must not appear as
one."
3. One test asserting the emitted posture contains `decision_authority` and no `bundle_*` key.

Either fix the code or amend that decision — but the two must
agree.

---

## Unresolved questions

1. **Should `init` warn, or refuse, when it finds an OpenBox entry it cannot attribute?** Silent
   replacement is convenient but swaps a governing binary without telling anyone. Leaning toward
   replace-and-print.
2. **Is `settings.local.json` the only surface with this problem?** Global/managed-settings scope
   and the plugin bundle were not checked; a stale plugin registration would presumably behave the
   same way.
3. **How many existing installs are affected?** Unknown and unmeasurable from here. Anyone whose
   engine path ever moved — including everyone who tested from a temp dir — is silently
   double-counting.
4. **Does `failure_policy` need its own key at all**, given `fail_closed` already carries the
information? Naming it would match that decision; leaving it is defensible. Decide, then make the
doc match.
