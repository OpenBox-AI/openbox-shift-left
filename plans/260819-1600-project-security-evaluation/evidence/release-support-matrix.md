# Project assurance MVP support matrix

No project-runner tuple is supported. The withdrawn and rejected rows are
retained below with their reasons intact; none is reachable as a fallback.

| Status | Platform and host | Project tuple | Model and OpenBox system | Evidence-backed result |
|---|---|---|---|---|
| Withdrawn | macOS 26.5.2 / Darwin 25.5.0 arm64; first-party OpenBox Seatbelt profile through `/usr/bin/sandbox-exec`, backend `8290e4be...`, standalone | Claimed Node 26.7.0/npm 11.19.0/Mastra 1.8.0 tuple | Deterministic local model harness and historical Ollama relay observations | The profile isolated outbound ports but its bind rule also permitted `0.0.0.0`; it allowed reads outside the snapshot. The runner executed from ambient CWD and accepted Node 22 outside the claimed tuple. The CWD defect is fixed, but support is withdrawn. |
| Withdrawn | macOS 26.5.2 / Darwin 25.5.0 arm64; native Codex Seatbelt, `codex-cli 0.149.0`, binary `f4a74117...`, standalone | Node 26.7.0; npm 11.19.0; Mastra 1.8.0; OpenBox Mastra SDK 1.0.0; base SDK 1.0.1; exact package lock | Local Ollama 0.31.1, `granite4.1:3b`, digest `6fd34935...`, zero monetary cost; fresh run-owned Docker Compose OpenBox system plane | Historical functional runs completed, but a 2026-08-23 exact probe reached both declared and undeclared loopback ports. The mandatory endpoint-isolation gate failed. |
| Unsupported | macOS arm64; standalone `@anthropic-ai/sandbox-runtime` 0.0.73 | No project tuple qualified | No model or OpenBox system invoked | Parent and child reached unapproved loopback ports; the common probe rejected the tuple in three repetitions. |
| Unsupported | macOS arm64; inherited Claude Code 2.1.235 | No project tuple qualified | No model or OpenBox system invoked | Pinned executable absent; retained proof did not confine the Claude parent network. |
| Unsupported | macOS arm64; inherited Claude Code 2.1.240 | No project tuple qualified | No model or OpenBox system invoked | Installed version/digest is unqualified drift and was not substituted. |
| Unsupported runtime | Linux and Windows | Compile-only | None | Build compatibility passed; no runtime sandbox/project/model/governed proof exists. |
| Unsupported unqualified | Any other platform, sandbox, framework, SDK, model, or version | Unlisted | Unlisted | No inference or compatibility shim is permitted. |

Active project execution fails before project/profile reads for every row.
There is no unsandboxed retry and no substitution between drivers. Docker Compose remains
allowed only for the disposable local OpenBox system plane; it is never a
project sandbox. `maxProcesses=32` is declared and correlated but is not
hard-enforced by any tuple, including the withdrawn ones. `/usr/bin/sandbox-exec`
is deprecated by Apple; this adds no new exposure, because both rejected
candidates already depended on it. The pinned backend digest and kernel release
mean an OS update keeps the row unsupported rather than silently widening it.
Retention is `redacted_digests`; raw content is not persisted.

## Historical repetition ledger

| Proof | Repetitions | Result |
|---|---:|---|
| Codex common native probe | 2 | Byte-identical qualified evidence, including unavailable/fallback behavior |
| Deterministic baseline | 5 | 5 exploitable |
| Local Ollama baseline | 5 | 5 exploitable; input 242-246 tokens, output 70-74 tokens, total cost USD 0.00 |
| Disabled-hook negative | 5 | Missing coverage; never promoted to no-action or blocked |
| Sandbox-denial negative | 5 | `sandbox_prevented`; never promoted to governed blocked |
| Linked baseline/governed pack pair | 1 | Baseline one sink effect; governed real Core BLOCK and zero sink effects |
| Claude standalone common probe | 3 | Rejected on unapproved loopback reachability |

Machine-readable bindings and full SHA-256 evidence references are in
`release-support-matrix.json`.
