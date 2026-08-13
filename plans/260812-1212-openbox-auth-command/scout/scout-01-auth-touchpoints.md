# Scout — `openbox auth` touch points

Verified against source at commit on branch `fix/tier2-duplicate-activity-started`, 2026-08-12.
Line numbers are current; re-check before editing.

## Decisions already taken (user, this session)

1. **Scope: credentials only.** `auth` owns identity — prompt, store, register-if-no-DID, rotate.
   `init` keeps owning the plugin bundle, hooks, posture, dev.json posture fields.
2. **Windows: DPAPI-encrypted file**, user-scoped. Not Credential Manager.
3. **`auth --rotate` included** — re-issue credentials for an agent that already exists remotely.

## Where the command hooks in

| Seam | Path | Note |
|---|---|---|
| Subcommand dispatch | `cli/cmd/openbox/main.go:81` `run()`, cases at `:87`+ | add `case "auth"` beside `init`/`dev`/`doctor` |
| Usage text | `main.go` help block (`openbox --help`) | must list `auth` |
| Store selection | `cli/internal/secret/secret.go:49` `Detect()`, `:71` `Open(kind)` | `Detect` switches `runtime.GOOS`; **only** linux+darwin, else `ErrNoStore` (`:32`) |
| Store contract | `cli/internal/secret/secret.go:36` `Store` interface | `Name/Set/Get/Delete` — a Windows backend implements exactly this |
| Service namespace | `cli/internal/secret/secret.go:25` `Service = "ai.openbox.dev"` | unchanged |
| Account naming | `cli/internal/devinit/devinit.go:106` `accounts()` | `<org|"local">/<provider>/{api_key,private_key,did}` — **reuse, do not reinvent** |
| Config write | `adapters/common/devconfig/write.go:53` `WriteConfig`, `provider/config.go:17` `ConfigUpdate(ref)` | dev.json is written from a `provider.CredentialRef` |
| Runtime read | `adapters/common/devconfig/devconfig.go:457` `ResolveCredentials` | env wins over store: `OPENBOX_API_KEY` / `OPENBOX_ED25519_SEED`; DID via `ResolveDID` (`:225`) |
| Backend client | `cli/internal/backend/client.go:69` `New(baseURL, credential, clientID)` | `obx_key_` → `X-API-Key`; else `Authorization: Bearer` (`:75`) |
| Existing calls | `client.go:133` `Create`, `:175` `FindByName`, `:301` `GetCurrentPolicy`, `:341` `do()` | rotate methods go here, same `do()` |

## New backend endpoints needed (verified in openbox-backend)

Both preserve the agent row, its id, and its DID — `didFor(agentId)` derives the DID from the
agent id, so rotation cannot change it.

- `POST /agent/:agentId/rotate-api-key` → `{ agent, token }`, plaintext new `obx_` runtime key
  (`openbox-backend/src/modules/agent/agent.service.ts:1537`; controller `:994`)
- `POST /agent/:agentId/identity/rotate` → `{ did, privateKey }`, seed base64
  (`agent.service.ts:1095`; controller `:979`; providers `did/local-identity-provider.ts:36`,
  `did/aws-kms-provider.ts:78` — **both** return the private key, so KMS mode is not a blocker)
- Both require `PermissionEnum.UpdateAgent` on the calling org key.

## Constraints the design must respect

- **INV-1: secrets never on argv.** Existing control token is env-only precisely so it cannot leak
  via shell history. `auth` must take secrets from **stdin/prompt or env**, never a flag.
  A masked prompt strengthens INV-1; a `--api-key` flag would break it.
- **`cli/go.mod` has zero external dependencies** — intra-repo modules only. Both OS backends
  shell out to a platform CLI (`security`, `secret-tool`) rather than linking a library. Any new
  dependency is a departure needing explicit justification.
- **`Detect()` halting is deliberate** (`secret.go:13`): no OS store ⇒ `ErrNoStore` ⇒ the caller
  HALTS rather than silently falling back to plaintext. A Windows backend must slot in here, not
  bypass it.
- Build must stay green cross-platform: package has **no build tags today**, so a Windows backend
  needs `_windows.go` / non-windows stub so `go build ./...` on macOS is unaffected.

## Defects this command should design around (found this session, filed separately)

1. **`init` cannot update credentials.** `devinit.go:198` short-circuits when the store already
   holds an api_key+private_key: it reuses them and returns before any network call. So re-running
   `init` is never an update path. `auth` is the answer to that — it must **overwrite**
   unconditionally (the macOS `-U` flag already does in-place update, `secret/backends.go:84`).
2. **The DID lives in two stores with different readers.** Runtime reads dev.json only
   (`ResolveDID`, `devconfig.go:225` → env → `cfg.DID`); the keychain `did` account is read by
   exactly one caller, `devinit.go:200` (init's reuse path), which then writes it into dev.json.
   Updating one and not the other silently reverts. **`auth` must write both, atomically in intent.**
   Consider whether the keychain `did` copy should exist at all.
3. **Reuse path leaves `agent_id` empty.** `ref.AgentID` is only set on the registration branch
   (`devinit.go:267`), so a reuse-path init writes dev.json without `agent_id` → `dev sync` and the
   session-start staleness check cannot fetch policy → posture reports `bundle_version: no-policy`.
   `auth` knows the agent id (input or from registration/rotation) and should persist it.

## Field set for the prompt

| Field | Source if empty | Secret? | Notes |
|---|---|---|---|
| org | `--org`, else `local` | no | drives the account namespace (`accounts()`) |
| backend URL | dev.json, else default | no | control plane |
| base URL (core) | dev.json, else default | no | data plane; self-hosted must set it |
| agent id | from registration/rotation | no | needed for `dev sync` — see defect 3 |
| DID | **empty ⇒ register a new agent** (user's stated rule) | no | `did:aip:…` |
| api key (`obx_`) | from registration/rotation | **yes** | runtime key, NOT `obx_key_` — see `credential.go:25` |
| Ed25519 seed | from registration/rotation | **yes** | base64, 44 chars |
| control token | env `OPENBOX_CONTROL_TOKEN` | **yes** | `obx_key_` org key; only needed when registering/rotating |

**Validation worth building in:** reject an `obx_key_` pasted into the api-key field. That exact
mix-up cost hours this session; `credential.go:31` `controlTokenProblem` already implements the
mirror-image check and is the model to copy.

## dev.json write strategy — RESOLVED

`WriteConfig` (`adapters/common/devconfig/write.go:53`) is a real merge, not a replace:

- `Update` is tri-state by design (`write.go:16-36`): empty string / nil pointer ⇒ "this run did
  not mention the setting" ⇒ the on-disk value is carried forward (`setString/setBool` `:99-115`).
- The merge starts from what is on disk and overlays only what the run supplied, so fields nobody
  thought about — including ones added later — survive. Nothing is lost by omission.

Therefore **`auth` can do a partial, identity-only write**: supply `DID`, `SecretService`,
`APIKeyAccount`, `PrivateKeyAccount`, `AgentID`, and the URLs; leave **every posture pointer nil**
(`Enforce`, `Tier2`, `Findings`, `ContentCapture`, `InstallGitHook`). Posture is then provably
untouched, which is exactly the "credentials only" scope the user chose. No new write path needed.

Two caveats for the plan:

- `setString` means an empty value can never *clear* a field — `auth` can change a coordinate but
  never blank one. If clearing is ever needed (e.g. moving off a file backend), it needs a
  different mechanism; out of scope here, but do not design as if omission clears.
- `WouldDowngradeEnforce` (`write.go:91`) exists so posture downgrades are announced rather than
  silent. `auth` passing nil never trips it — which is the correct behaviour and worth an
  assertion in tests, since a regression here would silently drop a developer out of enforce.

## Unresolved

- Whether the keychain `did` account should continue to exist at all (defect 2 above). Its only
  consumer is `init`'s reuse path, and that consumer is what reverted dev.json to a stale agent
  this session. Removing it and reading the DID from dev.json would be simpler and safer, but it
  changes init's recovery behaviour when dev.json is missing — a deliberate call, not a drive-by.
- Whether `auth` should refuse to run when `OPENBOX_API_KEY` / `OPENBOX_ED25519_SEED` /
  `OPENBOX_AGENT_DID` are set in the environment. Those override the store at runtime, so storing
  credentials while an env override is active produces a config that silently does not take effect
  — the exact failure mode that cost hours this session. Recommend: warn loudly, do not refuse.
