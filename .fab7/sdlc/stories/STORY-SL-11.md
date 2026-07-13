# STORY-SL-11 — `openbox dev verify` (auth/validate + signing round-trip)

**Risk:** low (a new read-only diagnostic subcommand; no credential mint, no config mutation)

## Source
- **Architecture:** `architecture.md` §1b interface 1 (developer onboarding), D5/OD16 (AIP signing); INV-1.
- **SDK parity target:** `openbox-temporal-sdk-python/openbox/config.py` `_validate_api_key_with_server()` — the SDK validates the key + signer against `GET /api/v1/auth/validate` at init so a `signing_required=true` agent fails fast with a clear error. Shift-left has no such preflight.
- **Session:** SDK↔shift-left gap analysis (2026-07-13) — bucket #2, item "auth/validate at init".

## User Value
After onboarding, a developer runs one command to confirm the *data-plane* path actually works — the `obx_` key is accepted and the Ed25519 signing round-trip passes against the configured core — turning a class of silent "no events arrive" failures (wrong key, missing verifier, clock skew, wrong core URL) into an immediate ✓ or a mapped, actionable ✗.

## Inlined context (verified — builder need not re-read)
- **The endpoint EXISTS:** openbox-core registers `GET /api/v1/auth/validate` → `groupAgent.ValidateToken` (`openbox-core/internal/api/main.go:118`). The SDK **signs** this GET (canonical `GET\n/api/v1/auth/validate\n<ts>\n<nonce>\n<sha256("")>`), so a `signing_required=true` agent passes — shift-left must sign it the same way (reuse `client/signing.go` `signer.sign`, which is method-agnostic).
- **Why a separate command, not inside `dev init`:** `dev init` talks to the **backend** (`agent/create`); `/auth/validate` is on **core**, whose base URL is set later in `dev.json` (`base_url`) and may be local vs prod. A standalone `openbox dev verify` reads the finished `dev.json` + creds and validates against the *actual* core it will emit to.
- **Creds/config resolution already exists:** `adapters/claude-code/creds.go` `ResolveIdentity` (DID) + `ResolveCredentials` (obx_ key + seed from OS keychain / file backend / env) and `dev.json` (`base_url`, `secret_file`, coordinates). Reuse these resolvers; do not re-implement secret I/O.
- **Diagnostics reuse SL-10:** map a non-200 to the SL-10 reason→guidance table.

## Acceptance Criteria
- New `client.Validate(ctx) error` — a **signed** `GET /api/v1/auth/validate` against the client's `base_url`; 200 → nil; non-200 → a mapped error (SL-10 reason table); reuses the same auth headers + AIP signing as `Emit` (INV-1: key only in `Authorization`, seed only in `ed25519.Sign`).
- New subcommand `openbox dev verify` — loads `dev.json` + resolves creds (reusing the existing resolvers), calls `client.Validate`, and prints a single ✓ line (`verified: <did> @ <base_url>`) or a ✗ with the mapped reason + fix hint; exit `0` on success, non-zero on failure. It is **read-only**: no mint, no config write, no secret print (INV-1).
- Honors the TLS guard (INV-1 `checkBaseURL`): refuses plaintext `http://` to a non-loopback core.
- `--dry-run` (or `--print-plan`) shows what it would call (method, path, base_url, DID) and makes **no** network call.
- Works against the local hybrid core (`http://localhost:8086`) and a real core alike.

## Nonfunctional Requirements
- **security:** never prints/logs the key, seed, signature, or nonce (INV-1); only the DID + base_url + status/reason.
- **usability:** one command; the failure line names the likely fix (via the SL-10 map) — e.g. "verifier_not_configured → set signing_required=false (RUNBOOK §3.2)".
- **reliability:** bounded timeout (reuse client default); a network failure is a clear ✗, not a hang.

## Write Scope
- `client/` — add `Validate(ctx)` (signed GET) to the client.
- `cli/` — new `dev verify` subcommand + wiring to the creds/config resolvers.

## Dependencies
- **Hard:** STORY-SL-3 (client + signer), STORY-SL-10 (reason→guidance map for the failure line).
- **Soft:** STORY-SL-2 (`dev init` produced the `dev.json` + creds this reads).
- **External (assumed-satisfied):** [EXT-core] — `/auth/validate` accepts the signed dev-agent request (it already exists for runtime agents; dev agents share the DID namespace, INV-7).

## Invariants
- **INV-1:** no secret in output/logs; TLS guard enforced.
- **INV-7:** validates the shared-namespace developer DID (no parallel identity).

## Human Gates
| Gate | Question | Owner | Evidence Needed | Allowed Outcomes |
|---|---|---|---|---|
| G3_REVIEW | Is the signed GET correct (matches core's verifier) and the command strictly read-only / secret-safe? | brian | diff review + a live ✓ against local core + a ✗ against a bad key | approve / revise |

## Validation
```bash
cd client && go build ./... && go vet ./... && go test ./...   # signed-GET unit test vs a known keypair; non-200 → mapped error
cd cli && go build ./... && go vet ./... && go test ./...
# live (hybrid stack up): openbox dev verify            -> ✓ verified: did:aip:… @ http://localhost:8086
#                          openbox dev verify (bad key)  -> ✗ signature_invalid / verifier_not_configured with fix hint, exit != 0
#                          openbox dev verify --dry-run   -> prints plan, no network
```

## Stop conditions
- If `/auth/validate` rejects the signed dev-agent request in a way that isn't a mappable reason (unexpected shape) → capture the exact status+body, surface it verbatim, and route to an EXT-core re-check; do not mask a real rejection as success.
- If no creds/`dev.json` are present → clear "run `openbox dev init` first" message, exit non-zero; never proceed with empty creds.
