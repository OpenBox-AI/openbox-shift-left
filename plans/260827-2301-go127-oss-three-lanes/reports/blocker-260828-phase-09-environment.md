# Phase 09 environment — the dependency block is SOLVED; the bind block is not

**Date:** 2026-08-28 · **Host:** macOS 25.0.0 darwin/arm64, go1.27.0 ·
**Branch:** `feat/tool-content-capture`

## B1 — Go module fetching: **SOLVED**

My first diagnosis was wrong and is corrected here rather than deleted. I read
`x509: OSStatus -26276` as the sandbox terminating TLS with an untrusted
certificate. It is not: the certificate served for `proxy.golang.org` is
**genuine** — `subject CN=misc-sni.google.com`, `issuer C=US, O=Google Trust
Services, CN=WE2`. There is no interception. What fails is `unable to get local
issuer certificate`: reading the macOS **keychain is denied** in this sandbox
(`security find-certificate` returns nothing), so Go's darwin verifier has no
roots at all. Every module failed, including a `github.com/google/uuid` control —
which was the clue that it was the trust store and not the host.

Two environment settings fix it, and neither weakens a security control:

```
SSL_CERT_FILE=/etc/ssl/cert.pem     # 128 public roots, readable; keychain is not
GOPATH=$TMPDIR/gopath               # the sumdb cache lands here instead of ~/go/pkg/sumdb
```

`GOSUMDB` stays **on**. The relocation is only about where the checksum database
cache is written — the verification itself still runs, which matters because it is
the control that makes a compromised transport unable to serve tampered modules.
(Earlier in this session I reached for `GOSUMDB=off` to get past the same symptom.
That was the wrong instrument and it is not needed.)

Verified: `go get go.opentelemetry.io/collector/receiver/otlpreceiver` →
**v0.159.0**, with `collector/receiver v1.65.0`, grpc 1.83.0, protobuf 1.36.12,
~70 require lines. `go.sum` written clean — 149 lines, **zero** redaction
placeholders.

**D-OSS-2 is available.** Phase 09 can be written and compiled against the real
pdata/receiver API rather than from recollection, which is what the phase demands
("Read the pdata API from the pinned source, not docs summaries").

## B2 — TCP bind: **still blocked**

Unchanged and not configuration-fixable: `net.Listen` fails on `127.0.0.1:0`,
`[::1]:0` and `:0` with `bind: operation not permitted`. Re-tested after the B1
fix.

What that leaves undoable here: step 1's encoding probe (needs a listening stub
plus a live client), step 8's install proof-order (unit → start → **prove
listening** → env), step 10's no-fakes control test, and success criteria 1–4.
Also the 322 listener-dependent tests from phase 08.

What it still permits: the module and its guard, config + loopback `Validate()`,
the receiver and consumer wiring **compiled against the real API**, the emitter
seam, the service unit, doctor's reporting shape, the dependency/binary-size
measurement, and every unit test that does not open a socket.

## The lockfile hazard, still live

`go mod tidy` earlier produced a `cli/go.sum` with two real base64 checksums
replaced by the literal `${OPENBOX_REDACTED_ENTROPY}` — the repo's own redaction
placeholder matching high-entropy strings — after which the build failed on a
checksum mismatch. The scratch fetch above came back clean, so it is intermittent
rather than certain. **Check `go.sum` for that placeholder after any `go get` in
this repo**; the collector tree is large and the corruption is silent until a
build runs. This is the open `generic-api-key` item in `CLAUDE.md`, with lockfile
checksums as a second demonstrated victim class.
