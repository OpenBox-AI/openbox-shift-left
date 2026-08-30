---
title: "Gateway passthrough core: the stdlib fights byte-identity"
date: 2026-08-25
summary: "Phase 04 shipped as a hand-rolled relay after a fail-first drill proved httputil's defaults mutate the forward; 6 review angles found 15 real defects incl. a trusted 'localhost' and a plan doc that would have broken phase 07 on every boot."
---

# Gateway passthrough core: the stdlib fights byte-identity

## What happened

Implemented phase 04 of plan `260825-0027` — the local OpenBox gateway's passthrough
core. New `gateway/` module (12th in `go.work`), plus `openbox gateway`.

The TDD order mattered more than usual. The identity test was written first and run
against a stock `httputil.NewSingleHostReverseProxy`, where it **failed** on an added
`X-Forwarded-For` and an injected `Accept-Encoding: gzip`. That red run is the whole
justification for hand-rolling the relay: both are silent mutations of the exact bytes
this gateway exists to preserve, and the gzip one additionally triggers transparent
decompression, which corrupts a relayed SSE stream.

Two of my own diagnoses were wrong in the same direction — I blamed the gateway for
things my *test client* did. Go's default client injects `Accept-Encoding: gzip` when a
request carries none, and follows redirects. Both showed up as gateway failures until I
drove the handler with no client in the path and saw it clean. Lesson: when the harness
sits between you and the thing under test, the harness is a suspect.

## Review

Six angles ran. They found 15 defects I had not, and the good ones were empirical rather
than stylistic — one extracted my guard test's AST logic into a standalone harness and
ran evasion fixtures against it.

Highest-value findings:

- `requireLoopback` **trusted the string `"localhost"`** without resolving it. A hosts
  file, DNS, nsswitch or an interception agent can point it anywhere, and the listener
  would follow. Now resolved, every answer must be loopback, and `gateway.Listen`
  re-checks the address the *kernel* returned — the check no resolver can talk past.
- The guard test could be **evaded by an aliased import**: it matched the package name at
  the call site, so `import os2 "os"; os2.Getenv(...)` was invisible. Now resolved through
  import bindings, covers `syscall`/`io/ioutil`, refuses dot-imports.
- The guard's **mutation control was a hand-maintained twin** of the scanner — it proved a
  parallel implementation worked, not the real one. Both now call one `scanSource`. Same
  lesson the repo already learned with `doctor`/`init`'s shared classifier.
- Phase 07's plan said the service unit runs `openbox gateway --config <path>`. **No such
  flag exists.** A unit generated from that wording would have failed to start on every
  boot — found only by tracing a plan doc against the shipped flag surface.
- `MaxIdleConns: 100` was **silently capped at 2** because `MaxIdleConnsPerHost` was unset
  and all traffic goes to one host.
- A graceful stop that outran its 10s grace exited **1**, which a supervisor with
  restart-on-failure reads as a crash.

Two of my comments were simply false and got corrected. `relayBufferSize` did not protect
SSE pings — the flush-per-read does, at any buffer size. And the package doc overclaimed
against `httputil.ReverseProxy`: the modern `Rewrite` hook strips `X-Forwarded-*` and
auto-flushes SSE, so the honest reason to hand-roll is phase 05's two-way tee, not stdlib
defaults. Writing a plausible rationale is easy; writing a true one needs checking.

## Process failure worth recording

An early smoke run left a gateway holding port 8788. A later run failed to bind, fell
through, and I measured the **stale binary** — reporting a body-cap "leak" that did not
exist. Caught it only because the banner said "address already in use". Kill what you
started before re-measuring, and read the startup output before trusting the numbers
under it.

## Decision

Kept the hand-rolled relay rather than switching to a `Rewrite`-based `ReverseProxy`,
even though the review showed the latter satisfies both invariants. Reason: phase 05
needs a re-readable tee of *both* directions, and the plan's own risk table already
prescribes hand-rolling as the mitigation for ping-stripping. Recorded the corrected
rationale rather than the flattering one.

Did NOT run the credential-bearing probes (P0, probe A, P1 §1). The runbook gates them to
a human; an advisory agent argued for overriding that and the recommendation drew a
security warning. Ran only P1 §3 — local account state, no credential, no network, no
quota — which found `oauthAccount.organizationUuid` and `emailAddress` readable as
strings, unblocking phase 05's account evidence.

## Next steps

- USER: P0 (both auth modes), probe A (needs an interactive session for the
capability-disable signal), P1 §1; accept; file the backend ask.
- Phase 05 is startable except requirement 5 — identity from `x-claude-code-session-id`.
  Phase 04 proved that header *relays* verbatim, which is silent on whether Claude Code
  *sends* it. That needs P0 positive.
- Phase 06 is hard-blocked: the refusal shape IS the phase.

> Historical work record — not durable authority. Prefer docs/specs for current decisions.
