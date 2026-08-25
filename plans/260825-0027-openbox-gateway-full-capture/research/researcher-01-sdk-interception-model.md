# openbox-sdk-python: HTTP/LLM I/O interception model

Reference repo: `/Users/phuongvu/Code/openbox/openbox-sdk-python`. All paths relative to it unless noted.

## 1. Interception mechanism

In-process only. No network proxy, no OS-level capture. Two distinct techniques, both requiring the governed code to run in the SAME Python process/import space as `openbox_core`:

| Target | Mechanism | Install point | Timing vs real call |
|---|---|---|---|
| `requests` | OTel `RequestsInstrumentor().instrument(request_hook=..., response_hook=...)` | `install_requests()`, `openbox_core/instrumentation/http.py:220-227` | hook fires inside OTel's own wrap, before/after real send |
| `httpx` (sync+async) | (a) OTel `HTTPXClientInstrumentor` request-hook (fallback/compat) AND (b) direct monkeypatch of `httpx.Client.send`/`AsyncClient.send` (primary) | `install_httpx_body_capture()`, `http.py:472-586` | patch wraps the ORIGINAL `send`; captures request pre-call, calls `_original_httpx_send`, captures response post-call — comment at `http.py:279-284` says OTel hooks alone can't read httpx bodies (unread stream / unsafe response stream), hence the send patch owns both stages |
| `urllib3` | OTel `URLLib3Instrumentor` hooks | `_urllib3_request_hook`/`_urllib3_response_hook`, `http.py:634-` (suppressed when `requests` is in use, `http.py:601-603` comment) |
| `urllib` | OTel hooks | `http.py:759-791` |
| function/tool calls | `@governed` decorator, NOT a patch — wraps the specific function at decoration time | `openbox_core/instrumentation/function.py:38-70` |
| LLM SDKs (OpenAI/Anthropic client libs) | **NOT separately instrumented.** `HookType.LLM_CALL` exists but `llm.py` is a 3-line placeholder: `"""LLM provider instrumentation placeholder.""" __all__: list[str] = []` (`instrumentation/llm.py:1-3`). LLM I/O is captured incidentally because those SDKs make calls over `httpx`/`requests`, which ARE patched. |

Install order is asserted explicit in `manager.py:60-84`: OTel provider → executor ContextVar patch → publish `HookRuntime` (`shared.py`) → per-toggle wrappers. `install_httpx_body_capture()` must run AFTER `install_httpx()` (`manager.py:71-75`, `http.py:473-474`) so the captured "original send" already carries the OTel request hook — layered patching, order-dependent.

## 2. Request capture

| Field | Source | Redaction / cap |
|---|---|---|
| method | `request.method` (bytes-decoded for httpx: `_decode_method`, `http.py:257-261`) | none |
| url | `request.url` str() | none, but checked against ignore-list first |
| headers | `dict(request.headers)` → `sanitize_headers()` | `_SENSITIVE_HEADERS` frozenset redacts to `"[REDACTED]"`: `authorization, proxy-authorization, cookie, set-cookie, x-api-key, api-key, x-auth-token, x-amz-security-token` (`http.py:89-100`, `103-115`) — key-name matching only, no value pattern matching |
| body | `requests`: `request.body` decoded utf-8/ignore (`http.py:150-155`). httpx: reads ONLY already-buffered attrs — `request._content` (send-patch, `_capture_httpx_request`, `http.py:394-405`) or `stream._stream/body/_body` (OTel-hook fallback, `_httpx_request_body`, `http.py:281-303`) — explicitly NEVER touches a live/unread stream property (comment `http.py:279-284`) | no size cap at capture time (see §5 for where the cap actually applies) |

## 3. Response capture

| Field | Source | Safety mechanism |
|---|---|---|
| status | `response.status_code` | — |
| headers | `sanitize_headers(response.headers)` | same redact list as request |
| body | `requests`: `response.text` gated by `_is_text_content_type` (json/text/xml marker in content-type, or absent=assume text) (`http.py:196-203`, `40-45`). httpx: same content-type gate, then `response.text`, wrapped in try/except — `ResponseNotRead` (raised for an unread streaming response) is CAUGHT, body left `None`, "safe, no consumption" (`_capture_httpx_response`, `http.py:422-444`) |
| correlation | request+response are captured in ONE call frame inside the send patch (no separate hook pairing needed); OTel-hook fallback path correlates via `_httpx_span_var` ContextVar stashed at request time (`http.py:378-379`, `409-419`) | span identity is the join key |
| duration | `time.perf_counter()` delta, stashed per-span-id in `_hook_timings` dict (max 4096 entries, cleared on overflow) (`http.py:172-188`) | — |

Binary/non-text bodies are never captured (`_is_text_content_type` gate) — no base64 fallback exists in this file.

## 4. Preflight gate — synchronous, blocking, in-call-stack

Confirmed synchronous and BEFORE the real I/O:
- Send patch: `runtime.preflight(...)` called, THEN `response = _original_httpx_send(...)` (`http.py:498-509`, same pattern async at `541-552`).
- `HookRuntime.preflight()` (`hooks/preflight.py:64-79`) builds an `EventEnvelope` then calls `self._gate.preflight(event)` synchronously (gate object itself lives outside the read file set — `runtime.gate`, not in `openbox_core/hooks/`).
- Blocking is via **exception raised into the caller's stack**, not a return code: `Verdict.should_stop()` → `self._adapter.raise_hook_blocked(result)`, declared `NoReturn` by contract; a defensive `raise GovernanceBlockedError(...)` follows in case the adapter returns anyway (`preflight.py:150-165`). This exception propagates up through `_patched_send`, so `_original_httpx_send` is simply never called — the real HTTP request never leaves the process.
- `REQUIRE_APPROVAL` verdicts block synchronously on `ApprovalPoller.wait_for_decision(...)` (long-poll) before returning `True`/proceed (`preflight.py:213-230`).
- API-call failure to reach Core itself: `ContractError`/`GovernanceAPIError` on a STARTED hook always fail closed → synthesized `Verdict.HALT` (`preflight.py:120-141`, comment explains fail-open is only for the completed/telemetry path, never for a started/gating hook).
- `completed()` (post-call telemetry hook) is fire-and-forget for the CURRENT call — never undoes the operation that already ran; a stop verdict there only marks FUTURE calls on the same activity aborted (`preflight.py:107,254-256,281-285`, `_mark_stopped`/`is_activity_aborted` check at `preflight.py:104-113`).

## 5. Wire shape — exact field names

`build_evaluate_payload()` (`wire/evaluate_payload.py:24-42`) assembles the `/api/v1/governance/evaluate` body: `EventEnvelope` top-level fields (`event_type`, `hook_trigger`, `activity_id`, `activity_type`, `timestamp` — from `hook()` in `hooks/events.py:52-58`) + `"spans": [...]` (flat `SpanData` dicts) + `"span_count": len(...)`.

Each span dict, normalized by `to_core_span_data()` (`wire/core_span.py:57-116`):

| Wire key | Notes |
|---|---|
| `request_body`, `response_body` | verbatim keys; ONLY these two are in `_TRUNCATABLE_FIELDS` (`core_span.py:37`) |
| `request_headers`, `response_headers` | dicts, sensitive values already `"[REDACTED]"` from capture time — NOT re-redacted here |
| `http_method`, `http_url`, `http_status_code`, `duration_ns`, `error` | family-specific root fields, always present (null if absent) per `_ROOT_FIELDS_BY_HOOK_TYPE` (`core_span.py:108-111`) |
| `hook_type` | `"http_request" \| "db_query" \| "file_operation" \| "function_call" \| "llm_call"` (`contracts/otel_spans.py:39-43`) — spans are FLAT, no nested `{"otel","openbox"}` envelope, explicitly stripped (`core_span.py:75-78`: `wire.pop("otel"/"openbox"/"data"/"metadata")`) |
| `span_id`(16 hex)/`trace_id`(32 hex)/`parent_span_id`(16 hex) | hex STRINGS not raw ints (`core_span.py:9-11`) |
| `start_time`/`end_time`/`duration_ns` | epoch NANOSECONDS; started-stage spans emit explicit `end_time: null` (`core_span.py:12-14`) |
| `attributes` | OTel-native attributes ONLY — separate from the semantic http_*/request_*/response_* fields |

**Truncation cap applied HERE, not at capture**: `PrivacyConfig.max_body_size` default **65536 chars** (`config.py:118`), applied via `truncate_string(value, privacy.max_body_size)` only to `request_body`/`response_body` at wire-normalization time (`core_span.py:95-104`), i.e. right before signing — headers are never truncated, only redacted at capture.

## 6. Self-instrumentation guard

Config's `api_url` is unconditionally added to the ignored-URL-prefix set at install time: `http_instrumentation.set_ignored_url_prefixes({config.api_url, *extra_ignored})` (`manager.py:63-65`). Every hook checks `should_ignore_url(url)` FIRST and returns before any capture or `runtime.preflight()` call (e.g. `http.py:147-149`, `489-490`). Matching is on a NORMALIZED prefix (lowercased scheme+host, default port made explicit, no trailing slash) specifically because httpx normalizes request URLs and a raw-string compare would miss a case/port variant and cause unbounded recursion (`_normalize_url_prefix`, `http.py:56-72`, module docstring `http.py:7-9`).

## 7. What is Python/in-process-only — cannot port to an out-of-process Go client governing a closed binary

| Mechanism here | Why it requires being IN-PROCESS with the governed code | Consequence for shift-left |
|---|---|---|
| Monkeypatching `httpx.Client.send`/`requests` adapter internals | Rewrites the CLASS METHOD object in the same interpreter; only works because the governed app `import`s `openbox_core` and shares its module/class objects | Shift-left governs Claude Code/Codex as opaque compiled binaries it does not import — there is no class object to monkeypatch. Confirms current shift-left design (external hook JSON over stdin/stdout) is the ONLY available seam, not a stopgap |
| Blocking via `raise` into the caller's own call stack (`raise_hook_blocked`, NoReturn, `preflight.py:150-165`) | The exception must unwind through the SAME stack that would have made the HTTP call | An out-of-process governor cannot throw into another process's stack. Shift-left's equivalent is coarser: deny at the hook's own request/response boundary (its `PreToolUse` stdout contract), which the closed binary must voluntarily honor — it cannot prevent a raw HTTP call the binary makes without going through that hook |
| Reading unread stream internals (`request._content`, `stream._stream/_body`, `http.py:281-303,394-405`) | Depends on direct memory/object access to the exact private attrs of the installed httpx version | Impossible across a process boundary; a Go client can only see what the binary's own hook payload chooses to expose |
| ContextVar-based span/request correlation across a patched call (`_httpx_span_var`, `_httpx_patch_span_var`, `http.py:376-379`) and the cross-thread executor patch (`manager.py:57-59`) | In-memory, per-interpreter state | N/A out-of-process; correlation must instead use IDs already present in the tool's own hook payload (e.g. `tool_use_id`) |
| **The governed surface itself**: any HTTP call the instrumented library makes is visible, including the LLM provider SDK's raw request/response to `api.anthropic.com`/`api.openai.com`, because that SDK also runs INSIDE the same process on the patched `httpx` | The trust boundary in this SDK is "code that imports openbox_core" | For shift-left, the model call happens entirely INSIDE the closed Claude Code/Codex binary — full HTTP headers/body to the LLM provider are categorically unavailable. Shift-left already reflects this: ADR-0018's turn capture takes the assistant reply from the `Stop` hook's TRANSCRIPT field, not from an intercepted HTTPS response — this is the ceiling, not a gap to close |

## Unresolved questions

1. `runtime.gate` (the object whose `.preflight()`/`.completed()` actually perform the HTTP POST to `/evaluate`) lives outside `openbox_core/hooks/` and `instrumentation/` — not read; exact sync-HTTP-client and retry/timeout behavior for that call is unconfirmed (inferred synchronous from call-site ordering only).
2. Whether `db.py`/`file.py` instrumentors share `_SENSITIVE_HEADERS`/truncation patterns or have their own — out of scope per task file list, not read.
3. No explicit per-request byte-size read cap found in `http.py` itself (e.g., a streaming response could be fully materialized into memory via `.text` before the 64KB wire cap applies at `core_span.py`) — worth confirming this isn't a memory-DoS vector on large LLM streaming responses, since shift-left's own doc already caps at "first 64KB" server-side but this SDK's client-side behavior reads the full text first.
