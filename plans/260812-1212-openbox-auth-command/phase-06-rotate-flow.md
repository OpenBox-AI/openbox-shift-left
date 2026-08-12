# Phase 06 — Rotate client methods + `--rotate`

## Context links

- Parent: [plan.md](plan.md) · Depends on: phase 5 (`auth.go` exists)
- Backend source: `openbox-backend/src/modules/agent/agent.service.ts`,
  `agent.controller.ts`, `src/modules/did/*`

## Overview

- **Date:** 2026-08-12
- **Description:** Two new backend client methods and an `auth --rotate` flow that
  re-issues credentials for an agent that already exists remotely, preserving its
  id and DID.
- **Priority:** P1 · **Implementation status:** implemented 2026-08-13 · **Review status:** self-reviewed

## Key Insights

- This closes the dead end that cost a full debugging session: agent exists
  remotely, its credentials were shown once and are lost, and `init` refuses with
  *"already exists … but no local credentials are stored."* Rotation is the only
  path that keeps the agent.
- **The DID survives rotation.** Both providers return `{did: didFor(agentId),
  privateKey}` — `local-identity-provider.ts:36→64`, `aws-kms-provider.ts:78→97`.
  `didFor` derives the DID from the agent id (`did/naming.ts:8`), so it cannot
  change. KMS mode is **not** a blocker: it generates locally and imports, so the
  private key is still returned.
- **An `obx_key_` org key goes in `X-API-Key`, not `Authorization: Bearer`**
  (`cli/internal/backend/client.go:75`). Getting this wrong yields a bare 401.
- **Undocumented precondition:** `rotateAgentIdentity` 404s with *"Agent identity
  has not been provisioned"* when `signing_required === null`
  (`agent.service.ts:1096-1099`). That is **not** "no such agent" and needs its own
  message, or a user will chase a nonexistent agent.
- **Load-bearing assumption, flagged as inference not evidence:**
  `AgentIdentityResponseDto` (`agent-response.dto.ts:592-604`) declares
  `did`/`key_id`/`key_arn`/`public_key` but **not** `privateKey` — which is what the
  service actually returns. A grep found no `ClassSerializerInterceptor` anywhere in
  backend `src/` (only `TransformInterceptor`, `app.module.ts:119`), so nothing
  strips it today. If anyone adds one, every rotation silently returns nothing
  usable. Hence the fail-loud guard below. **This is a live landmine in
  openbox-backend and worth a ticket there.**
- Rotation invalidates the previous key server-side and writes a `SECURITY_EVENT`
  audit entry. Both are correct and expected; say so in the confirm prompt.

## Requirements

1. `backend.Client.RotateAPIKey(ctx, agentID) (token string, err error)` —
   `POST /agent/{id}/rotate-api-key`, reuses `client.do()` (`client.go:341`).
2. `backend.Client.RotateIdentity(ctx, agentID) (did, privateKey string, err error)`
   — `POST /agent/{id}/identity/rotate`.
3. **Fail loud** when a 2xx response omits `token` or `privateKey`: error naming the
   field and the DTO-drift risk. Never write a partial credential set.
4. Error mapping: 403 ⇒ "org key lacks `update:agent`"; 404 with the
   *not provisioned* body ⇒ its own message distinct from unknown-agent; 401 ⇒
   "wrong credential type — an org key is `obx_key_…`".
5. `auth --rotate` requires an agent id (prompt if absent), confirms explicitly
   (naming: previous key invalidated, DID preserved, audit event written), rotates
   **both** key and identity, then writes `.env` + dev.json exactly as phase 5 does.
6. Rotation is skipped entirely without `--rotate`; no implicit rotation, ever.
7. `--yes` skips the confirm for automation, and only then.

## Architecture

```
auth --rotate
  ├─ resolve agent id (flag/.env/dev.json/prompt)
  ├─ confirm (explicit consequences)      <- skipped only by --yes
  ├─ RotateAPIKey     -> token
  ├─ RotateIdentity   -> did, privateKey
  ├─ guard: both non-empty, token is obx_ (not obx_key_), key is 32-byte b64
  └─ write .env + dev.json  (phase 5's writer, unchanged)
```

Both calls go through the existing `do()`; no new HTTP plumbing. Order is
key-then-identity so a mid-sequence failure leaves the agent with a working
signing identity rather than a working key it cannot sign with.

## Related code files

| Path | Why |
|---|---|
| `cli/internal/backend/rotate.go` | new — the two methods |
| `cli/internal/backend/client.go:69-80,341-365` | `New` auth-header selection; `do()` to reuse |
| `cli/cmd/openbox/auth.go` | the `--rotate` branch |
| backend `agent.controller.ts:979,994` | route shapes + `UpdateAgent` permission |
| backend `agent.service.ts:1095-1101,1537-1570` | return shapes + the null precondition |

## Implementation Steps

1. `rotate.go` with both methods and typed responses that tolerate a
   `{status,data:{…}}` envelope (search the decoded body for the field rather than
   assuming depth).
2. Unit tests against an `httptest` server: 200 happy path, 200-missing-field
   (⇒ fail loud), 403, 404-not-provisioned, 404-unknown-agent, 401.
3. Assert the `X-API-Key` header is used for an `obx_key_` credential and `Bearer`
   otherwise — pin it, since getting it wrong is a silent 401.
4. `--rotate` branch in `auth.go`: agent-id resolution, confirm text, both calls,
   the guard, then the existing write path.
5. Integration-ish test with a mock backend: rotate ⇒ `.env` contains the new
   token and key and the **same** DID.
6. Manual verification against the real stack is phase 8's job.

## Todo list

- [x] `RotateAPIKey` + `RotateIdentity` on the existing `do()`
- [x] Envelope-tolerant decoding; fail loud on a missing field
- [x] All five error cases mapped with distinct messages
- [x] `X-API-Key` vs `Bearer` selection pinned by test
- [x] `--rotate` branch with explicit confirm; `--yes` bypass
- [x] DID unchanged across rotation, asserted

## Success Criteria

- `auth --rotate` against a mock backend writes a new token + private key and an
  unchanged DID.
- A 2xx body without `privateKey` produces a clear failure and **no** file write.
- 403 says the org key lacks `update:agent`.
- 404 *not provisioned* reads differently from 404 *unknown agent*.
- No implicit rotation: without `--rotate`, no rotate endpoint is called.

## Risk Assessment

| Risk | L×I | Observable signal it broke | Pre-decided response |
|---|---|---|---|
| Response wraps `privateKey` differently than assumed | M×H | field-missing guard fires in real use | **Adjust:** guard already prevents a bad write; capture the real body shape, fix the decoder, keep the guard. |
| A `ClassSerializerInterceptor` is added upstream and strips `privateKey` | L×H | every rotation fails the guard after a backend deploy | **Stop and escalate:** this is an upstream change, not ours. File the openbox-backend ticket **now**, before it bites. |
| Org key lacks `update:agent` | M×M | 403 on the first call | **Accepted:** clear message; the user grants it in the dashboard. Nothing to code around. |
| Partial rotation (key rotated, identity call fails) | L×M | `.env` not written; server has a new key the client lacks | **Accepted, mitigated:** no write happens until both succeed, so re-running `--rotate` recovers. The previous key is already invalid either way — say so in the error. |
| KMS-mode agent returns a non-exportable key | L×H | `privateKey` empty for a `kms` agent | **Adjust:** research says both providers return it (`aws-kms-provider.ts:97`); if a real KMS agent proves otherwise, `--rotate` must refuse for `attestation_mode: kms` with a message, not write a broken config. |

## Security Considerations

- Rotation is destructive server-side: it invalidates the previous key. The confirm
  text must say that plainly, and `--yes` must be the only way past it.
- The org control token is read from the environment (`OPENBOX_CONTROL_TOKEN`),
  never a flag (INV-1), and is not persisted by `auth`.
- Never log a response body from these endpoints — both contain credentials.
- Order key-then-identity so a failure cannot leave a signable-but-unauthenticated
  or unauthenticated-but-signable half state that writes to disk.

## Next steps

Phase 7 slims `init`; phase 8 verifies the whole flow across platforms and updates
user docs. `--rotate` is also the documented recovery path for an existing install
whose credentials are stranded in the OS keychain (D1) — phase 8's migration note
points here.
