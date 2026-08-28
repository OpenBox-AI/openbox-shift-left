# Phase 09 verification — telemetry receiver (partial: the module, not the daemon)

**Date:** 2026-08-28 · **Host:** macOS 25.0.0 darwin/arm64, go1.27.0 ·
**Branch:** `feat/tool-content-capture` · **Gates:** phase 10

## Verdict

**Half the phase is done and verified; the other half is blocked by the host, not
by the design.** Stated before the evidence so it cannot read as a footnote.

Done and green: the `telemetry/` module, its dependency guard, loopback-only
config, the otlpreceiver wiring **compiled against the real API**, the consumers,
the emitter seam, and 27 tests including a mutation drill.

Not done: the emitter's production wiring, `telemetryservice`, the install
proof-order, doctor's lane block, the posture key, and the no-fakes control test.
Every one of them depends on binding a listener, which this host refuses.

## The environment, half unblocked

The dependency block **was** total and is now solved — see
[blocker-260828-phase-09-environment](blocker-260828-phase-09-environment.md).
Briefly: my first diagnosis (TLS interception) was wrong. The certificate served
for `proxy.golang.org` is genuine Google; what fails is that reading the macOS
keychain is denied, so Go's verifier has no roots. `SSL_CERT_FILE=/etc/ssl/cert.pem`
plus `GOPATH=$TMPDIR/gopath` fixes it **with `GOSUMDB` left on**, so module
checksum verification — the control that matters — is intact.

`go.opentelemetry.io/collector/receiver/otlpreceiver v0.159.0` is therefore
resolved, and the receiver is written against the pinned source rather than
recollection, which is what the phase demanded.

The **bind** block is unchanged and not configuration-fixable: `net.Listen` fails
on every address with `bind: operation not permitted`.

## What was built

| File | What |
|---|---|
| `telemetry/go.mod` | new module, 8 direct requires, all collector-family |
| `telemetry/config.go` | loopback-only `Validate()`, `DefaultAddr` 127.0.0.1:**8789**, `MaxRequestBodyBytes` |
| `telemetry/record.go` | the narrow projection phase 10 consumes, and its bounds |
| `telemetry/consume.go` | logs/traces/metrics consumers, attribute merge, the `Emitter` seam |
| `telemetry/receiver.go` | otlpreceiver factory wiring, three signals, start/shutdown, counters |
| `telemetry/guard_test.go` | direct-require allowlist + credential/file-read scan |
| `telemetry/{config,consume}_test.go` | 27 tests |
| `go.work` | 12 modules → 13 |

### Decisions worth not re-litigating

- **HTTP only; gRPC switched OFF.** The client exports over HTTP. Leaving the gRPC
  endpoint at its 4317 default would open a second unauthenticated content
  listener bound for nothing.
- **Port 8789, not 4318.** The OTLP default is what any other collector on the
  machine already holds — binding it either fails or, worse, succeeds after that
  collector dies and silently swallows exports meant for it.
- **Bounds set explicitly, not inherited.** `MaxRequestBodySize` 20MiB → **8MiB**
  (the library default is sized for a fleet collector; here the listener is
  unauthenticated by construction, so the default hands any local process a large
  memory budget), and `ReadHeaderTimeout` 0 → **10s** (zero means no deadline, so
  a half-open connection holds a goroutine indefinitely).
- **Traces and metrics are accepted and discarded, not projected.** Enabled
  because a 404 on a configured signal produces export errors in the governed
  tool's own logs — a lane meant to be invisible making noise in the thing it
  observes. Phase 10 decides whether either carries anything.
- **An emitter error never reaches the exporter.** This lane is additive by
  construction: a returned error becomes a retry the tool eventually surfaces, so
  a failing sink would degrade the session it exists to observe.
- **`client` and `decision` are NOT dependencies.** The plan budgeted for both;
  the `Emitter` interface removed the need. The module hands out a `Record` and
  never builds an event, signs a payload or runs the redactor — which is what
  keeps the collector tree quarantined. Its guard asserts their absence.

## Measurements (phase 06 discipline)

| | telemetry/ | gateway/ (comparison) |
|---|---|---|
| direct requires | **8** (all collector-family) | 2 |
| transitive packages | **492** | 381 |
| modules in graph | **124** | 206 |
| go.sum lines | 191 | — |

**Binary size — the number that needs a decision.** A minimal `main` linking the
receiver is **18.8 MB** against a **2.3 MB** baseline: **+16.5 MB**. The shipped
`openbox` binary is **17.0 MB** today, so wiring this lane into it roughly
**doubles** the binary every developer installs.

**Leak check: zero.** `gateway`, `decision`, `client`, `cli` and both adapters
have **0** collector requires. The module boundary is holding, and the guard is
what keeps it holding.

## Evidence

- 27 tests green; `-race` green; `GOWORK=off` release build green.
- **Mutation drill:** deleting the `requireLoopback` call makes 5 non-loopback
  addresses validate. Restored ⇒ green. The security control is load-bearing, not
  decorative.
- Workspace build, `go vet`, and both cross-compiles green across **13** modules.
- No regressions: every pre-existing module's non-listener tests still green.
- `go.sum` clean repo-wide — checked, given the corruption seen earlier.

## What is NOT verified

1. **Nothing has listened.** The receiver has never bound a port, so the HTTP path
   in front of the consumers is entirely unexercised: URL routing, both OTLP
   encodings, the body limit, the header timeout, and `Start`/`Shutdown` against a
   real listener. The consumers are tested by driving real pdata directly.
2. **Step 1's probe did not run** — it needs a listening stub plus a live client.
   So the env values this lane will write are still unchosen. OD3 makes that
   config input rather than a design fork, but it is unwritten.
3. **No record has reached a spool.** The emitter seam exists and has no
   production caller — which is precisely the gateway's `WithCapture` bug, and it
   stays open until the control test can run. It must not be called done on the
   strength of the stub emitter in `consume_test.go`; a fake at each end of a seam
   proves nothing about the seam.
4. Success criteria 1–4 are all bind-dependent and untested.

## Unresolved questions

1. **Should the receiver be linked into `openbox` at all, given +16.5 MB on a
   17 MB binary?** A separate daemon binary would keep the CLI small at the cost
   of a second artifact to distribute and version. This is a product/packaging
   call, not a technical one — and it is cheaper to make now than after the CLI
   wiring exists.
2. **Does the plan's ~13-key env activation still fit** once the probe names the
   protocol? The key list is written from the logger's proven set, but which
   endpoints are enabled depends on the probe.
3. Phase 09 as written also owns `telemetryservice`, doctor and the posture key.
   All are writable here but unverifiable, and each mirrors a phase-04 surface
   that itself has never run. Worth deciding whether they land now unverified or
   with the lane on a capable host.
