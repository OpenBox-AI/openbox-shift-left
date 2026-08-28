# Phase 11 spike — the gate answer: **PROCEED**

**Date:** 2026-08-29 · **Host:** the owner's macOS machine (bind available) ·
**goproxy:** v1.9.0 · **Branch:** `feat/tool-content-capture`

## Verdict

**goproxy forwards byte-identically and streams SSE per chunk. The gate's
pre-decided PROCEED branch is taken; phase 11's service work is unblocked.**

Five tests, five passes, on a real socket:

| test | what it settles |
|---|---|
| `TestGoproxyForwardsIdentically` | method, target, body, Content-Length and every sent header forwarded verbatim; **no** `X-Forwarded-For`/`X-Forwarded-Host`/`X-Forwarded-Proto`/`Via`/`Accept-Encoding` added; no header the client never sent |
| `TestClientAcceptEncodingSurvives` | a client's explicit `Accept-Encoding: identity` reaches the provider unchanged |
| `TestGoproxyDefaultsBreakByteIdentity` | a **stock** goproxy does NOT preserve it — so the settings below are load-bearing, not decoration |
| `TestGoproxyStreamsPerChunk` | four SSE chunks arrive separately, each released only after the previous was read |
| `TestStreamingTeeDoesNotBuffer` | the capture tee sees the whole body while per-chunk delivery to the client is unaffected |

## Byte-identity needs THREE non-default settings, and the plan named one

`NewIdentityProxy` sets them; the negative control proves each matters.

- **`KeepAcceptEncoding = true`.** The plan anticipated goproxy *injecting*
  `Accept-Encoding: gzip`. v1.9.0's actual hazard is the opposite:
  `RemoveProxyHeaders` **deletes the client's own** header. A client that asked
  for `identity` and had its request rewritten is as much a byte-identity break
  as an injected header, and it fails in the more dangerous direction — the
  provider may then compress a reply the client cannot read.
- **`Tr.DisableCompression = true`.** With `Accept-Encoding` absent, Go's own
  transport adds `gzip` on the caller's behalf and transparently decompresses the
  reply, so the bytes reaching the client are not the bytes the provider sent.
  Same setting, same reason, as `gateway/proxy.go` sets on its own transport;
  goproxy's field doc points at it explicitly.
- **`PreventCanonicalization = true`.** Header names pass through as written.
  Measured limit: this only affects the **MITM path's request reader**
  (`https.go:453`), so on the plain path it is inert — kept because the MITM path
  is the one production will use.

**Header ORDER is not preserved and cannot be.** `net/http` models headers as a
map, so no Go proxy can hold their order — and the gateway's own identity suite
asserts presence, values and absence-of-additions, never order, for exactly that
reason. **The plan's "header order preserved" criterion is unachievable as
written** and should be struck rather than carried forward as an unmet
requirement.

## Per-chunk streaming works, by two different mechanisms

Read from the source and then confirmed by the run:

- **Plain HTTP** wraps the response writer in `flushWriter`, which flushes after
  every write — but only when content-type is `text/event-stream` or
  transfer-encoding is chunked (`http.go:100-104`). Anthropic streaming satisfies
  the first.
- **MITM** instead does `resp.Write()` straight to the raw TLS conn
  (`https.go:574`), so there is no intermediate buffer to hold a chunk back.

The tests exercise the **plain-HTTP path only**. See the limits below.

## What this spike does NOT settle

Stated plainly, because the gate's authority is exactly as wide as what ran:

1. **The MITM/TLS path was never executed.** Its per-chunk behaviour is a source
   reading, not a measurement. `RemoveProxyHeaders` is shared between the paths
   (verified), so the header findings transfer; the response-copy mechanism is
   **different code** and does not. Phase 11's own conformance work has to run
   the CONNECT path.
2. **No CONNECT, no CA, no allowlist** was exercised — none of it exists yet, by
   design: the gate comes before service code.
3. **First-byte latency was not measured**, only chunk separation. A relay could
   in principle deliver each chunk separately and still add a constant delay; the
   plan's criterion mentioned latency and this does not cover it.
4. **HTTP/2 is out of scope and off** (`AllowHTTP2` false). An in-path relay
   doing frame-level work is its own gate; the gateway lane has never claimed it
   either.

## One correction to the source reading

goproxy's default transport uses a variable named `tlsClientSkipVerify`
(`proxy.go:176` → `certs.go:25`). It is `&tls.Config{}` — **empty**, so
`InsecureSkipVerify` is false and verification is stock. Checked rather than
assumed, because an in-path relay that skipped upstream verification would be a
downgrade this product cannot ship, and the name alone would have justified a
wrong "fix". A note lives at the call site for the next reader.

## Two test-side bugs this spike produced, worth recording

Both cost the owner a run, and both are the same class: **a test whose failure
mode is a hang, on a host where its author cannot execute it.**

- **The streaming test deadlocked itself, twice.** The client's `Do()` cannot
  return until response headers arrive; goproxy cannot push headers until it has
  body bytes (Go's `http.Server` buffers `WriteHeader`); so a first chunk gated
  behind a release the test only performs *after* `Do()` returns can never
  arrive. The first fix — flushing headers before the gate — could not work,
  because a flush has nothing to carry until the first write. Chunk 0 is now
  ungated.
  **This wore the costume of the gate's stop-and-report branch**, which is the
  worst possible disguise: reporting it would have killed the transport lane on a
  test bug.
- **A wrong hypothesis, recorded because it was plausible:**
  `PreventCanonicalization` breaking the content-type check that selects
  `flushWriter`. It does not — that field only touches the MITM request reader.

The durable mitigations: the streaming reads carry a per-read deadline, **and the
whole exchange carries a request-context deadline**, because a buffering relay
stalls in `Do()` where no read deadline reaches. A stalled `go test` reads as an
environment problem rather than a gate answer; 20 seconds now yields a named
failure.

## Dependency cost (phase 06/09 discipline)

| | transport/ | telemetry/ (comparison) |
|---|---|---|
| direct requires | **1** (goproxy) | 11 |
| transitive packages | **203** | 492 |
| modules in graph | 287 | 124 |
| go.sum lines | 16 | 191 |

`transport/` is its own module with its own dependency guard, and the reason is
not size: goproxy sits on the **credential path** — every model call, with the
developer's provider key in its headers, transits it. Inside `gateway/` it would
have breached that module's two-entry allowlist and moved credential-path code
outside the scan the allowlist protects. Guard drilled both directions.

## Unresolved questions

1. **Strike "header order preserved" from phase 11's criteria** — unachievable in
   Go, and not what the gateway proves either. Needs an explicit edit rather than
   being quietly ignored.
2. **First-byte latency** is in the plan's wording and untested. Worth deciding
   whether it is a real requirement or was shorthand for "does not buffer".
3. **The MITM path's streaming** is the one that matters in production and is
   read-only evidence today. It should be the first thing phase 11's conformance
   work executes, not the last.
