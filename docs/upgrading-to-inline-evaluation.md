# Upgrading: inline policy evaluation

What changes for an existing install when
[ADR-0017](adr/ADR-0017-inline-policy-evaluation.md) lands. Read the two starred
items even if you read nothing else.

## ⚠️ File bodies now leave the machine on a gated call

**Who this affects:** any org running with `content_capture: true` (the default)
**and** enforcement on.

Until now, only shell commands and MCP arguments were sent for a decision, and only
for those two classes. Every gated tool call is decided by OpenBox now, so a **Write
or Edit body is sent** as part of asking for that decision.

What bounds it:

- It is gated on `content_capture`. Set `content_capture: false` and no body is sent
  for any class — enforcement then decides on the structural axes alone (tool, path,
  operation, MCP server), which is coarser rather than broken.
- The body is scanned for secrets **locally, before it is sent**, and it is the
  redacted copy that goes — the same bytes your tool call is rewritten to.
- The server sees at most the first 64KB.

Turning `secret_detection` off while capture is on sends bodies unredacted. There is
no server-side redaction to fall back on yet.

**Whether your organization needs to tell anyone about this is a product and legal
question, not an engineering one.** This note exists so the decision is made
knowingly rather than discovered.

## ⚠️ Enforcement now depends on reaching OpenBox

There is no local policy any more, so a gated call cannot be decided offline. What
happens when the control plane is unreachable is one setting:

```jsonc
// ~/.openbox/dev.json
{ "fail_closed": true }   // deny gated calls when OpenBox cannot be reached
```

It defaults to **false**, so gated calls proceed and an outage never blocks work.
The cost of that default is that blocking one hostname disables enforcement for that
machine. If your threat model includes a developer who does not want to be governed,
set `fail_closed: true` — and accept that an outage then blocks work. An org can pin
either choice through the managed config.

In exchange, an org whose policy is hand-written rego is **enforced for the first
time**. The local evaluator could not evaluate raw rego at all, so those gates
silently opened on every call.

## `openbox dev sync` is gone

It fetched the local policy bundle. There is no bundle, so there is nothing to
fetch — policy is applied per call, by the control plane.

The command reports its own removal and exits non-zero, so a pipeline that still
calls it fails loudly rather than appearing to succeed. **Remove it from your
scripts.** Nothing replaces it.

## Config keys that are now inert

All three still parse, so an existing `dev.json` keeps working. None of them does
anything:

| Key | Why |
|---|---|
| `tier2` | there are no tiers; every gated call is evaluated. **Deliberately not honoured** — an org that set `tier2: false` under the old design would otherwise stay silently ungoverned after upgrading. It warns once to stderr. |
| `tier2_timeout_ms` | the budget derives from the provider's hook ceiling, which is a correctness bound rather than a knob |
| `require_verified_bundle` | there is no bundle to verify. It is also **absent from the reported posture** now: a control that cannot engage must not appear as one |

`OPENBOX_TIER2`, `OPENBOX_TIER2_TIMEOUT_MS` and
`OPENBOX_REQUIRE_VERIFIED_BUNDLE` are inert in the same way.

## Leftover files are inert

These stay on disk after the upgrade and nothing reads them. Delete them or leave
them; neither affects behaviour.

```
<os-config-dir>/openbox/policy-bundle.json    the old local bundle
<os-config-dir>/openbox/stale/                session-start staleness markers
```

On macOS `<os-config-dir>` is `~/Library/Application Support`; on Linux,
`~/.config`.

## What did not change

Telemetry is still spooled and asynchronous — the observe path is untouched, and it
still carries no tool commands or file bodies. Approvals, lineage, commit
attestation and usage capture are unchanged. There is still no daemon and no socket:
a bounded outbound call from a hook is not a resident process.

One approval behaviour did shift: a `REQUIRE_APPROVAL` verdict is now always a
*filed* record, so the hook holds briefly for a real decision instead of falling
back to the tool's own permission prompt. An unanswered request denies rather than
asking the developer to approve their own call.
