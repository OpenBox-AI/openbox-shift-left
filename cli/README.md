# `openbox` CLI (STORY-SL-2)

The developer-runtime governance front door. One command onboards a coding tool
to OpenBox: register a developer agent, capture its credentials into the OS
secret store, and delegate the tool's native config to that provider's adapter.
Governance is ambient thereafter.

```
openbox dev init --provider <claude-code|codex|cursor> [flags]
```

## What `dev init` does

1. **Register** a developer agent via the backend control plane
   `POST /agent/create` with `agent_type="developer"` and a default developer
   AIVSS risk posture (`internal/aivss`). Org scope (INV-4) and the `did:aip:`
   DID (INV-7) come from the backend.
2. **Capture credentials** — the once-shown `obx_` API key and the base64 raw
   32-byte Ed25519 seed — into the **OS secret store** (`internal/secret`).
   These are the runtime credentials SL-3 uses to AIP-sign `/evaluate` calls.
3. **Delegate config** to the selected provider's installer (`internal/provider`).
   Until an adapter ships (SL-4 Claude Code, SL-7 Codex, SL-8 Cursor) the CLI
   prints the required manual config and exits non-zero (code 2).

## Flags & environment

| Flag | Env | Notes |
|---|---|---|
| `--provider` | — | `claude-code` \| `codex` \| `cursor` (required) |
| `--org` | `OPENBOX_ORG` | namespace for credential storage |
| `--backend-url` | `OPENBOX_BACKEND_URL` | openbox-backend base URL |
| `--client-id` | `OPENBOX_CLIENT` | `x-openbox-client` header (Keycloak JWT path) |
| `--dry-run` | — | print the plan; **no** network / secret-store writes |
| `--force` | — | register a new distinctly-named agent even if one exists |
| `--managed-enable` | — | record org force-enable substrate (verified, not activated) |
| — | `OPENBOX_CONTROL_TOKEN` | **required for a real run**; see below |

The control-plane credential is a Keycloak Bearer JWT **or** an org control-plane
key (`obx_key_…`, sent as `X-API-Key`) — auto-detected by prefix.

## Security posture (for G_SEC review — Sam)

- **INV-1, credential handling.** The `OPENBOX_CONTROL_TOKEN` is read only from
  the environment — never a flag — so it cannot leak via `argv`/`ps`/shell
  history. The minted `obx_` key and Ed25519 seed go straight to the OS secret
  store and are never printed, logged, or written to a config file. Command
  output shows only the secret-store *reference* (service + account) and the
  non-secret DID.
- **Secret-store backends.** Linux uses `secret-tool` (libsecret) and passes the
  secret on **stdin** (no argv exposure). macOS uses the `security` CLI; note the
  **argv caveat** documented on `keychainStore` in `internal/secret/backends.go`
  (`security add-generic-password -w <value>` transiently exposes the value via
  `ps` — no no-cgo alternative through that binary). If no store is available the
  CLI **HALTs** rather than write plaintext.
- **Idempotency reality.** `agent/create` shows the key + seed exactly once and
  has no upsert. Local secret-store presence is therefore the idempotency key: a
  re-init with stored creds does no network call. An agent that exists remotely
  but whose creds were never stored locally is unrecoverable — the CLI refuses to
  duplicate and explains the options (delete + re-run, or `--force`).

## Layout

```
cmd/openbox/        entrypoint + subcommand routing
internal/aivss/     default developer AIVSS posture (integers verified vs backend)
internal/backend/   control-plane client: agent/create + agent/list
internal/secret/    OS secret store abstraction + backends
internal/provider/  adapter-installer seam (stubs until SL-4/7/8)
internal/devinit/   the dev-init orchestration
```

## Build & test

```bash
cd cli && go build ./... && go vet ./... && go test ./...
```
