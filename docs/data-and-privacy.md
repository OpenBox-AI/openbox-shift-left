# Data and privacy

What leaves the machine, what never does, and the one setting that changes it.

## The short version

| | Sent to OpenBox | Notes |
|---|---|---|
| Session, tool and MCP **metadata** | always | tool name, kind (`shell`/`file`/`mcp`), file path, MCP server + tool name, timing, token counts and cost |
| **Prompt text** | yes, by default | the one content field on ordinary telemetry. `content_capture: false` turns it off |
| **Shell command text** | **never** on telemetry | only on an approval request, and only with content capture on |
| **File contents** (read or written) | **never** | a Write/Edit body is scanned *locally* for secrets and rewritten in place; the body itself is not sent |
| **Tool output** | **never** | |
| **Credentials** | **never** | the runtime key and signing seed stay in the OS keychain; the config file holds only their coordinates |
| Git **commit trailer** and signed attestation | yes | commit sha, tree sha, session id, policy bundle id — no diff, no file content |

The rule behind the table: content is gated at one choke point in the client, so a
new field cannot start egressing by accident. Structural identifiers (paths, tool
names, MCP server names) are metadata and always flow; bodies are content and do
not.

## Content capture

Content capture is **on by default**. Prompt text is sent so that governance can act
on it — guardrails, drift detection and policy that reasons about intent all need it.

Turn it off per install:

```jsonc
// ~/.config/openbox/dev.json
{ "content_capture": false }
```

or per session with `OPENBOX_CONTENT_CAPTURE=0`. With it off, sessions still produce
full metadata, lineage and cost — you lose prompt visibility and any policy that
depends on it.

An org can pin the setting so a developer cannot change it, via the managed config
(`deploy/managed/`). `openbox doctor` always reports the effective value and where it
came from.

> **Redaction at source is not implemented yet.** With capture on, prompt text is
> sent as-is; the server-side Guardrail redaction layer is not wired. If that matters
> for your data, run with capture off until it is.

## The one exception: approval requests

An approval request carries what the call is asking to *do* — the command for a
shell call, the arguments for an MCP call — because a request that reads `tool=Bash`
and nothing else is a gate no approver can exercise. It is:

- **escalation-only.** The observe copy of the same tool call is mapped separately
  and never carries it, so ordinary telemetry is unaffected.
- **content-gated.** With capture off, the field is dropped at the client choke
  point and the approval queue shows `(not captured)` rather than something that
  looks decidable.

## Local files

Everything the engine writes lives under `~/.config/openbox/` (or
`$XDG_CONFIG_HOME`), readable only by you:

| File | What it holds |
|---|---|
| `dev.json` | non-secret coordinates and your posture. No credentials |
| `policy-bundle.json` | the policy bundle pulled from your org, `0600` |
| `enforcements.jsonl` | what enforcement did: verdict, source, whether it blocked, redaction *categories* — never the secret, never the body |
| `advisories.jsonl` | advisory verdicts and guardrail findings |
| `cc-spool/` | events awaiting flush |
| `pending-approvals/`, `stale/` | content-free markers keyed by session id |
| `approvals-auto.jsonl` | an autonomous approver's decisions, if you run one |

Secrets live in the OS keychain (libsecret on Linux, Keychain on macOS). The
`--secret-backend file` opt-in writes a `0600` plaintext file instead, for machines
with no keyring — it is explicit because it trades away at-rest encryption.

## Secret detection stays local

In enforce mode, a `Write`/`Edit` body is scanned locally for credential patterns
before the tool runs. A hit is redacted **in the tool input** — the file is written
with `OPENBOX_REDACTED…` in place of the secret — and the audit records the category
(`aws_key`, `entropy`, …), never the value. Nothing about the finding except the
category leaves the machine.

## Verified, not asserted

The end-to-end suite proves this rather than documenting it: a real session writes a
file containing a synthetic AWS key and runs a shell command containing a marker,
both sourced from files so neither appears in the prompt. It then asserts the prompt
marker **is** present in what reached OpenBox and the command and file markers are
**absent from every row** the session produced. See
[`docs/testbed/e2e.md`](testbed/e2e.md) § capture.
