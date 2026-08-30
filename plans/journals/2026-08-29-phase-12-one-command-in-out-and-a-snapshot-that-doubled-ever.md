---
title: "Phase 12: one command in/out, and a snapshot that doubled every token count"
date: 2026-08-29
summary: "Derived producer election over a stored one; review caught the daemon freezing it at startup, which 19 mutation drills could not reach."
---

# Phase 12: one command in/out, and a snapshot that doubled every token count

## What happened

Phase 12 of `260827-2301-go127-oss-three-lanes`: `openbox init --full` installs
hooks + telemetry + transport in proof order; `--remove-all` backs it all out
before the credential gate. Three shared packages instead of three copies —
`activation` (settings-env record), `laneservice` (all three supervisor units
from one Spec), `atomicfile`.

## Decisions worth keeping

**Derived election over a stored one.** The phase specified a
`model_call_producer` field in `dev.json`. I wrote it, tested it green, then
reverted it. A lane observes a call only if the client is routed to it, so "who
is routed" and "who may emit" are one question — and a stored copy drifts in the
worst direction: remove a lane without rewriting the field and the machine
reports no model calls at all while looking perfectly configured.

**Precedence needed one correction a pure ranking gets wrong.** That decision
ranks transport above gateway. That answers "what should an org install". The
election answers "what will actually see this call", and a base URL takes the
relay out of the path — loopback is not proxied, anything else is
blind-tunnelled. With both configured, the gateway is the emitter. The count was
never at risk; the attribution was. Hence `Routed` and `Candidates` as separate
fields.

**Cut the gateway env port; kept the unit port.** The env port is a
zero-behavior-change refactor of the only socket-verified lane here and serves
neither half of the outcome. The unit layer genuinely needed sharing: a plist
that forgets StandardErrorPath logs nowhere, and an ExitTimeOut that stops
matching --shutdown-grace is SIGKILLed mid-drain every restart. Neither surfaces
as an error.

**`--remove-all` does not delete the spool**, against the phase's requirement
text, because that text contradicts its own security section. The spool lives
outside `~/.openbox` and is shared with the hook path, which this command does
not remove.

## The defect review caught, and why it is the interesting part

The telemetry daemon resolved the election ONCE at startup and froze it in a
`Policy{Elected bool}`. `--full` installs telemetry FIRST and transport second,
so the daemon booted correctly elected, kept that answer, and went on emitting
after transport took the election from it. Both lanes then described the same
model call — and because the activity_id namespaces are deliberately disjoint,
core stores both rather than rejecting one. Every token count doubles, silently,
while `doctor` reports one clean elected lane.

**It survived 19 mutation drills.** A drill can only be red on a line some test
executes, and every test agreed with this one: they covered the pure election
function, or ONE lane's install and removal in isolation. None built a second
daemon's already-running snapshot and changed the routing under it. Drills prove
a test is load-bearing; they say nothing about a case no test constructs.

The rejected fix is the instructive half. Bouncing the daemon whenever an install
flips the election was the obvious repair — and it is the same shape as the
stored election I had just removed: a second copy with a sync obligation,
covering only the paths `init` controls, leaving a hand-edited settings file or an
MDM deployment stale. `Policy.Elected` is a `func() bool` now, nil suppressing, so
the zero value's guarantee is structural rather than conventional.

## Two smaller lessons from my own drills

One drill was GREEN until I strengthened its test: on darwin the install path
only reaches `launchctl bootstrap <path>`, and the LABEL is used by `bootout`
alone, which nothing but a removal calls. The test stopped after the install, and
its assertion passed off the plist path — which contains the label as a
substring. A test that passes for a reason other than the one it names is
indistinguishable from one that works.

Another reported RED because my mutation did not compile. A build failure is not
a red test. Every drill now vets before its verdict is believed.

## Next steps

Phase 13 proves the bytes — and should exercise the SEQUENTIAL-INSTALL case, not
only steady state. The highest-consequence unverified claim remains that the 13
telemetry env keys are the ones the client actually reads: every test asserts
JSON we wrote, and the client silently drops names it does not recognize.

> Historical work record — not durable authority. Prefer docs/specs for current decisions.
