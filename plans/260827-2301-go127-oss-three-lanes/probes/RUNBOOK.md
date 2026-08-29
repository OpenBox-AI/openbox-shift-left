# Probe A — which refusal shape does Claude Code surface instead of retrying?

**Status:** ready to run. Needs a bind-capable host, a real Claude Code install, a
throwaway project, and provider credentials. Nothing else in this plan blocks on it.

## Why this exists

ADR-0021 is **DRAFT**, and as of 2026-08-28 exactly one thing keeps it there: §9.
The gateway can refuse a model call, but *what a refusal should look like on the
wire* is an empirical question about a client we do not control.

The failure it guards against is specific and quiet. **A refusal the client retries
around is worse than no refusal at all**: the developer sees a slow session rather
than a policy decision, and the call succeeds on the retry — so the control reports
"enforced" while the model call happened. The opposite error is just as bad in the
other direction: a shape the client cannot parse can silently disable a capability
for the rest of the session.

So `gateway/refuse.go` ships with `refusalStatus` and `refusalErrorType` marked
PROVISIONAL, `gate.Decide`/`WriteRefusal` have **no production caller**, and a
dormancy test holds that arrangement in place. This probe is what replaces the
guess with a measurement.

## The instrument

`probes/refusal-injector/` — its own module, no product dependency, not in any
release artifact.

```bash
go run ./probes/refusal-injector -list
```

It is a loopback reverse proxy. It forwards every request to the provider except
the Nth model call, which it answers with a candidate shape.

**It fabricates provider responses.** Throwaway project, throwaway session, every
time. A refusal injected mid-conversation can leave the client's context in a state
it did not expect — finding out what that state is *is the exercise*.

It uses `httputil.ReverseProxy`, which injects `X-Forwarded-For` and manages
`Accept-Encoding`. That is fine for measuring a client's reaction and means
**nothing observed here is evidence about the product's own relay**, whose
byte-identity is proven separately (`cli/cmd/openbox/transportreplay_test.go`).

## Setup

1. A scratch directory, not a real repository:
   ```bash
   mkdir -p /tmp/probe-a && cd /tmp/probe-a && git init
   ```
2. Enable the provider's own per-attempt telemetry, which is how retries are
   counted — see "Counting retries" below.
3. Start the injector with one candidate, refusing the 3rd model call so the
   refusal lands mid-conversation rather than at startup:
   ```bash
   go run ./probes/refusal-injector -shape invalid_request_error -after 2
   ```
4. In the scratch directory, point the tool at it and run a session that will make
   several model calls:
   ```bash
   ANTHROPIC_BASE_URL=http://127.0.0.1:8791 claude
   ```
   Ask something that takes four or five turns. The injector prints each request it
   matched and each response it fabricated.

## Counting retries

**Do not count from the injector's own log alone.** Its counter says how many
requests arrived, not how many the client considered part of the same logical
call — and that distinction is the entire measurement.

Read both:

- The injector's stderr: how many `/v1/messages` requests arrived after the
  injected one, and how quickly. Three requests in under two seconds is a retry
  loop; one request a minute later is the developer trying again.
- The client's own telemetry: `x-stainless-retry-count` on subsequent requests is
  the provider SDK's own attempt counter, and it is the authoritative signal. A
  non-zero value on the request following the refusal means the client retried;
  `0` means it did not.

  **Measured on the corpus, and the caveat matters.** All **5,231** model-call
  requests in `openbox-logger` run `20260827T063932Z-225cac` carry the header, so
  it is reliably present — but every one of them reads `0`. The corpus contains
  **no observed retry at all**, which means it cannot tell you what a retry looks
  like. That is precisely why the negative control below is not optional: it is
  the only thing in the run that demonstrates a non-zero value is reachable on
  this client at all. A run without it that reports "no retries anywhere" is
  indistinguishable from a run that cannot see retries.

Record, for each shape: attempts observed, `x-stainless-retry-count` on the next
request, whether the session displayed anything to the developer, the exact text
shown, and whether the session continued usefully afterwards.

## The candidates

Run every one, including the negative control. `-list` prints the rationale for
each and its predicted behaviour; the predictions are hypotheses, and a run that
matches them is as informative as one that does not.

`overloaded_error` is the **negative control** and is not a candidate. It should be
retried. If a run cannot distinguish it from the others, the instrument is not
measuring retries and nothing else in the run means anything — fix that before
recording any result.

## Qualifying

A shape qualifies when **all** of these hold:

1. no retry — `x-stainless-retry-count` is absent or `0` on the next request, and
   no burst of attempts follows the refusal;
2. the developer sees something — the refusal reaches the session UI as text, not
   as a silent stall or a generic network error;
3. no credential side effect — the client does not re-prompt for or discard
   credentials (this is what disqualifies `authentication_error` if it otherwise
   looks good);
4. the session survives — subsequent turns still work.

## Pre-decided outcomes

Both directions are decided in advance, so the run cannot be argued with after the
fact.

**A shape qualifies.** Fill in ADR-0021 §9's two constants — `refusalStatus` and
`refusalErrorType` in `gateway/refuse.go` — with the measured pair, record the run
in the ADR, and move ADR-0021 from DRAFT to ACCEPTED. Wiring a production caller
for `gate.Decide`/`WriteRefusal` is then its own change with its own review; this
probe answers the shape question, not the enablement question. `testbed/45-gateway.sh`
case D becomes live.

**No shape qualifies.** Refusal **descopes to observe-only**. Record the negative
result in ADR-0021 §9 — it is a finding about the client, not a gap in this plan —
make the dormancy test permanent rather than temporary, delete case D from
`testbed/45-gateway.sh` rather than leaving it to rot, and state in `COVERAGE.md`
that the in-path lanes observe and do not refuse. Do not iterate on new shapes
without a reason to expect a different answer: the candidate table already spans
the provider's own error taxonomy.

**A shape qualifies but only sometimes.** Treat as "no shape qualifies". An
intermittently-surfaced refusal is the worst of the three outcomes, because it
reports enforcement that did not happen on exactly the calls nobody looked at.

## Results

| shape | attempts after | retry-count header | shown to developer | session survived | qualifies |
|---|---|---|---|---|---|
| invalid_request_error | | | | | |
| permission_error | | | | | |
| authentication_error | | | | | |
| sse_error_event | | | | | |
| overloaded_error (control) | | | | | expected NO |

## Afterwards

Stop the injector and unset `ANTHROPIC_BASE_URL` in the scratch shell. Delete the
scratch directory: the session transcripts in it contain fabricated provider
responses, and a transcript that says the provider refused something it did not is
exactly the kind of false record this product exists to prevent.
