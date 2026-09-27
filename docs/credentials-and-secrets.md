# Credentials and secret detection

Where this machine's credentials live, what protects them (and what does not),
and what the local secret redactor catches. For what leaves the machine, see
[Data and privacy](data-and-privacy.md).

## Where credentials live

All of these files are **plaintext**. Nothing in them is sent to OpenBox, but
nothing encrypts them at rest either.

**One store per governed tool:**

```
~/.openbox/<tool>/.env
    OPENBOX_API_KEY='obx_…'            # the agent's API key
    OPENBOX_WORKLOAD_PRIVATE_KEY='…'   # the agent's RS256 private key
~/.openbox/<tool>/dev.json
    agent_id                           # a UUID; not a secret
~/.openbox/<tool>/workload-token.json
    a cache of the current access token (valid at most 270 seconds; safe to delete)
```

**One organization-level store**, shared by all tools:

```
~/.openbox/.env
    OPENBOX_CONTROL_TOKEN='obx_key_…'  # written by `openbox auth`
~/.openbox/dev.json
    backend_url, base_url              # copied into each tool's dev.json by `init`
```

A file holds either secrets (`.env`) or coordinates (`dev.json`), never both,
and no value is stored in two places. An old install once kept the same value
in two stores, and a stale copy kept silently overwriting a corrected one.

### What protects them

- **macOS and Linux:** the files are `0600` in a `0700` directory. Other users
  cannot read them. **Anything running as you can**, including the coding agent
  being governed, which runs arbitrary commands as you.
- **Windows:** no protection. `0600` does nothing there, and other local
  accounts can read the files. Use full-disk encryption.
- **They are the only copy.** OpenBox shows the API key and private key once,
  at registration, and does not keep them. If you lose a tool's `.env`, the
  next `init` registers a new agent. Only the adopt prompt keeps the same
  agent, and it needs the original private key.
- **Never commit them.** They live in your home directory, away from repos,
  for that reason.

So a signed event proves that *a machine holding this agent's key*
produced it. It does not prove the developer, or the agent they run, could not
have tampered with it.

### The organization key is the sensitive one

An `obx_key_…` organization key can create agents across your whole
organization. One tool's key compromises one agent; this key compromises the
fleet.

`auth` saves it to `~/.openbox/.env` because `auth` and `init` are separate
processes. It is never written to a per-tool store, and never accepted as a
command-line flag. To keep it off disk entirely, skip it at the `auth` prompt
and export `OPENBOX_CONTROL_TOKEN` for the one `init` run. `openbox uninstall`
deletes the file.

### Precedence

An environment variable beats the file, so CI can supply credentials without
writing to disk:

```
secrets       OPENBOX_API_KEY, OPENBOX_WORKLOAD_PRIVATE_KEY  env > ~/.openbox/<tool>/.env
coordinates   OPENBOX_AGENT_ID, …                            env > ~/.openbox/<tool>/dev.json > default
org key       OPENBOX_CONTROL_TOKEN                          ~/.openbox/.env > env
```

An exported variable applies to **every** tool at once: one `OPENBOX_AGENT_ID`
makes every governed tool report the same agent. `openbox doctor` shows which
source is in effect.

The organization key runs the other way because it is the only value passed
between two commands. If the environment won, a forgotten `export` in an old
shell would silently override every `auth` you ran.

### Leftover temp files

Credential writes are atomic (write a temp file, then rename). A process killed
in between can leave a `~/.openbox/**/.env-*.tmp` holding the same secrets.
`openbox uninstall` finds and deletes them.

## The transport CA key is a credential too

`~/.openbox/transport-ca.pem` and `transport-ca.key` exist once the transport
lane is installed. The key has the same protection as `.env`: `0600` on macOS
and Linux, none on Windows, readable by anything running as you.

The CA is **not restricted to specific hosts**. What limits the lane is the
per-provider allowlist of hosts it decrypts
(`internal/transport/hosttable.go`), not the certificate. So a leaked key
could forge a certificate for **any** site that trusts this CA.

- On every platform, only the governed tool trusts the CA (through
  `NODE_EXTRA_CA_CERTS` in its settings).
- **On macOS**, `init` also trusts it in the System keychain, so every app and
  browser on the machine trusts it.

`openbox uninstall` untrusts the CA, then deletes the key. If untrusting fails,
it still deletes the key (a trusted certificate with no key cannot sign
anything new) and prints the command to remove the keychain entry by hand.

A machine with a CA from an older install, which was restricted to one host,
keeps working: hosts it cannot sign for are passed through undecrypted.
`openbox doctor` reports it, and re-running `openbox init` replaces it.

## Secret detection stays local

Every body is scanned on your machine before it is attached to an event: the
prompt, the reply, thinking, tool input and output, and refusal reasons. A
match is replaced with a placeholder, and only the category (`aws_key`,
`entropy`, …) is logged, never the value.

On a gated `Write` or `Edit`, the redaction is applied to the **file itself**:
the file is written with the placeholder in place of the secret.

Redaction protects what egresses, not what stays on disk: the
[local trace](data-and-privacy.md#the-local-trace) keeps every body before and
after redaction, so any secret the scanner saw (and any it missed) is in
plaintext under the runtime directory's `trace/`, for up to 7 days.

The detector lives in `internal/decision/`. It has two layers:

1. **Format rules**: nine local patterns (`secrets.go`) plus gitleaks' rule set
   (`gitleaks.go`). These match a known credential by its shape, anywhere.
2. **Keyword and entropy**: a value next to a credential-like key name
   (`api_key=…`, `"password": …`), or a long high-entropy token in a value
   position.

### What the scanner catches; and where it stops

Measured against the real detector
(`TestContentCaptureCredentialCoverage` in the conformance suite):

| Input | Redacted? | Why |
|---|---|---|
| an AWS, GitHub, Stripe, JWT or `sk-` key, anywhere | yes | matched by shape |
| a GitLab, Shopify, Twilio, Grafana or other known token | yes | a gitleaks rule |
| `OPENBOX_API_KEY=obx_…` | yes | credential keyword |
| `OPENBOX_WORKLOAD_PRIVATE_KEY=<long base64>` | yes | private-key keyword with a long base64 value |
| a PEM private key block | yes | dedicated pattern |
| `API_KEY=<64 hex chars>` | yes | keyword match |
| `{"password":"…"}` nested in JSON tool output | yes | patterns handle JSON quoting and escaping |
| **`AWS_ACCESS_KEY_ID=<value in no known format>`** | **no** | the keyword must sit right next to the `=`, and `_ID` is in the way |
| **`DEPLOY_HEX=<64 hex chars>`** | **no** | no keyword, and hex never reaches the entropy threshold |

**Why the entropy threshold is not lower.** It is set above what hex strings
can reach, so git SHAs, UUIDs and hashes are never flagged. On a file write
the redactor rewrites the file, so a false positive corrupts real content.

**Known false positive:** a long base64 string assigned in source code
(`x := "<48 base64 chars>"`) can be redacted on write. If you keep base64
fixtures in source, generate them in code instead.

If your secrets fall into the "no" rows, turn `content_capture` off, or keep
them out of the working directory of a governed session.
