# ADR-0008 — Signed policy bundles

Status: **Accepted (client side implemented; backend signing not yet built)**
Date: 2026-07-29
Story: E8-S6 (report finding SL-02)
Supersedes nothing. Related: ADR-0005 (bundle sync + staleness pin), ADR-0006 (in-process decider).

## Context

The enforcement policy a session evaluates lives in a JSON file in the developer's
own config directory (`~/.config/openbox/policy-bundle.json`). Any process running
as that user can:

1. edit a rule so a dangerous tool call is allowed;
2. restore an older, more permissive bundle;
3. keep a stale bundle indefinitely and never re-sync.

None of that was detectable. `dev sync` fetched the policy over an authenticated
channel, but authenticating the *channel* says nothing about the *file* that ends
up on disk, and the file is what the decider reads. The pin added in ADR-0005
(`policy_id` + `updated_at`) detects that the backend has *moved on*, not that the
local content is what the backend sent — a hand-edited bundle keeps its pin.

Meanwhile `version_hash` from the backend is a content hash, not a signature:
anyone who can edit the bundle can recompute it.

## Decision

Policy content is trusted only when a **signature over it verifies against a
pinned org public key**, the **epoch has not gone backwards**, and it **has not
expired**. Three parts, in order of importance:

### 1. The evaluated policy is re-derived from the signed bytes

This is the load-bearing decision. Verifying a signature over a payload and then
evaluating the surrounding file's fields would prove nothing — an attacker would
edit the fields the evaluator actually reads and leave the signed payload alone.

So `Bundle.VerifyIntegrity` returns a **new bundle built from the signed payload**,
and callers evaluate only that. The bundle file's own `policy_builder` / `rules`
fields become a debugging view whose contents cannot affect a decision.

### 2. Ed25519 over bytes carried verbatim

The signed bytes travel as `canonical_b64` rather than being re-serialized by the
verifier from parsed fields. A verifier that reconstructs its own input is a
verifier that eventually disagrees with the signer over key order, number
formatting or unicode escaping. Carrying the bytes removes canonicalization from
the trust path entirely.

Ed25519 because core already verifies Ed25519 for AIP request signing
(`internal/services/agent.go` + the KMS verifier), and openbox-backend already has
the Ed25519 generate → wrap → KMS-import path (`src/modules/did/aws-kms-provider.ts`).
Algorithm is pinned in code, not read from the field, so a bundle cannot ask to be
verified with something weaker.

### 3. Rollback protection by monotonic epoch, not by content hash

A signed-but-superseded bundle is a valid signature, so only an ordering fact can
refuse it. `policy_epoch` is a monotonic integer; the client remembers the highest
it has accepted in `<bundle>.epoch` and refuses anything lower.

Note this reconciles with a real product feature: openbox-backend supports
**rolling back** control sets. "No rollback" therefore means *a rollback mints a
new, higher epoch* — never "rollbacks are forbidden".

## Wire contract (pinned, because signer and verifier are in different repos)

Response shape from `GET /agent/:agentId/policies/current`:

```jsonc
{ "status": 200,
  "data": {
    "id": "...", "rego_code": "...", "config": {...}, "updated_at": "...",
    "signed": {
      "key_id": "org-key-1",
      "algorithm": "Ed25519",
      "canonical_b64": "<base64 of the exact signed bytes>",
      "sig_b64": "<base64 raw 64-byte Ed25519 signature>"
    } } }
```

`canonical_b64` decodes to this payload — **the authoritative policy**:

```jsonc
{ "policy_id": "...", "agent_id": "...", "updated_at": "...",
  "policy_epoch": 7, "expires_at": "2026-08-05T00:00:00Z",
  "version_hash": "...", "policy_builder": {...},
  "raw_rego_unlocalized": false }
```

Rules:

- The signature is over the **decoded bytes of `canonical_b64`**, nothing else.
- `expires_at` is RFC3339. Recommended TTL 7 days: long enough for offline work,
  short enough to force re-sync. Empty means no expiry (discouraged).
- `policy_epoch` must be monotonic per agent and must increase on every policy
  change, **including a rollback**.
- `policy_builder` must parse as `decision.PolicyBuilderConfig`. It cannot express
  a deny-by-default (the builder is first-match, no-match-means-allow), so the
  fail-open default survives signing by construction.
- Omitting `signed` is legal and means unsigned — see compatibility.

Client pins the key as non-secret config in `dev.json`:
`org_signing_key_id`, `org_signing_pubkey` (base64 raw 32-byte Ed25519), with
`OPENBOX_ORG_SIGNING_PUBKEY` / `OPENBOX_ORG_SIGNING_KEY_ID` overrides.

## Outcomes

`decision.Integrity`, recorded per session in posture (E8-S5) as
`posture.bundle_integrity`:

| Outcome | Meaning | Policy loaded? |
|---|---|---|
| `unsigned` | No signature block. Compatibility path. | yes, the file's own |
| `verified` | Signature, epoch and expiry all good; policy re-derived from signed bytes. | yes, re-derived |
| `no_key` | Signed, but no key pinned — an *incomplete deployment*, not suspect content. | yes, the file's own |
| `bad_signature` | Content altered, or signed by an untrusted key. | no |
| `expired` | Valid signature, past `expires_at`. | no |
| `epoch_rollback` | Valid signature, epoch below the pinned floor. | no |
| `malformed` | Undecodable block or payload. Untrusted, never treated as absent. | no |

Only `verified` is `Trusted()`. `unsigned` and `no_key` are explicitly **not**
assurance — they are *unverifiable*, which is a different thing from *untrusted*.

## Behaviour

- **`dev sync`** verifies *before* replacing the last-good bundle. A signature
  that fails verification **aborts the sync** and leaves the previous bundle in
  place — installing it and distrusting it later would trade a policy that did
  verify for one that did not. Unsigned, and signed-with-no-pinned-key, install
  with an explanatory note.
- **The enforce gate** loads policy for the three outcomes above where
  verification did not *fail*; a bundle that failed verification is **not loaded
  at all**, leaving the decider at cold-start fail-open.
- **The epoch pin** advances only on `verified`. `Bundle.Epoch()` reads the payload
  without checking the signature, so pinning from any other outcome would let
  whoever answered the fetch set the floor — a claimed `MaxInt64` makes every later
  genuine bundle read as `epoch_rollback`, and the floor never lowers.
- **Posture** records the outcome in every case.

`unsigned` and `no_key` must stay symmetric here, and the reason is operational:
they are the same epistemic state (this client cannot check the content) and differ
only in whose deployment is incomplete. Loading one and refusing the other meant
that the day a backend began signing, every install without `org_signing_pubkey`
pinned — all of them, the key being new — silently stopped enforcing while
`dev sync` reported success; under the opt-in fail-closed policy it denied every
tool call instead. Neither is a security trade, per the honest limit below.

### Honest limit

Refusing to load a tampered bundle is **detection, not prevention**. If the tamper
made policy *more permissive*, fail-open lands where the attacker wanted anyway.
What this buys today is that the outcome is *recorded* rather than silent.

Turning an unverifiable bundle into a **deny for high-risk tool classes**
(Bash/shell, MCP) is the posture change **OD-E8-3** gates, and it is deliberately
not implemented here: it is a behaviour change developers will feel, so it needs
an explicit decision on the risk-class split. The incident record (MCP
"agentjacking", poisoned MCP tool descriptions, credential access dominating
blocked coding-agent activity) supports Bash+MCP as that class.

The epoch pin is a supporting control: deleting `<bundle>.epoch` re-enables a
rollback. It reads as 0 when missing because refusing to load policy over a
corrupt bookkeeping file would turn a local disk problem into a governance
outage. The signature is the primary control.

## Compatibility

Unsigned bundles load exactly as before. A backend that does not sign is therefore
not a regression — it is simply not an improvement. This is what makes the client
side shippable ahead of the backend, which is how it was sequenced.

## Backend work still required

Not implemented (openbox-backend); the contract above is the specification.

1. **Org signing key**: `org_signing_keys` (`org_id`, `key_id`, `key_arn`,
   `public_key`, `algorithm='Ed25519'`, `created_at`, `active`), provisioned via
   the existing Ed25519 generate → RFC-5649 wrap → KMS import path under alias
   `alias/openbox-org/<org-uuid>`.
2. **`SignCommand` plumbing** on `KmsService` (`Ed25519Sha512` / `MessageType.RAW`).
   The STS-cached client factory and error handling already exist; the sign call
   itself does not (there is no `SignCommand` anywhere in `src/` today).
3. **`policy_epoch`**: a monotonic column on `policies` (or a small
   `policy_bundle_versions` table) stamped inside the existing
   `createPolicy`/`updatePolicy` transactions. `version_hash` cannot serve — it is
   a content hash and cannot be ordered.
4. **`expires_at`**: signed at response time as `signed_at + TTL`.
5. **Public-key route**: `GET /organization/:organizationId/signing-keys`, guarded
   like the other org-scoped reads, so `dev init` can pin the key automatically.
   Until it exists, the key is pinned by hand in `dev.json`.
6. **Parse compatibility**: the translate path must accept policy-builder **v1 and
   v2** (`origin/develop` ships `PolicyBuilderConfigV1/V2`).

## Alternatives rejected

- **Sign the whole bundle file.** The file is written by the client (it adds the
  pin and the translated shape), so the signer cannot produce its bytes. Signing a
  payload the client embeds is the only version that survives the client owning
  the file format.
- **Trust `version_hash`.** A content hash detects accidental corruption, not a
  motivated editor who recomputes it.
- **Fail closed on any unverifiable bundle.** Correct destination, wrong order: it
  changes developer-visible behaviour and needs OD-E8-3 first.
- **TLS/channel authentication only.** Already in place, and orthogonal — it says
  nothing about the file on disk, which is what gets evaluated.
