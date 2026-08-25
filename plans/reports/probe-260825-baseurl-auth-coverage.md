# Probe: `ANTHROPIC_BASE_URL` auth coverage (P0) and org-id matchability (P1)

Status: **PARTIAL — P1 §3 run, P0 and P1 §1 NOT RUN.** The one leg that needs no
credential, no network and no quota is filled from a real inspection; every leg
that redirects live model traffic is still a user action per
[the runbook](../260825-0027-openbox-gateway-full-capture/probes/RUNBOOK.md).
Do not read an empty cell as a negative result — silence is only a real answer
after a positive control, and no control was run.

Date: 2026-08-25 (§3 only) · Claude Code version: 2.1.229
Gates: plan `260825-0027` phase 03 steps 1 and 3.

Rule this file obeys: **behaviours, not tokens.** Credentials appear only as
`(kind, length, sha256[:8])`, which is all the probe server can print. §3 below
records key names and JSON types only — no values, and specifically not the
machine owner's email address or org UUID.

## P0 — does `ANTHROPIC_BASE_URL` redirect this auth mode?

**NOT RUN.** Requires pointing real model traffic at a throwaway listener in each
auth mode.

| Auth mode | Request arrived? | `credKind` reported | Header verbatim? |
|---|---|---|---|
| API key (`ANTHROPIC_API_KEY`) | not run | — | — |
| Subscription OAuth | not run | — | — |

**Answer:** unresolved.

One environment fact worth recording because it is easy to mistake for evidence:
on this machine `ANTHROPIC_BASE_URL` is already set, to the default
`https://api.anthropic.com`, and `ANTHROPIC_API_KEY` is absent (so the machine is
in subscription-OAuth mode). **That the variable is *set* proves nothing about
whether OAuth traffic follows a *changed* value** — which is the entire question.
It does mean the API-key half of P0 cannot run here without a key being supplied.

**Consequence.** If OAuth does not redirect, pass-through covers API-key/console
orgs only, and ADR-0021 plus the product docs must say so in those words. Track B
still proceeds — this scopes the gateway tier, it does not cancel it (plan risk
table).

### Headers observed (non-sensitive values verbatim)

```
not run
```

### Request body

Top-level keys only (the body is the developer's prompt and file contents):

```
not run
```

## P1 — is an org identifier matchable?

### 1. From the credential

**NOT RUN** — reading the bearer requires the OAuth session to actually reach the
probe server, which is P0 1b.

JWT? unresolved. Claim keys and value shapes:

```
not run
```

**Matchable org id in the credential:** unresolved.

### 2. From provider response headers

**Unresolved by construction.** The probe server IS the provider here, so it has
no upstream response to inspect. Carried into phase 04, where the gateway has a
real upstream. Not guessed.

### 3. Local account state

**RUN 2026-08-25.** Pure local file inspection: no credential presented, no
network, no model call, no probe server. This is the one leg of P1 whose blast
radius is a file read, which is why it is filled here while the rest is not.

Source: `~/.claude.json`, top-level key `oauthAccount`. Key names and JSON types
only:

```
oauthAccount.accountUuid                    string
oauthAccount.emailAddress                   string
oauthAccount.organizationUuid               string
oauthAccount.organizationName               string
oauthAccount.organizationType               string
oauthAccount.organizationRole               string
oauthAccount.organizationRateLimitTier      string
oauthAccount.userRateLimitTier              string
oauthAccount.seatTier                       string
oauthAccount.billingType                    string
oauthAccount.displayName                    string
oauthAccount.fullName                       string
oauthAccount.accountCreatedAt               string
oauthAccount.subscriptionCreatedAt          string
oauthAccount.profileFetchedAt               number
oauthAccount.hasExtraUsageEnabled           boolean
oauthAccount.workspaceRole                  null (present, unset on this machine)
oauthAccount.claudeCodeTrialEndsAt          null
oauthAccount.claudeCodeTrialDurationDays    null
oauthAccount.ccOnboardingFlags              object
```

Two sibling top-level keys also exist and are named here only so a later reader
does not mistake them for the account record: `orgModelDefaultCache` and
`penguinModeOrgEnabled`. A third, `clientDataCacheSlots.<slot>.org`, holds an
`org` field per cache slot.

**Org UUID readable:** yes — `oauthAccount.organizationUuid`, a string at a
stable, documented-by-observation path.
**Email readable:** yes — `oauthAccount.emailAddress`, likewise.

**What this settles for phase 05.** The validated evidence scope (org UUID +
email, from the 2026-08-25 plan validation) is *readable locally without touching
the credential at all*. That matters for the design: local account metadata does
not depend on P0 or on the credential being parseable, so phase 05's
account-evidence attachment is **not blocked** by the two unrun legs. It also
confirms the plan's exclusions are real choices rather than absent fields —
`organizationName` and `organizationRole` both exist here and are deliberately
NOT egressed.

**What this does NOT settle.** Reading an org UUID locally is not the same as
matching one to the credential actually in use. A developer could hold a
subscription session for org A while the local file still described org B, and
this leg cannot detect that. Binding the two together is P1 §1's job, and it
remains open.

## Unresolved questions

- **P0, both modes:** does `ANTHROPIC_BASE_URL` redirect, and does the credential
  arrive verbatim? Unrun. Needs a user with a throwaway listener; the API-key half
  additionally needs a key, which this machine does not have.
- **P1 §1:** is an org id matchable from the OAuth bearer? Unrun, and gated behind
  P0 1b arriving at all.
- **P1 §2:** provider response headers — unresolvable from a throwaway server by
  construction; carried into phase 04.
- **Is the local `oauthAccount` record trustworthy as evidence?** It is written by
  the client this product governs and is readable and writable by anything running
  as the developer (same posture ADR-0015 already concedes for the signing key).
  So it is *evidence of origin-of-config*, not a tamper-resistant account claim —
  the same honest limit the rest of this product states. ADR-0021's account rule
  should say so rather than imply the local read is authoritative.
