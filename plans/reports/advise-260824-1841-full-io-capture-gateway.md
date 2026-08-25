# Advise — full I/O capture: OpenBox gateway vs MITM vs OTel

Date: 2026-08-24 (rev. 2026-08-25). Advisory only, nothing implemented.
Question: is a MITM proxy the best way to capture all inputs/outputs incl. HTTP headers
and body, and send to OpenBox core for evaluation? Is there a more minimum solution?

## Confirmed reframing

**Problem.** Dev-runtime governance captures a fraction of what a session produces. Tool
output, tool input on observe path, thinking, and all HTTP-level detail never egress.
Owner wants full capture + evaluation. Three requirements selected by owner as real
(not completeness instinct): **real-time blocking of model calls**, **thinking blocks**,
**tamper-resistance (developer cannot disable capture)**.

**Requirements.**
1. Capture full model request + response (headers, body) per call.
2. Capture tool + MCP input and output.
3. Capture thinking blocks.
4. Evaluate synchronously; able to refuse a model call.
5. Capture not defeatable by the governed developer.

**Goals.** All classes reach core through the existing pipeline (`Spool.Append` → client →
`/evaluate`). No parallel pipeline.

**Non-goals.** SSO / IdP-group RBAC. MITM CA. Bedrock/Vertex/Foundry gateway formats
(v1 is Anthropic Messages only). CI pipelines.

**Constraint accepted by owner.** OpenBox becomes critical path for all model calls.

## Verdict

**MITM is the wrong build. OTel alone cannot meet the requirements. Build the OpenBox
gateway as THE gateway** — the substitution pattern Anthropic documents
(`gateways`: "A gateway is a proxy your organization runs between Claude Code and a model
provider"), not chained behind Claude apps gateway.

MITM fails req 5 outright: `HTTPS_PROXY`/`NODE_EXTRA_CA_CERTS` live in the same
`settings.json` `env` block as `OTEL_LOG_*`. Same removal cost. It buys a CA-forging
capability and zero assurance. Only **credential custody** is tamper-resistant — and once
a credential-holding gateway exists, MITM is also unnecessary: the gateway is the
legitimate TLS endpoint. No CA, no ALPN/h2 interception, no cert-store bugs.

## Architecture

```
Claude Code ──▶ OpenBox Gateway ──▶ Anthropic API
                auth: obx_ → DID
                evaluate → forward | refuse
                capture: headers, body, thinking
                passthrough: byte-identical, open lists
```

Distributed by managed settings: `ANTHROPIC_BASE_URL`, `ANTHROPIC_AUTH_TOKEN`,
`forceLoginMethod`.

### Most of the "new" scope already exists

| Gateway needs | OpenBox status |
|---|---|
| Per-developer credential | **exists** — `openbox auth` issues `obx_` runtime key + DID |
| Credential → identity | **exists** — DID is the identity through the whole pipeline |
| Wire carrier for hdrs/bodies | **exists** — core `SpanData` has `request_headers`, `response_headers`, `request_body`, `response_body`, `http_method`, `http_url`, `http_status_code` (`openbox-core/internal/content/governance.go:276-304`) |
| Event ingress | **exists** — `Spool.Append` → client → `/evaluate`, same auth/signing |
| Redaction | **exists** — `decision.Redactor.RedactText` |
| Provider credential custody | new — one secret held server-side |
| SSO / IdP RBAC | not required; OpenBox policy IS the authorization layer |

`ANTHROPIC_AUTH_TOKEN` is an arbitrary bearer — it can carry the `obx_` token already issued.

### The treadmill mostly dissolves

The documented risk — "a gateway that doesn't forward them breaks the corresponding
features" — punishes gateways that TRANSFORM. Per `llm-gateway-protocol`:

> "Treat the headers and body fields as open lists, not closed ones … A gateway pinned to
> an observed list strips the next capability's header or field and breaks it on the
> release that introduces it."

A pure passthrough that inspects without modifying is forward-compatible by construction.
**The hardest constraint on this design is also what immunizes it against release cadence.**

Conformance oracle: a running Claude apps gateway "serves a machine-readable version of
this contract at `GET /protocol`". Run one in CI to extract the spec and test against it.
Test dependency, not production.

### Chaining — supported variant, not the design

`claude-apps-gateway-config` allows `upstreams[].provider: anthropic` with `base_url`
pointing at a proxy you operate, plus `forward_user_identity: true` (adds
`x-claude-gateway-user-id`/`-user-email`; gateway refuses to start if `base_url` is the
Anthropic API). Only the `anthropic` provider redirects. Worth supporting for orgs already
running Claude apps gateway. Not worth designing around.

## THE constraint — inspect without modifying

> "A gateway that rewrites or redacts request bodies for content inspection breaks the
> pairing the same way stripping does, so **inspect without modifying**."

Consequences, all load-bearing:

1. **Cannot redact secrets from the outbound request.** Guardrail-redaction-at-source
   (open item in CLAUDE.md) is *unreachable at this layer*. Options are observe-and-record
   or refuse the whole call. Nothing in between.
2. **`system` array must pass byte-identical, block first.** Prepending / reordering /
   collapsing to a string defeats the positional attribution strip; the block then reaches
   the model and the prompt-cache key.
3. **`anthropic-beta` verbatim, never allowlist.** Header+body pairs travel together;
   splitting them = hard `400`. Only both-absent turns a feature off quietly.
4. **Error bodies unmodified.** Retry logic matches on upstream error wording; wrapping in
   an envelope breaks capability-rejection recovery even with the status preserved.
5. **Forward unknown `anthropic-*` / `x-claude-code-*` headers and body fields.**

Redaction still applies to the **captured copy** before attach (existing INV-2 ordering:
detect → redact → attach → cap → sign). Never to the forwarded bytes.

## Streaming — hard requirements

- "Inference responses must stream … a gateway that buffers complete responses before
  relaying them stalls the client."
- **Forward keep-alive pings.** CC counts every relayed byte incl. SSE `ping` and comment
  lines; aborts on 300s silence. "The upstream's pings are the only traffic during long
  thinking pauses, so if your gateway strips or buffers them, Claude Code aborts."
- Byte watchdog 180s on direct Anthropic API (`network-config`).

Capture must be a **tee on a passthrough**, never buffer-then-forward.

Opportunity: the ping channel doubles as an approval-hold mechanism — hold within watchdog
budget while `/evaluate` returns REQUIRE_APPROVAL, emitting pings. Elegant but unproven;
later phase, not P1.

## Endpoints

| Path | Required | Note |
|---|---|---|
| `POST /v1/messages` | yes | arrives as `/v1/messages?beta=true` — match on PATH |
| `POST /v1/messages/count_tokens` | optional | absent ⇒ CC counts via inference (wasteful) |
| `GET /v1/models?limit=1000` | optional | 3s timeout, **redirects = failure**; gates `/model` picker |
| `HEAD /api/hello` | no | warming probe, safe to reject |

## Identity available free on every request

| Header | Use |
|---|---|
| `x-claude-code-session-id` | "aggregate all requests from one session **without parsing request bodies**" |
| `x-claude-code-agent-id` | subagent that issued the request |
| `x-claude-code-parent-agent-id` | **parent agent** |

`parent-agent-id` closes a documented limit: CLAUDE.md records it as absent from hook
payloads, "so the tree is flat-by-agent_id rather than parented". The gateway path gives
the real tree.

## Cheaper paths, ranked (effort→impact)

1. **Bind `tool_response` + failure free-text in `mapper.go`** — hours. Zero references in
   the adapter today; data already on hook stdin. Closes tool AND MCP output (MCP flows
   through the same hooks). Do regardless of every other decision.
2. **Transcript tail for thinking** — extends existing `hookflow.TurnCursor` (ADR-0014).
   Thinking confirmed present in 5/6 real sessions on this machine. Meets req 3 WITHOUT
   the gateway. Undocumented format = accepted risk.
3. **OTel receiver** — `OTEL_LOG_RAW_API_BODIES=file:<dir>` gives untruncated full request
   + response JSON with a `body_ref` pointer; `OTEL_LOG_TOOL_CONTENT` gives tool I/O.
   Solves the volume problem. **Cannot** do headers, thinking (redacted at that layer
   permanently), or blocking. Defense-in-depth, not a substitute.
4. **OpenBox gateway** — only route to reqs 1 + 4 + 5 together.

Items 1–2 are weeks, independent of the gateway, and must not wait for it.

## What NOT to do

- **Do not build the MITM proxy.** CA key = universal TLS forgery on the dev machine,
  stored beside the signing key (ADR-0015 posture), and buys no assurance.
- **Do not redact or rewrite forwarded bodies.** Breaks beta pairing → hard 400s.
- **Do not buffer streams or strip pings.** Watchdog aborts.
- **Do not allowlist `anthropic-beta` values or body fields.** Breaks on the next release.
- **Do not wrap upstream errors.** Breaks retry recovery.
- **Do not rebuild identity.** `obx_` + DID already is the developer credential.
- **Do not skip items 1–2 waiting for the gateway.**

## Trade-offs

- **Availability inversion.** Today OpenBox down = ungoverned. After: OpenBox down =
  nobody codes. Cannot fail open past a service holding the connection. Directly reverses
  the `fail_closed:false` house posture. **This is the price of req 5 and it is not
  avoidable** — tamper-resistance and fail-open are mutually exclusive by construction.
- **CI ungoverned.** No service-token flow for gateway sign-in.
- **v1 is Anthropic Messages only.** Bedrock/Vertex/Foundry formats are separate work.
- **Guardrail-redaction-at-source unreachable** at the model layer.
- **Volume.** Full bodies per call; copy OTel's `body_ref`-to-sink pattern rather than
  inlining 64KB × ~52 calls/turn.
- **Not all traffic routes through it.** Fast-mode availability check and the WebFetch
  domain safety check call `api.anthropic.com` directly regardless of `ANTHROPIC_BASE_URL`.

## Condition under which this stops being right

If the org will not deploy managed settings, credential custody cannot be mandated, req 5
is unmeetable, and the gateway degrades to an expensive observe-only proxy. At that point
OTel + items 1–2 dominate on every axis. Switching cost stays low if the capture/mapping
layer is kept independent of the transport.

## Work checklist

- [ ] Bind `tool_response` → `activity_output.output` in `adapters/claude-code/mapper.go`, content-gated, redact-before-attach, conformance case on outbound bytes
- [ ] Bind `PostToolUseFailure.error`, `PermissionDenied.reason`, `StopFailure.error_details` (free text, gated)
- [ ] Extend `hookflow.TurnCursor` to lift `thinking` blocks; amend ADR-0014 allowlist; evolve `TestFinops_NoContentOnWire` to assert present-redacted-capped (must not pass trivially)
- [ ] ADR: OpenBox gateway as a new service (repo rule: new service ⇒ ADR)
- [ ] ADR amendment: gateway config install scope — user/managed, not project (conflicts ADR-0016)
- [ ] Prototype: `POST /v1/messages` passthrough + SSE tee + ping forwarding; assert forwarded bytes byte-identical and `system` array unchanged
- [ ] Gateway auth: accept `obx_` bearer from `ANTHROPIC_AUTH_TOKEN`, resolve to DID
- [ ] Map capture → `SpanData` (`request_headers`, `response_headers`, `request_body`, `response_body`, `http_*`)
- [ ] Set `attributes["http.method"]="POST"` + `attributes["http.url"]` — core's `isLLMCall` reads attributes, not root fields; retire `openbox.span_synthetic` and ADR-0018's fabricated attributes
- [ ] Identity mapping: `x-claude-code-session-id` → session, `x-claude-code-parent-agent-id` → agent tree
- [ ] `/evaluate` integration + verdict → forward | refuse; decide HALT rendering (status + body) against CC's retry-on-error-wording
- [ ] Conformance case: forwarded bytes unmodified while captured copy is redacted
- [ ] CI conformance against `GET /protocol` from a test Claude apps gateway
- [ ] Backend ask: retention/storage for body-class volume; server-side dedupe on developer events
- [ ] Testbed phase against a real gateway

## Success metrics

| Metric | Target |
|---|---|
| Tool output present on `ActivityCompleted` | 100% of PostToolUse events, capture ON |
| Forwarded request bytes vs received | byte-identical (conformance assertion) |
| `system` array position/shape | unchanged; attribution block first |
| Stream aborts attributable to gateway | 0 over a full testbed run |
| SSE ping forwarding | relayed within 1s of upstream emission |
| Thinking blocks captured per turn | ≥1 where extended thinking active |
| `TestFinops_NoContentOnWire` | passes non-trivially (redacted+capped, not absent) |
| Gateway added latency, ungated path | < 50ms p95 |
| Verdict round-trip, gated call | within watchdog budget; p99 < 5s |
| Bypass attempt (unset gateway env) | 0 model calls succeed |
| `GET /protocol` conformance suite | green on each Claude Code release |

## Unresolved questions

1. How does a HALT render to Claude Code? No `ask` concept at the model layer. Status +
   body must not trip capability-rejection retry (which matches on error wording).
2. Is the SSE-ping approval hold viable, or does the event-level watchdog (300s, needs
   *parsed events* not just bytes) abort it? Needs empirical test.
3. Does the org have managed-settings distribution? The whole assurance argument depends
   on it; repo notes E8-S8/S9 unshipped.
4. Retention posture for body-class volume — unmade backend decision since ADR-0019.
5. Codex: no gateway equivalent. Parity path unaddressed by this design.
