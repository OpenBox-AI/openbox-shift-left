# Probe: `ANTHROPIC_BASE_URL` auth coverage (P0) and org-id matchability (P1)

Status: **TEMPLATE — NOT RUN.** Fill from a real run per
[the runbook](../260825-0027-openbox-gateway-full-capture/probes/RUNBOOK.md).
Delete this line when it holds real observations.

Date: — · Claude Code version: — (`claude --version`)
Gates: plan `260825-0027` phase 03 steps 1 and 3; blocks phase 04.

Rule this file obeys: **behaviours, not tokens.** Credentials appear only as
`(kind, length, sha256[:8])`, which is all the probe server can print.

## P0 — does `ANTHROPIC_BASE_URL` redirect this auth mode?

| Auth mode | Request arrived? | `credKind` reported | Header verbatim? |
|---|---|---|---|
| API key (`ANTHROPIC_API_KEY`) | — | — | — |
| Subscription OAuth | — | — | — |

**Answer:** —

**Consequence.** If OAuth does not redirect, pass-through covers API-key/console
orgs only, and ADR-0021 plus the product docs must say so in those words. Track B
still proceeds — this scopes the gateway tier, it does not cancel it (plan risk
table).

### Headers observed (non-sensitive values verbatim)

```
—
```

### Request body

Top-level keys only (the body is the developer's prompt and file contents):

```
—
```

## P1 — is an org identifier matchable?

### 1. From the credential

JWT? — . Claim keys and value shapes (KEY NAMES ONLY where a value is uuid- or
email-shaped):

```
—
```

**Matchable org id in the credential:** —

### 2. From provider response headers

**Unresolved by construction.** The probe server IS the provider here, so it has
no upstream response to inspect. Carried into phase 04, where the gateway has a
real upstream. Not guessed.

### 3. Local account state

Key paths only, no values:

```
—
```

**Org UUID readable:** — · **Email readable:** —

**Consequence.** With no matchable org id, the OAuth account rule ships
detection-only; API-key fingerprints can still refuse (plan risk table). Phase 05's
evidence contract is org UUID + email and nothing more — `organizationName` and
`organizationRole` are explicitly excluded.

## Unresolved questions

- (list anything the run could not settle, rather than resolving it by inference)
