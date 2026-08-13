# ADR-0015 — Credentials live in one plaintext file; the OS keychain is deleted

Status: Accepted — 2026-08-12.
Implements: `adapters/common/devconfig/paths.go`, `envfile.go`, `migrate.go`,
`devconfig.ResolveCredentials`, `cli/cmd/openbox/auth.go`.
Deletes: `cli/internal/secret/` (the whole package), `devconfig.OSSecretLookup`,
`devconfig.FileSecretLookup`, `devconfig.SecretLookup`, `DevConfig.SecretFile`,
`devinit.(Options).accounts`, and the `--secret-backend` flag.
Reverses: the posture stated in `cli/internal/secret/file.go:12-32` and
`secret.go:10-13` — keychain by default, HALT rather than fall back to plaintext.
Related: ADR-0016 (what a bare `openbox init` does), ADR-0007 (shared devconfig).

## Context

`openbox` stores two credentials per developer: an `obx_` runtime API key and an
Ed25519 signing key. Until now they went into the platform secret store —
libsecret via `secret-tool` on Linux, the login keychain via `security` on macOS
— and the CLI refused to run anywhere else. The prior reasoning is on disk and
deserves to be quoted rather than paraphrased, because this ADR argues against
it.

On the store being mandatory (`cli/internal/secret/secret.go:13`):

> `other: none; Detect returns ErrNoStore so the caller HALTs (INV-1)`

On the file backend that existed as an escape hatch
(`cli/internal/secret/file.go:17-25`):

> never selected automatically — the operator must ask for it … plaintext at
> rest, which is the one guarantee it trades away vs. the keychain. That is the
> conscious, warned tradeoff — hence opt-in only.

That posture cost more than it bought, in four ways.

**Windows had no story at all.** `Detect` returns `ErrNoStore` for every GOOS
that is not linux or darwin, so on Windows every credential-writing command
halted. Closing that gap meant a DPAPI backend behind a `_windows.go` build tag,
a stub for everything else, and `golang.org/x/sys` as a storage dependency — a
design researched and drafted before being abandoned
(`plans/260812-1212-openbox-auth-command/research/researcher-01-windows-dpapi-report.md`,
superseded). Three code paths for one map lookup.

**Three config paths for one product.** `os.UserConfigDir()` resolves to
`~/Library/Application Support/openbox`, `~/.config/openbox`, and
`%AppData%\openbox`. Nothing about this product needs that variance.

**The store was not actually protecting the credential from its own threat
model.** The governed coding agent runs arbitrary commands as the developer. On
macOS the login keychain is unlocked for the length of a desktop session and the
`security` binary will read it out; on Linux the same is true of an unlocked
libsecret collection. So the agent whose behaviour OpenBox governs could already
read the seed it signs with. Encryption at rest defends against a stolen disk,
which is a real threat — but it was being presented as though it defended against
the process on the other side of the hook, and it never did.

**Two stores meant two copies of one field.** The keychain held a DID and so did
`dev.json`, and `devinit.go:198-208` read the keychain's copy and wrote it into
the config. A stale keychain entry therefore reverted a corrected DID on the next
`init`, silently.

## Decision

Credentials go in **one plaintext file, `~/.openbox/.env`**, and the OS keychain
support is deleted rather than demoted.

- `~/.openbox/` (override with `OPENBOX_HOME`) is the single config directory on
  every OS, including Windows (`C:\Users\<you>\.openbox\`). `~/.aws`, `~/.kube`
  and `~/.docker` establish the convention.
- `.env` is dotenv format, `0600`, under a `0700` parent, written atomically
  (temp + rename), with a header comment stating that it is the only copy of
  once-shown credentials and must not be committed.
- Resolution precedence for a secret is **real environment variable >
  `~/.openbox/.env` > unset**. The file sits exactly where the secret store sat
  and nowhere else. Sourcing the file is never required.
- No dependency is added for storage. Dotenv parsing and writing are hand-rolled
  (~120 lines including the edge cases) rather than pulling a library for a
  format this repo writes and reads on both ends.

### `.env` holds secrets; `dev.json` keeps the coordinates

The two files do not overlap, and that is a decision, not an accident.

| File | Holds | Keys |
|---|---|---|
| `.env` | **secrets only** | `OPENBOX_API_KEY`, `OPENBOX_AGENT_PRIVATE_KEY`, and on approver installs `OPENBOX_CONTROL_TOKEN` |
| `dev.json` | posture **+ the non-secret coordinates** | `developer_did`, `agent_id`, `backend_url`, `base_url`, plus every posture field and the managed/`Locked` layer |

One store per field. The two-DID-stores bug above is the bug class this avoids,
and it is worth naming because the tempting shortcut — "put the DID in `.env`
too, it's right there" — reintroduces it exactly. Coordinate resolution is
therefore left untouched at **real env var > `dev.json` > built-in default**, and
a DID written into `.env` by hand is *ignored*. A test pins that.

A consequence worth stating plainly: **a `.env` alone is not enough to run.** It
carries no DID, so `dev.json` (or the environment) must supply the coordinates. A
user who copies only `.env` to a new machine gets the no-DID error, which is the
correct failure and not an obscure one.

The alternative — moving the coordinates into `.env` as well, so one file is the
whole config — was rejected on cost: 12 files and 61 references read those fields
from `DevConfig`, and re-pointing them buys no behaviour.

### The dependency: `golang.org/x/term`

This is the repo's **first external Go dependency**. Until now the dependency
graph was standard library plus the repo's own modules, and that is a property
worth spending deliberately rather than by accident.

It is not for storage. It is for **input**: `term.ReadPassword` masks a pasted
credential, and `term.IsTerminal` decides whether to mask at all. The stdlib has
no equivalent that works on Windows — `os.Stdin.Stat()` misreports console
handles there, because a Windows console sets `ModeCharDevice` but not
`ModeDevice` ([golang/go#23123](https://github.com/golang/go/issues/23123)) — and
`stty -echo` is unix-only. `x/term` drives `GetConsoleMode`/`SetConsoleMode`
directly on Windows, which is the whole reason to take it.

Pinned as **`golang.org/x/term v0.34.0`**, which pulls **`golang.org/x/sys
v0.35.0`** transitively and nothing else — exactly the two-module footprint this
decision budgeted for. Deliberately not the latest: `x/term v0.35.0` and up
declare `go 1.24.0`+ (v0.45.0 wants `go 1.25.0`), so taking one would raise this
repo's language floor from 1.23 across all eleven modules and `go.work` — a
toolchain decision with its own blast radius, arriving disguised as a dependency
bump. v0.34.0 is the newest release still declaring `go 1.23.0`, so the floor
stays where [ADR-0011](ADR-0011-multi-module-layout.md) put it.

One visible consequence: `cli/go.mod` and `go.work` now read `go 1.23.0` rather
than `go 1.23`, because a module must declare a version at least as specific as
its dependencies'. Same language version with the patch component spelled out; no
`toolchain` directive is pinned, so any Go 1.23.x still builds the repo.

Being honest about what this buys: masking keeps the credential out of terminal
scrollback, screen shares, tmux buffers and recorded sessions, and off argv. **It
does not protect the value at rest** — the next thing that happens to it is being
written to a plaintext file. Anyone reading the masked prompt as evidence of
protection has read it wrong, and the package doc comment says so.

## What this weakens

Two exposures, and they are not the same size. Folding them into one sentence
about "credentials" would hide the larger one, so they get separate headings.

### The agent signing seed

`~/.openbox/.env` holds the Ed25519 private key this machine signs governance
events with. Anything running as the developer can read it and sign events as
this agent: an ordinary process, a shell one-liner, a malicious dependency's
postinstall script — and **the coding agent under governance**, which by design
executes arbitrary commands as that user.

So attestation proves **origin-of-config**, not tamper-resistance against the
developer or against the agent they run. A commit attestation
([ADR-0010](ADR-0010-signed-commit-attestation.md)) says "a machine holding this
agent's key signed this", not "this agent's behaviour is what produced it". That
distinction was always true — the keychain did not change it, for the reason
given in Context — but the plaintext file makes it obvious, and obvious is
better.

### The organization control token — a strictly larger blast radius

On an approver install (`openbox auth --role approver`), `.env` also holds
**`OPENBOX_CONTROL_TOKEN`**. When that is an `obx_key_…` organization key rather
than a short-lived Keycloak JWT, it carries **org-wide create and rotate
authority over the agent fleet**. Today it is read from the OS store as a
fallback (`cli/cmd/openbox/approve.go:96-114`); after this ADR it is read from
the plaintext file.

The seed compromises **one agent**. This compromises **every agent in the
organization** — it can mint new ones and rotate existing ones' credentials out
from under them. Anyone deploying an approver install should treat the machine
accordingly: prefer a short-lived JWT over an `obx_key_` where the deployment
allows it, and do not put an approver install on a shared or developer-managed
host.

`auth` never persists an org key it was merely *using* — the token supplied for
registration or `--rotate` is read from the environment and dropped. It lands in
`.env` only on an approver install, where the whole point is that `openbox
approve` can run later without one in the environment.

### Windows has no at-rest protection whatsoever

| OS | `0600` on `.env` | Effect |
|---|---|---|
| macOS, Linux | real, owner-only | other local users cannot read it; root and anything running as the user can |
| **Windows** | **no-op** | `os.Chmod` only toggles the read-only attribute; the file inherits the parent ACL, so **other local accounts can read it** |

The code sets `0600` unconditionally because doing so is correct where it works
and harmless where it does not, but on Windows it buys nothing. No ACL
manipulation is attempted: doing it properly means `golang.org/x/sys/windows`
security descriptors, which is the kind of platform-specific storage code this
decision exists to remove. A Windows deployment that needs at-rest protection
should use full-disk encryption and not treat `.env` as protected.

### Existing keychain credentials are stranded

**Nothing reads the OS keychain before it is deleted.** There is no automatic
migration, by decision: writing a keychain reader in order to delete the keychain
means shipping the platform-specific code path this ADR removes, for one run per
machine.

An existing macOS/Linux install therefore has credentials in a store the new
binary cannot see. Three recovery paths, in the order most people should try
them:

1. **`openbox auth --rotate`** — re-issues both credentials for the same agent,
   preserving its id and DID. Needs an org key. This is the intended path.
2. **Read them out by hand and paste them into `openbox auth`** — no org key
   needed, keeps the existing credentials as they are:

   ```bash
   # macOS
   security find-generic-password -s ai.openbox.dev -a '<org>/<provider>/api_key' -w
   # Linux
   secret-tool lookup service ai.openbox.dev account '<org>/<provider>/api_key'
   ```

   The account names are `<org-or-"local">/<provider>/{api_key,private_key,did}`.
3. **Register a fresh agent** — `openbox auth` with a blank agent id. Loses the
   old agent's history continuity.

The missing-credential error text names path 1 and path 2 directly, because that
error is where a stranded user actually meets this decision. The migration note
in `docs/getting-started.md` carries the same commands.

The asymmetry is deliberate and worth stating: **`dev.json` and `approver.json`
migrate themselves** from `os.UserConfigDir()` on first run (non-destructively,
old file left in place), because that is a plain file copy with no
platform-specific reader. Credentials do not, because they would need one.

### The old opt-in file backend leaves a live copy behind

Anyone who used `--secret-backend file` has a `secrets.json` under
`os.UserConfigDir()/openbox/` holding the same credentials in the same plaintext.
Deleting the code does not delete the file. It should be removed by hand — it is
a stale copy of live credentials, and stale copies are worse than current ones
because nobody rotates them. The migration note says so.

## Consequences

**Gained**

- One code path on three operating systems. No build tags, no `x/sys` for
  storage, no `Detect`/`Open`/`Store` indirection, no `ErrNoStore` HALT.
- **Windows works at all.** It previously could not store a credential.
- One config directory instead of three.
- The two-DID-stores revert loop is structurally gone: one DID, one file.
- A whole category of untestable integration disappears. There is no keychain to
  mock, no `secret-tool` to install on a CI runner, no interactive keychain
  unlock prompt in a headless run. The dotenv codec and the resolution
  precedence are pure Go, so unit tests genuinely cover them on every OS — which
  is a real gain in *verifiability*, not just in file count.
- Credentials are inspectable. `cat ~/.openbox/.env` answers "what does this
  machine think it is" in one command, which is most of what support asks for.
- Tests inject through `OPENBOX_HOME` + `t.Setenv` rather than a `SecretLookup`
  function seam, which is less machinery.

**Lost — the accepted trade-offs**

- **At-rest encryption, everywhere.** This is the whole cost. On macOS/Linux it
  is replaced by `0600`; on Windows by nothing.
- **The HALT guarantee.** The CLI no longer refuses to store a credential
  insecurely; storing it insecurely is now the only thing it does. `INV-1` is
  narrowed accordingly: it still holds that no secret reaches argv, shell
  history, logs, or repo history — the guarantees the prompt and the file mode
  defend — but "not in a plaintext file" is no longer one of its claims.
- **An org key sits on an approver's disk in the clear** (see above).
- **Existing installs break until the user acts** (see above).
- A `.env` in the wrong place is a credential leak with no encryption behind it.
  Compensating controls: `0600`, the header comment, and the file living in
  `~/.openbox/` rather than anywhere near a repo.

## Alternatives rejected

**Keep the keychain as the default and add DPAPI for Windows.** The
fully-researched option, and the plan of record until 2026-08-12. It preserves
at-rest encryption on all three OSes. It costs a `_windows.go`/stub build-tag
split, `golang.org/x/sys` as a storage dependency, three untestable
platform-specific code paths, and it keeps both the three-config-path problem and
the two-DID-stores bug. Rejected because the protection it preserves does not
defend against the process this product exists to govern (see Context), so the
complexity bought less than it appeared to.

**Windows Credential Manager instead of DPAPI.** Same objection, plus a 2560-byte
per-blob limit and a CLI (`cmdkey`) that cannot read a secret back out, which
would have meant `x/sys` syscalls regardless.

**Keychain when present, plaintext file when not, chosen automatically.** The
smallest change from today — it just deletes the HALT. Rejected because it is the
worst of both: two storage paths to maintain and test, a security posture that
differs per machine with no way to tell from the outside which one a given
developer got, and a silent fallback to plaintext, which is precisely what
`file.go:19` refused to do on purpose. If plaintext is acceptable it should be
acceptable visibly and everywhere.

**Encrypt `.env` with a key derived from a passphrase.** Real at-rest
protection, portable, no platform code. Rejected because the hooks run
non-interactively on every tool call — there is nobody to type a passphrase — so
the key would have to be cached somewhere readable by the same processes that can
already read `.env`, which is encryption whose key sits next to the ciphertext.

**A dotenv library.** `godotenv` or similar. Rejected to keep the external
dependency count at exactly one deliberate entry; the format this repo needs is
~120 lines including CRLF handling, quoting, and the duplicate-key error, and
this repo writes both ends of it.
