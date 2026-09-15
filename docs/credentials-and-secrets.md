# Credentials and secret detection

Where this machine's credentials live, and what the local redactor catches
before anything is attached to an event. The companion to
[Data and privacy](data-and-privacy.md), which owns what leaves the machine.

## Where credentials live

Two kinds of file, both in **plaintext**. Nothing is sent to OpenBox; but there
is no encryption at rest either, and the difference matters, so here it is
plainly.

**One store per governed tool.** Each tool you run `openbox init --provider
<tool>` for gets its own agent, with its own DID, in its own directory:

```
~/.openbox/claude-code/.env
~/.openbox/codex/.env
    OPENBOX_API_KEY='obx_…'             # that tool's agent runtime key
    OPENBOX_AGENT_PRIVATE_KEY='…'       # the Ed25519 key that tool signs with
```

**One org-level file**, shared by every tool, holding the organization
connection and nothing else:

```
~/.openbox/.env
    OPENBOX_CONTROL_TOKEN='obx_key_…'   # written by `openbox auth`; see below
~/.openbox/dev.json
    backend_url, base_url               # coordinates `init` copies into each tool
```

- **On macOS and Linux** the file is `0600` under a `0700` directory, so other
  local users cannot read it. Anything running **as you** can: a shell
  one-liner, a dependency's install script, and **the coding agent under
  governance**, which by design runs arbitrary commands as you.
- **On Windows there is no at-rest protection at all.** `0600` is a no-op there,
  it only toggles the read-only attribute, so the file inherits the parent ACL
  and other local accounts can read it. Use full-disk encryption; do not treat
  this file as protected.
- **It is the only copy.** OpenBox shows the API key and signing key exactly
  once, at registration, and does not store them. Lose a tool's file and there
  is no recovery for that identity: `openbox init --provider <tool>` registers a
  new agent with a new DID, which leaves work attributed to the old one attached
  to the old one. If you still have the key and seed, that command offers to
  **adopt** the existing agent instead, which keeps the DID.
- **Never commit it.** The file's own header comment says so; it lives in your
  home directory rather than anywhere near a repo for that reason.
- **A killed write can leave a copy.** Every credential write is atomic — the
  body goes into a temporary file and is renamed over the target — so a process
  killed in between leaves that copy behind holding the same secrets.
  `openbox uninstall` sweeps the ones OpenBox writes beside their target
  (`~/.openbox/**/.env-*.tmp`) and lists them before deleting them. The other
  atomic writes on this machine — the activation record, the tools' own
  settings files — stage elsewhere: through the system temp directory
  (`$TMPDIR`, per-user, `0700`) when it shares a volume with your home, and
  otherwise beside the target under a name carrying no `.tmp` suffix. Neither
  is enumerated by `uninstall`. Those hold displaced configuration rather than
  an OpenBox credential, but if your own settings carried a key, check both
  places after a crash.

What that means for evidence: a signed event or commit attestation proves
**origin-of-config**, a machine holding this agent's key produced it, not
tamper-resistance against the developer or the agent they run. The OS keychain
this replaced did not actually change that, since it was unlocked for the whole
desktop session and readable by the same processes; the plaintext file just
makes it obvious.

**The organization credential is written to the org-level file, and that is a
real exposure.** `OPENBOX_CONTROL_TOKEN` is what registers an agent, and when it
is an `obx_key_…` organization key it can **create and rotate agents across your
whole organization** — a tool's signing key compromises one agent, this one
compromises the fleet.

`openbox auth` takes it and persists it to `~/.openbox/.env`, because `auth`
takes it and `openbox init` needs it, and those are two separate processes with
nothing exported between them. So it sits on disk in plaintext, `0600` on macOS
and Linux, unprotected on Windows, readable by anything running as you —
**including the coding agent under governance**. Everything the first section
says about at-rest protection applies to this credential too, and it is the one
worth caring about most.

Two things bound it. It is **never written to a per-tool `.env`**, so a
compromised tool store is not a fleet compromise. And it is never accepted as a
flag, so it cannot leak through argv or shell history.

Prefer exporting `OPENBOX_CONTROL_TOKEN` for the one `init` run and skipping
`auth`'s token prompt if you would rather it never touch the disk; a real
environment variable wins, and `openbox uninstall` deletes the file either way.

A real environment variable always beats the file, so CI can supply credentials
without writing anything to disk:

```
secrets       OPENBOX_API_KEY, OPENBOX_AGENT_PRIVATE_KEY  env var > ~/.openbox/<tool>/.env
coordinates   OPENBOX_AGENT_DID, OPENBOX_AGENT_ID, …      env var > <tool>/dev.json > default
org secret    OPENBOX_CONTROL_TOKEN                       ~/.openbox/.env > env var
```

An exported variable outranks **every** store at once: one `OPENBOX_AGENT_DID`
makes every governed tool report the same identity. `openbox doctor` names which
source is actually in effect for exactly this reason.

**The organization control token is the one exception, and it runs the other
way.** It is the only value handed between two commands in two processes —
`openbox auth` writes it, `openbox init` reads it — so the file wins and the
variable is the fallback for when the file holds none. That keeps both routes
that need the variable working (a CI image that never runs `auth` has no file at
all; declining the token prompt leaves none in it) while stopping a forgotten
export from silently overriding every `auth` you run. `auth` says so when it
finds one set.

Secrets and non-secrets never share a file, and no value lives in two places.

## Secret detection stays local

In enforce mode, a `Write`/`Edit` body is scanned locally for credential
patterns before the tool runs. A hit is redacted **in the tool input**, the file
is written with `OPENBOX_REDACTED…` in place of the secret, and the audit
records the category (`aws_key`, `entropy`, …), never the value. Nothing about
the finding except the category leaves the machine.

### What the scanner catches; and where it stops

Measured against the real detector, not asserted (conformance
`TestContentCaptureCredentialCoverage` drives a dotenv dump through a real tool
event and asserts the flushed bytes):

| In tool output | Redacted? | Why |
|---|---|---|
| an AWS / GitHub / Stripe / JWT / `sk-` key, anywhere | yes | matched by shape, so surrounding syntax is irrelevant |
| a GitLab / Shopify / Twilio / DigitalOcean / Grafana / … token, anywhere | yes | one of gitleaks' 222 rules; shape again, no key name needed |
| `OPENBOX_API_KEY=obx_…` | yes | the key name matches a known credential keyword |
| `OPENBOX_AGENT_PRIVATE_KEY=<base64>` | yes | matched as a generic API key by shape; the entropy pass would catch it too |
| `API_KEY=<64 hex chars>` | yes | keyword match; the value's alphabet does not matter |
| **`AWS_ACCESS_KEY_ID=<value in no known format>`** | **no** | **the keyword must sit NEXT TO the delimiter, and `_ID` intervenes** |
| `DEPLOY_HEX=<64 hex chars>` | **no** | no keyword, and hex cannot clear the entropy floor |
| `{"password":"…"}` or `{"key":"<base64>"}` **nested in tool output** | yes | the generic patterns tolerate JSON quoting and escaping |

The format layer is two sets that stack: nine hand-rolled regexes
(`decision/secrets.go`) beneath gitleaks' 222 maintained rules
(`decision/gitleaks.go`). The nine are loose where gitleaks is precise,
gitleaks adds charset, length and entropy floors and allowlists published
documentation keys, so deleting them in favour of it regressed six conformance
cases and they were restored as a floor. Both layers run before the
keyword/entropy layer.

Two standing limits and one recently closed, all measured rather than assumed:

**1. For generic secrets, the keyword decides; not the shape of the value.** A
high-entropy value next to an unrecognized key name is invisible. That one is
deliberate: the entropy floor sits above what hex can reach (16 symbols cap it
at 4.0 bits per character, against a 4.5 threshold) precisely so git SHAs, UUIDs
and content hashes are never flagged. Lowering it would make the scanner fire on
ordinary identifiers; and on the enforce path the scanner **rewrites the file
your tool is about to write**, so a false positive corrupts real content.

**1b. The keyword has to be adjacent to the delimiter.** `access_key=…` is
caught; `AWS_ACCESS_KEY_ID=…` is not, because `_ID` sits between the recognised
keyword and the `=`. If the value happens to match one of the 231 format rules
the format layer catches it anyway, a real AWS key id is caught, but a
credential-named assignment carrying an unrecognised value is invisible.
Measured, and it is the gap that caused a real regression: when the nine format
regexes were briefly deleted, six conformance cases went red on exactly this
shape.

**A false-positive class worth stating, because the enforce path rewrites
files.** The entropy pass fires on a base64-class token of ≥24 characters at
≥4.5 bits per character **in a value position**; and a Go source line like
`myConstant := "<48 chars of base64>"` is a value position. During this work the
redactor rewrote three of the repo's own test files that way, replacing a
fixture with a placeholder on disk. Nothing detected it except a test that then
measured the wrong thing. If you keep base64 fixtures in source under a governed
session, that is the shape to know about.

**2. Nested JSON used to be a second gap. It is closed.** A tool's response is
itself JSON, so a nested value arrives escaped (`{\"key\":\"…\"}`), and both
generic mechanisms used to miss that shape; which covers `cat config.json` and
every MCP tool result. Both were widened, so a password or a high-entropy token
inside nested JSON is now redacted like a flat one. Recorded because the
scanner's behaviour changed, and because the named formats were never affected:
an AWS key in JSON was always caught while a database password was not, which is
exactly what made the gap easy to miss.

If your credentials fall under limit 1, that is the case to plan around: run
with `content_capture: false`, or keep them out of the working directory of a
governed session.

The same scanner runs on **every** content body before it is attached to an
event, enforce mode or not: the prompt, the assistant's reply, tool input, tool
output, and the refusal reasons. **The prompt is no longer exempt**; it was the
one field assigned directly instead of through the mapper's redactor, so it
egressed unscanned with `secret_detection` fully on; that was fixed on
2026-08-26 and conformance C42 asserts it on the outbound bytes
(`internal/adapters/claude-code/mapper.go:225`). The same shape is **still live
for Codex**, whose mapper has no redactor at all; see [coverage.md
§3.4](coverage.md). Redaction runs **before** attachment in all cases; a
redaction applied afterwards would pass every code-level test and still ship the
secret, so the ordering is asserted on the outbound bytes (conformance C18, C26,
C34).
