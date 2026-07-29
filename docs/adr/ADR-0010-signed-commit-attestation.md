# ADR-0010 — Signed commit attestation

Status: **Accepted (implemented: shift-left producer + openbox-core verifier)**
Date: 2026-07-29
Story: E8-S10 (report finding SL-05)
Related: ADR-0008 (signed policy bundles — same carry-the-signed-bytes discipline),
E8-S8/S9 (managed tier, which bounds what this can prove).

## Context

Deploy lineage rests on the `OpenBox-Session` git trailer, and a trailer is just
text in a commit message:

```bash
git commit -m "fix: thing

OpenBox-Session: <a real session id>"
```

Nothing about that commit was produced by that session. Server-side ownership
verification (SL-15) established a genuinely useful second fact — the session id
belongs to the agent that pushed — but it still does not connect the session to
*this* commit. A developer could stamp one of their own real sessions onto a
commit written entirely by hand, and lineage would read `attributed`.

`deploy_session_links.verified` existed for the stronger statement, but nothing
ever set it, so in practice every link was a claim.

## Decision

The session's keyholder signs a statement about the commit, and the deploy path
verifies it.

### What gets signed

```jsonc
{ "v": 1,
  "repo": "github.com/acme/app",       // canonical remote, credentials stripped
  "commit_sha": "…", "tree_sha": "…", "parent_shas": ["…"],
  "session_ids": ["…"], "thread_id": "…",   // thread_id only when it differs (E8-S4)
  "bundle_policy_id": "…", "bundle_sha256": "…",
  "adapter": "openbox-cli", "did": "did:aip:…", "signed_at": "RFC3339" }
```

Three deliberate inclusions:

- **`tree_sha` + `parent_shas`** — the statement is about this exact content at
  this exact position in history, not about a sha string.
- **`bundle_policy_id` + `bundle_sha256`** — the policy in force when the code was
  written. This is what makes the attestation worth more than provenance: a deploy
  gate can ask *"was this written under current policy"*, not only *"who wrote
  it"*.
- **`v`, inside the signed bytes** — so a verifier cannot be tricked into reading
  a v1 payload as a later shape with different semantics.

Signed with the **existing dev-agent Ed25519 seed** — the same key AIP request
signing uses. No new key material, and the public half is already KMS-resident
under `alias/openbox-agent/<did-uuid>`, so the server can verify with machinery
that already exists.

The signed bytes travel **verbatim** as `canonical_b64`, for the same reason as
ADR-0008: a verifier that re-serializes its own input eventually disagrees with
the signer over key order or number formatting.

### Transport: a git note, not a trailer

`refs/notes/openbox-attest`, separate from the existing session mirror
(`refs/notes/openbox`) so the two can be fetched, pushed and pruned independently
and neither can be parsed as the other.

A note rather than a trailer because the sha is *part of what is signed*, so the
artifact can only exist after the commit does — and because a signature does not
belong in a message that a rebase will rewrite.

Notes are not pushed by default. A pipeline must opt in:

```yaml
- run: git fetch origin "+refs/notes/openbox-attest:refs/notes/openbox-attest"
```

A missing note is **normal**, not an error, and degrades to today's
inferred/attributed claim.

### `verified` requires BOTH facts

```
verified = ownership_verified  AND  attestation_verified
```

Neither is sufficient, and this is the crux of the design:

- **Ownership alone** says the session is yours, not that it produced this commit.
- **A signature alone** proves keyholding. An attacker with their own valid agent
  key could sign an attestation naming *someone else's* session id.

So core composes them. Attestation verification checks, in this order (cheap
checks first, so a replay costs no KMS call):

1. envelope and payload decode; `v` is supported;
2. the payload's `did` is present and agrees with the envelope's routing copy —
   the **signed** DID decides which key to check;
3. `commit_sha` equals the commit the claim was resolved from;
4. `session_ids` contains the session being verified;
5. the signature verifies against the DID's KMS alias.

**This tightens the column.** An ownership-only link that previously wrote
`verified=true` now writes `false` until an attestation is present. That is
correct — the column is read as "this session produced this commit", which
ownership never established — and the disruption is minimal because nothing reads
the column yet (the lineage reader, C4, is still deferred).

## What it proves, precisely

> The holder of this session's key asserted that this exact commit came from this
> session, under this policy bundle.

It does **not** prove the session's model produced the diff. Nothing local can: a
developer can hand-write code inside a governed session, and that is a legitimate
workflow, not an attack.

It defeats the report's "stamp an owned-but-unrelated session id" attack, **only
if the attacker lacks the seed**. A compromised endpoint can still self-attest —
which is exactly why the managed tier (E8-S8/S9) is part of the same story: an
attestation is only as trustworthy as the machine that produced it. The honest
ladder is:

| Level | Means | Established by |
|---|---|---|
| `inferred` | a trailer says so | anyone |
| `attributed` | the session belongs to the pusher | server ownership check |
| `verified` | the keyholder bound this commit to this session | this ADR |
| *(aspirational)* | …on a machine that could not have lied | + managed tier |

## Failure posture

Every leg is best-effort and fail-open:

- **Signing** happens in `post-commit`, after the commit exists, so there is
  nothing to fail. No credentials, no session, or an unresolvable sha ⇒ no note.
- `Attest` **refuses** on incomplete input rather than producing something
  unverifiable — a broken attestation would force the deploy path to decide what
  it means.
- **The git action** carries the note verbatim and does not verify it: the public
  key is in KMS, unreachable from a pipeline, so a local check could only be
  decorative. It does bind the attestation to the commit and session it is
  presented for, so one note cannot be offered as evidence for an unrelated claim.
- **Core** logs a declined attestation only when one was actually offered ("no
  attestation" is the common case and would drown the real failures), and
  distinguishes *could not verify* (verifier unhealthy) from *did not verify*
  (content suspect) — those need different operator responses.
- A deploy is **never** failed over lineage. A governance feature that blocks
  releases is a governance feature that gets switched off.

## INV-1

The signed bytes are shipped to the server, so a credential must never reach them.
`canonicalRemote` strips userinfo from remote URLs (`https://x-token:ghp_x@host/…`
→ `host/…`) and there is a test asserting it.

## Alternatives rejected

- **Sign the commit itself (gitsign / GPG).** Signs *authorship*, which is the
  developer's identity, not the session's. It also collides with orgs that already
  mandate their own commit signing. Attesting alongside leaves both intact, and
  aligns later with in-toto/SLSA where the attestation is a separate artifact by
  design.
- **Put the signature in a trailer.** The sha is part of what is signed, so it
  cannot exist pre-commit; and a rebase rewriting a signed message would silently
  invalidate it.
- **Verify in the git action.** The key lives in KMS. A pipeline-side check would
  have to trust a key the pipeline was handed, which verifies nothing.
- **Make `verified` mean attestation alone.** Drops the ownership fact, letting
  any keyholder claim any session id.
- **Fail the deploy on a bad attestation.** Turns lineage into a release gate. If
  an org wants that, it belongs in an explicit deploy policy reading `verified`,
  not in the lineage writer.
