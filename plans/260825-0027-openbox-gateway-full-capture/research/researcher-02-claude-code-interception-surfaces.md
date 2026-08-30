# Full I/O Interception Surfaces — Claude Code + Codex

Scope: surfaces for full HTTP req/resp (headers+body) + full tool/MCP I/O capture. Env: installed `claude` 2.1.229, native Mach-O arm64 binary at `/Users/phuongvu/.local/share/claude/versions/2.1.229` (confirmed via `file`, 2026-08-24). Codex NOT installed here (`command not found`) — Codex claims below are doc/GitHub-sourced only, weaker tier than the binary-verified Claude Code claims.

**Confirmed**: hooks and OTel both cap out at bodies/content, NEVER raw HTTP headers. Only an egress proxy (base-URL swap) sees real headers. No single route gives full I/O + headers + sync gate at once — combo required (see Recommendation).

## Trade-off matrix

| Route | Yields | Cannot yield | Effort | Fragility | Sync gate? |
|---|---|---|---|---|---|
| 1. Egress proxy (`ANTHROPIC_BASE_URL`/`OPENAI_BASE_URL`) | Full HTTP req+resp incl. headers, raw model JSON, streaming SSE | Nothing informational — cost is operational (new SPOF) | Med-High: new gateway service | Low-Med: public documented iface; you own body-schema drift | YES — only route that can block/alter the model call itself |
| 2. OTel (`OTEL_LOG_*`) | Full req/resp JSON bodies, tool params/content, prompts/responses | HTTP headers (docs say explicitly none) | Low: env vars + OTLP collector | Med: `OTEL_LOG_TOOL_CONTENT` behind a beta flag; schema separate from dev-event | NO — async batched export |
| 3. Hooks (stdin JSON) | Tool identity, `tool_input`, `tool_response` (currently unbound), prompt text, assistant text, error signals | Headers, raw API body, non-tool MCP chatter | Very low: one mapper.go field bind | Low: stable public hook contract | Partial: PreToolUse/UserPromptSubmit sync; PostToolUse structurally post-hoc |
| 4. Transcript JSONL tail | Everything incl. `thinking`, `tool_use`, `tool_result`, `usage`, `model` | Headers, raw HTTP | Low-Med: extend existing `TurnCursor` | HIGH: undocumented internal format | NO — written after the fact |
| 5. MCP stdio wrap | MCP protocol chatter only (non-tool) | Nothing new for tool-level I/O (already route 3) | Med: per-server shim, breaks on HTTP-transport servers | Med-High: config-fragile | Technically yes, redundant w/ existing PreToolUse gate |

## 1. Egress proxy — the only header route

Binary evidence: `strings` on the installed 2.1.229 binary shows literal `ANTHROPIC_BASE_URL`, `ANTHROPIC_CUSTOM_HEADERS`, `ANTHROPIC_AUTH_TOKEN`, `HTTPS_PROXY`, `HTTP_PROXY`, `NO_PROXY`, `NODE_EXTRA_CA_CERTS`, `CLAUDE_CODE_HTTPS_PROXY`, `CLAUDE_CODE_PROXY_URL`, `CLAUDE_CODE_CLIENT_CERT/KEY/PASSPHRASE` (mTLS), `CLAUDE_CODE_CERT_STORE`, `CLAUDE_CODE_GZIP_REQUEST_BODIES` (verified 2026-08-24, this session).

Doc: https://code.claude.com/docs/en/network-config (WebSearch synthesis) — `ANTHROPIC_BASE_URL` is the documented LLM-gateway hook; official NTLM/Kerberos guidance is literally "stand up a gateway, point `ANTHROPIC_BASE_URL` at it." Env read once at startup, can live in `settings.json`. Real friction (GitHub): `NODE_EXTRA_CA_CERTS` not honored from `settings.json` (claude-code#22512), not inherited by Desktop's Code-pane subprocess (#24916); MCP servers don't inherit `HTTPS_PROXY`/`NODE_EXTRA_CA_CERTS` from parent — must set per-server.

Two sub-approaches: **base-URL swap** (recommended) — point at your own HTTPS gateway with a normally-trusted cert, gateway forwards to `api.anthropic.com`, no CA changes, no MITM; **true MITM** — keep real hostname, intercept via `HTTPS_PROXY`+decrypting proxy, trust its cert via `NODE_EXTRA_CA_CERTS`/`CLAUDE_CODE_CERT_STORE`, heavier, inherits the CA bugs above.

Streaming SSE: swap-gateway IS the upstream from CC's view — can tee/buffer the live SSE stream while passing it through (standard reverse-proxy pattern, NOT Anthropic-documented — engineering inference, flagged). Codex equivalent CONFIRMED via docs only (not binary): `OPENAI_BASE_URL` overrides the built-in provider base URL; `config.toml` `model_provider` + `[model_providers.<id>]` (`base_url`, `wire_api`, headers) for custom providers (https://developers.openai.com/codex/config-advanced, https://developers.openai.com/codex/config-reference). Same swap pattern applies.

Architectural cost: a NEW SERVICE — repo's own rule ("new table/endpoint/service requires a decision record", `CLAUDE.md`) applies. Also a new SPOF: gateway down = every model call fails, unlike hooks (OpenBox down today just means ungoverned, CC
still works).

## 2. OTel — bodies not headers, async only

Per https://code.claude.com/docs/en/monitoring-usage (WebFetch summary; env-var NAMES cross-verified via binary `strings`, size-limit/default NUMBERS not independently tested — UNVERIFIED beyond names):

| Var | Exports | Default | Limit |
|---|---|---|---|
| `OTEL_LOG_USER_PROMPTS` | user prompt text | off | 60KB UTF-16 units (`CLAUDE_CODE_OTEL_CONTENT_MAX_LENGTH`) |
| `OTEL_LOG_ASSISTANT_RESPONSES` | assistant text only (no thinking/tool blocks) | falls back to prompts flag | 60KB |
| `OTEL_LOG_TOOL_DETAILS` | tool names/params, MCP server+tool name, error messages | off | ~4KB, 512 char/value |
| `OTEL_LOG_TOOL_CONTENT` | tool input+output bodies, span events | off; needs `CLAUDE_CODE_ENHANCED_TELEMETRY_BETA=1` + traces | 60KB |
| `OTEL_LOG_RAW_API_BODIES` | full Messages API req+resp JSON (system, messages, tools, usage) | off | 60KB inline; unlimited w/ `=file:<dir>` |

All five: doc states "HTTP headers: not applicable" — confirms the no-headers premise extends to OTel. Export = OTLP grpc/http-protobuf via `OTEL_LOGS_EXPORTER`/`OTEL_TRACES_EXPORTER`, a pipe separate from the dev-event schema — ingest needs a new OTLP collector in core (same decision record trigger as route 1). Async/batched (`OTEL_LOGS_EXPORT_INTERVAL`) —
cannot gate.

## 3. Hooks — biggest gap is `tool_response`, fixable in mapper.go today

Probe report (trusted, not re-probed): `plans/reports/probe-260813-2329-claude-code-hook-surface.md:36-42`

| Hook | Fields on stdin | Produced live in probe? |
|---|---|---|
| `PostToolUse` | `tool_name, tool_input, tool_response, tool_use_id, duration_ms?` | yes |
| `PostToolUseFailure` | `tool_name, tool_input, tool_use_id, error(string), is_interrupt?, duration_ms?` | yes |
| `PermissionDenied` | `tool_name, tool_input, tool_use_id, reason(string)` | not triggered in probe — IS wired in code (below) |
| `StopFailure` | `error`(10-value enum), `error_details?`, `last_assistant_message?` | not triggered in probe |
| `Stop`/`SubagentStop` | `last_assistant_message?` + subagent fields | yes |

Current binding, checked against code directly (probe only proves the field exists on stdin, not that shift-left forwards it): **`tool_response` has zero references in `adapters/claude-code/mapper.go`** (grepped `ToolResponse`/`tool_response`, no hits) — on stdin, dropped by the adapter. **The single biggest "output not captured" gap**: full tool result (file read, command stdout, MCP result) never reaches core. `PermissionDenied.reason` is bound but neutered: `mapper.go:279` runs it through `enumOr(e.Reason, reasonValues)` (closed-enum categorization), raw text discarded by design per `contracts/dev-event/schema/dev-event.schema.json:185`. `PostToolUseFailure.error`/`StopFailure.error_details` free text NOT bound; only closed-enum `error_type` ships (`schema/dev-event.schema.json:191`). `last_assistant_message` IS bound, content-gated, into the one that decision span (`mapper.go:395-396`). `tool_input` already egresses on the PRE-call gate path (PreToolUse → `/evaluate`, secret-redacted) per project `CLAUDE.md` — not re-verified this session.

Effort to close the gap: bind `tool_response` the same content-gated way `last_assistant_message` already is — smallest, lowest-risk change of all 5 routes, zero new services. Cannot ever yield headers/raw HTTP (hook stdin is CC's own JSON, never Anthropic's wire bytes) — confirms the task's starting premise.

## 4. Transcript JSONL — richest content, weakest guarantee

Live sample, 6 real sessions under `~/.claude/projects/**/*.jsonl` (2026-08-24, keys only): top-level `type, uuid, parentUuid, sessionId, timestamp, cwd, gitBranch, userType, version, isSidechain, entrypoint, message, promptId, isMeta, attachment`; `message: {role, content}`; content block types seen: `text, thinking, tool_use, tool_result` (5/6 sessions; 1 trivial session had only `text`). Assistant messages carry `usage`+`model` (True in 5/6) — matches this repo's own `usage.go` INV-2 discussion in `CLAUDE.md`.

Genuinely full I/O incl. extended-thinking — nothing else here gets thinking blocks. But format is Anthropic-internal, UNDOCUMENTED, no schema contract — highest fragility of all 5 routes, can change silently on any point release. Repo has a narrower working precedent already: `hookflow.TurnCursor` (that decision, byte-offset cursor over this file) tails it for turn/assistant-text today — extending it to lift `tool_use`/`tool_result`/`thinking` is the smallest lift of any new capability here, but inherits undocumented-format risk beyond what that decision already reads. Observe-only, always
post-hoc.

## 5. MCP — already covered by hooks for tool-level I/O

Live `~/.claude.json` `mcpServers` sample confirms shape: `{command, args, env, type:"stdio"}` — a plain subprocess spec, stdio-wrapping is mechanically possible but mostly unnecessary: MCP calls are named `mcp__<server>__<tool>` and flow through the SAME `PreToolUse`/`PostToolUse` hooks as any tool (`mapper.go:627 splitMCPName`; fixture `contracts/dev-event/conformance/testdata/valid/tool_call_mcp.json`). Closing the route-3 `tool_response` gap ALSO closes MCP tool-output capture, free. A stdio wrapper would only surface MCP-protocol traffic that never becomes a tool call (resource/prompt listing, `listChanged`) — narrow value, per-server cost, breaks for HTTP-transport MCP servers (needs route-1-style proxying instead), duplicates the PreToolUse gate if used for blocking. Not recommended as a distinct route.

## Recommendation (ranked)

1. **Bind `tool_response` in `mapper.go`** (route 3 fix). Lowest effort/fragility, zero new services, closes tool+MCP output capture in one change. Do regardless of anything else.
2. **Egress proxy / base-URL swap** (route 1), only if raw HTTP headers are a hard requirement. Real new service + decision record + SPOF risk — justified only because it's the sole way to get headers, raw model JSON, true pre-flight gating. Treat as a distinct initiative.
3. **OTel**, only as bootstrap/defense-in-depth IF core already runs an OTLP collector (unverified) — else it's a second new service for a subset of routes 1+3, minus headers.
4. **Transcript tailing**, narrowly, only to extend `TurnCursor` for `thinking` (nothing else here gets thinking blocks) — accept the undocumented-format risk explicitly.
5. **MCP stdio wrapping** — skip. No incremental tool-I/O value over the route-3 fix; revisit only if MCP protocol-level (non-tool) visibility becomes a stated requirement.

No single route satisfies "ALL inputs/outputs + headers" — combination is (1)+(3), or all four if thinking-block capture is required.

## Limitations

- Codex evidence is doc/GitHub-only (not installed here) — weaker tier than Claude Code's binary-verified claims; Codex's route-3 equivalent not checked (budget; `CLAUDE.md` already notes Codex's hook surface is thinner than CC's).
- OTel size-limit/default numbers came from one WebFetch summary, not independently triggered against the live binary (only env-var NAMES cross-verified via `strings`).
- No route tested end-to-end (no proxy stood up, no OTLP collector run, no live transcript tail) — this is a surface enumeration, not a working prototype.
- `contracts/dev-event/MAPPING.md`/`COVERAGE.md` not read — may already cover some route-3 gaps found here.

## Unresolved questions

1. Does OpenBox core already run/plan an OTLP collector? Decides if route 2 is reuse or new build.
2. Is a gateway SPOF (route 1) acceptable, or must calls fail-open if the gateway is down? Repo's `/evaluate` posture (`fail_closed:false` default) suggests fail-open is house style — same call needs making for a gateway outage.
3. `PermissionDenied`/`StopFailure` never triggered live in the referenced probe — worth a targeted re-probe before building on their schema-only shape.
4. Does binding `tool_response` risk the same "not content, don't smuggle a new goal" problem that decision solved for `signal_args` (age.go:112-137)? Needs a check before shipping.
