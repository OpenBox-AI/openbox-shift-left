# Manual verification — tool status, failure signals, and the assistant-turn span

What that decision changed, checked by hand. **No OpenBox stack, no Docker, no
network** for the first section; ~15 minutes.

You are verifying four things:

1. a completed tool call now reports `status: "completed"` — the field that has
   pinned Tool Health at SUCCESS 0.0% for every session ever recorded;
2. a failed one reports `"failed"`, with a real duration;
3. a model turn carries the assistant's reply on one span, so Goal Alignment has
   something to score;
4. **with content capture off, none of that text goes anywhere** — and `status`
   still does.

Written for someone who has not read the plan. Every check says what it proves and
what it does not.

---

## Setup (5 minutes)

### 1. Build

```bash
go build -o /tmp/obx-t18/openbox ./cli/cmd/openbox
```

### 2. Fake credentials in an isolated home

`openbox auth` registers against a real control plane you do not have. Hand-write
what it would have written. **`OPENBOX_HOME` keeps this out of your real
`~/.openbox` — export it in every shell below.**

```bash
export OPENBOX_HOME=/tmp/obx-t18/home
mkdir -p "$OPENBOX_HOME" && chmod 700 "$OPENBOX_HOME"
```

Both values are *constructed here rather than pasted*, on purpose: a literal that
looks like a credential gets rewritten by this repo's own secret redactor the
moment it lands in a file, and you would copy a placeholder instead of a key.

```bash
{ printf 'OPENBOX_API_KEY=obx_'; printf '0%.0s' $(seq 1 48); printf '\n'
  printf 'OPENBOX_AGENT_PRIVATE_KEY=%s\n' "$(printf 'A%.0s' $(seq 1 43))="
} > "$OPENBOX_HOME/.env"
chmod 600 "$OPENBOX_HOME/.env"

cat > "$OPENBOX_HOME/dev.json" <<'EOF'
{"developer_did":"did:aip:7f3c9b2e-0000-5000-a000-000000000001","base_url":"http://127.0.0.1:8098"}
EOF
```

That is an all-zeros API key and a signing key of 32 zero bytes, base64. Both are
*test* values: the client signs with them and the stub does not check. `http://`
is accepted only because 127.0.0.1 is loopback — the client refuses plaintext to
any other host.

### 3. A stub that records what was POSTed

Everything below is an assertion about **outbound bytes**, so the stub keeps them
and replays them on `GET /`.

```bash
mkdir -p /tmp/obx-t18
cat > /tmp/obx-t18/stub.py <<'PY'
import http.server, json
BODIES = []
class H(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        BODIES.append(self.rfile.read(int(self.headers.get("Content-Length", 0))).decode())
        b = b'{"verdict":"allow"}'
        self.send_response(200); self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(b))); self.end_headers(); self.wfile.write(b)
    def do_GET(self):
        b = json.dumps(BODIES).encode()
        self.send_response(200); self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(b))); self.end_headers(); self.wfile.write(b)
    def log_message(self, *a): pass
http.server.HTTPServer(("127.0.0.1", 8098), H).serve_forever()
PY
python3 /tmp/obx-t18/stub.py &
sleep 1
```

Two helpers you will use constantly — one to send a hook payload, one to read back
what reached the stub:

```bash
hook() { /tmp/obx-t18/openbox hook claude-code "$1"; }          # payload on stdin
sent() { curl -s http://127.0.0.1:8098/ | jq -r '.[]' | jq -c "$@"; }
reset() { pkill -f obx-t18/stub.py; sleep 1; python3 /tmp/obx-t18/stub.py & sleep 1; }
```

### 4. Govern a scratch project

```bash
mkdir -p /tmp/obx-t18/proj && cd /tmp/obx-t18/proj
/tmp/obx-t18/openbox init --provider claude-code
export OPENBOX_ENFORCEMENT_FILE=/tmp/obx-t18/enf.jsonl
export OPENBOX_SPOOL_DIR=/tmp/obx-t18/spool
export OPENBOX_REALTIME=0     # deterministic: flush when we say, not on a debounce
export OPENBOX_ENFORCE=0      # observe path — these checks are about telemetry
```

`OPENBOX_SPOOL_DIR` matters twice: it keeps the spool out of your real config
directory (`OPENBOX_HOME` does **not** move runtime state), and T7 needs to know
where the turn cursor lives.

> **You will see this on every `SessionEnd` below, and it is not a failure:**
> `openbox hook: finops: transcript usage skipped: no transcript_path`. The
> SessionEnd payloads here carry no transcript, so the usage rollup has nothing to
> read. It goes to stderr and never to stdout — a hook that wrote stdout could
> block a session.

**Confirm the four new hooks were registered.** They are what an existing install
does *not* have until `init` re-runs:

```bash
jq -r '.hooks | keys[]' .claude/settings.local.json
```

**Expect** — 11 entries including `PostToolUseFailure`, `SubagentStart`,
`PermissionDenied`, `StopFailure`.

> ⚠️ If you are testing an install that predates this change, this is the step
> that fixes it. No re-init, no new events, and nothing warns you.

---

## Stack-free checks

Each drives the hook exactly as Claude Code does — subcommand, payload on stdin —
then flushes the spool at the stub.

### T1 — a completed tool call reports success ⭐

**Why it matters:** `IsSuccess` in openbox-core is one comparison against the
literal `"completed"`. No producer has ever sent the field, so every completed
call scored as a failure and the Tool Health matrix read 0.0% — for every
producer, forever.

```bash
reset
echo '{"hook_event_name":"PostToolUse","session_id":"t1","cwd":"/tmp/obx-t18/proj","tool_name":"Bash","tool_use_id":"tu_1","tool_input":{"command":"echo hi"},"tool_response":{"output":"hi"}}' | hook PostToolUse
echo '{"hook_event_name":"SessionEnd","session_id":"t1","cwd":"/tmp/obx-t18/proj","reason":"other"}' | hook SessionEnd

sent 'select(.event_type=="ActivityCompleted") | {activity_type, status, duration_ms}'
```

**Expect:**

```json
{"activity_type":"Bash","status":"completed","duration_ms":null}
```

**Proves:** the literal is on the wire, on the right event.
**Does not prove:** that core stores it or that the widget renders — see L1.
(`duration_ms` is null because no `PreToolUse` preceded this call, so the duration
stash had nothing to pair with. T2 exercises the paired case.)

### T2 — a failed tool call reports failure, with a duration ⭐

**Why it matters:** this is the half that makes SUCCESS% mean anything. It also
crosses two *different* hooks, so it is where the duration stash can silently
break — and a stash miss drops every failure out of the latency percentiles.

```bash
reset
echo '{"hook_event_name":"PreToolUse","session_id":"t2","cwd":"/tmp/obx-t18/proj","tool_name":"Bash","tool_use_id":"tu_2","tool_input":{"command":"exit 3"}}' | hook PreToolUse
sleep 1
echo '{"hook_event_name":"PostToolUseFailure","session_id":"t2","cwd":"/tmp/obx-t18/proj","tool_name":"Bash","tool_use_id":"tu_2","tool_input":{"command":"exit 3"},"error":"Error: exit code 3","is_interrupt":false,"duration_ms":562}' | hook PostToolUseFailure
echo '{"hook_event_name":"SessionEnd","session_id":"t2","cwd":"/tmp/obx-t18/proj","reason":"other"}' | hook SessionEnd

sent 'select(.event_type=="ActivityCompleted") | {status, duration_ms, interrupt: (.metadata.is_interrupt)}'
```

**Expect** `status` `"failed"`, a `duration_ms` near 1000, `interrupt` `false`.

Then confirm the tool's own error text did **not** ride along:

```bash
curl -s http://127.0.0.1:8098/ | grep -c "exit code 3"
```

**Expect `0`.** Free-text failure detail is deliberately unbound (that decision
owns it); `is_interrupt` is the structural half, and it is what separates a
user cancellation from a broken tool.

### T3 — a model turn carries the assistant's reply ⭐

**Why it matters:** Goal Alignment Trend and Recent Drift read assistant text from
`payload.Spans` and from nothing else, so a span-less session could never feed
them however much metadata it sent.

```bash
reset
export OPENBOX_CONTENT_CAPTURE=1 OPENBOX_FINOPS=1
cat > /tmp/obx-t18/transcript.jsonl <<'EOF'
{"type":"assistant","isSidechain":false,"timestamp":"2026-08-13T09:00:01.500Z","message":{"model":"claude-opus-4-8","content":[{"type":"text","text":"x"}],"usage":{"input_tokens":100,"output_tokens":30}}}
EOF
echo '{"hook_event_name":"Stop","session_id":"t3","cwd":"/tmp/obx-t18/proj","transcript_path":"/tmp/obx-t18/transcript.jsonl","last_assistant_message":"I refactored the spool and all 11 modules are green."}' | hook Stop
echo '{"hook_event_name":"SessionEnd","session_id":"t3","cwd":"/tmp/obx-t18/proj","reason":"other"}' | hook SessionEnd

sent 'select(.spans) | {n: .span_count, stage: .spans[0].stage, type: .spans[0].semantic_type, attrs: .spans[0].attributes}'
```

**Expect** exactly:

```json
{"n":1,"stage":"completed","type":"llm_completion",
 "attrs":{"http.method":"POST","http.url":"https://api.anthropic.com/v1/messages","openbox.span_synthetic":true}}
```

Then unwrap the body — this is the shape core unmarshals, and anything else logs
and yields `""`:

```bash
sent 'select(.spans) | .spans[0].response_body' | jq -r 'fromjson | .choices[0].message.content'
```

**Expect** the sentence you sent.

> Those `http.*` attributes describe a request the client never made. They are the
> only input core's classifier accepts, and `openbox.span_synthetic` marks them as
> synthesized. **Do not "clean them up"** — removing them does not error, it
> silently stops the span being classified as an LLM call, and alignment dies with
> no signal. They retire with openbox-core#130.

**Proves:** the exact shape core demands is on the wire.
**Does not prove:** that core classifies it as expected — see L2.

### T4 — capture off ⇒ nothing new, but status survives ⭐

**The single most valuable check here.** One command validates the whole gate
design, and its failure mode is the one nobody would notice.

```bash
reset
export OPENBOX_CONTENT_CAPTURE=0
echo '{"hook_event_name":"Stop","session_id":"t4","cwd":"/tmp/obx-t18/proj","transcript_path":"/tmp/obx-t18/transcript.jsonl","last_assistant_message":"CANARY-REPLY-must-not-egress"}' | hook Stop
echo '{"hook_event_name":"PostToolUse","session_id":"t4","cwd":"/tmp/obx-t18/proj","tool_name":"Read","tool_use_id":"tu_4","tool_input":{"file_path":"/tmp/x"}}' | hook PostToolUse
echo '{"hook_event_name":"SessionEnd","session_id":"t4","cwd":"/tmp/obx-t18/proj","reason":"other"}' | hook SessionEnd

curl -s http://127.0.0.1:8098/ | grep -c -e CANARY-REPLY -e '"spans"' -e '"span_count"'
sent 'select(.event_type=="ActivityCompleted" and .activity_type=="Read") | .status'
```

**Expect `0`** from the first command — no text, no `spans`, no `span_count` — and
**`"completed"`** from the second.

**Proves:** the content gate governs the assistant text, and `status` is
structural rather than gated (so Tool Health does not depend on a privacy
setting).

### T5 — a secret in the reply is redacted before it is sent ⭐

**Why it matters:** with capture on this is the *only* in-transit control there
is. It must run before attachment, not after — a redaction applied to a copy would
satisfy every other check here and still leak.

```bash
reset
export OPENBOX_CONTENT_CAPTURE=1
unset OPENBOX_SECRET_DETECTION            # default ON
echo '{"hook_event_name":"Stop","session_id":"t5","cwd":"/tmp/obx-t18/proj","transcript_path":"/tmp/obx-t18/transcript.jsonl","last_assistant_message":"your key is AWS_ACCESS_KEY_ID=${OPENBOX_REDACTED_AWS_KEY}"}' | hook Stop
echo '{"hook_event_name":"SessionEnd","session_id":"t5","cwd":"/tmp/obx-t18/proj","reason":"other"}' | hook SessionEnd

curl -s http://127.0.0.1:8098/ | grep -c ${OPENBOX_REDACTED_AWS_KEY}   # expect 0
curl -s http://127.0.0.1:8098/ | grep -c OPENBOX_REDACTED       # expect >=1
```

`${OPENBOX_REDACTED_AWS_KEY}` is AWS's own documentation placeholder — never paste a live
credential to test masking.

**Does not prove:** that redaction catches *your* secret formats. It is
pattern- and entropy-based, and `secret_detection: false` disables it entirely, at
which point replies egress unredacted.

### T6 — the lifecycle signals

```bash
reset
echo '{"hook_event_name":"SubagentStart","session_id":"t6","cwd":"/tmp/obx-t18/proj","agent_id":"agt-1","agent_type":"code-reviewer"}' | hook SubagentStart
echo '{"hook_event_name":"PermissionDenied","session_id":"t6","cwd":"/tmp/obx-t18/proj","tool_name":"Bash","tool_use_id":"tu_6","tool_input":{"command":"rm -rf /"},"reason":"DENIAL-REASON-must-not-egress"}' | hook PermissionDenied
echo '{"hook_event_name":"StopFailure","session_id":"t6","cwd":"/tmp/obx-t18/proj","error":"rate_limit","error_details":"DENIAL-REASON-must-not-egress"}' | hook StopFailure
echo '{"hook_event_name":"SessionEnd","session_id":"t6","cwd":"/tmp/obx-t18/proj","reason":"other"}' | hook SessionEnd

sent 'select(.signal_name) | {signal_name, has_args: (has("signal_args"))}'
curl -s http://127.0.0.1:8098/ | grep -c DENIAL-REASON      # expect 0
```

**Expect** `subagent_started`, `permission_denied`, `api_error` — each with
`has_args: false` — and no free text.

> `has_args: false` is not cosmetic. Core reads **any** `SignalReceived` carrying
> `signal_args` as a new user goal and overwrites the alignment session's goal with
> it. Putting the denied tool name there would replace the developer's prompt as
> the thing every later turn is scored against.

Note the provider limit: Claude Code fires `PermissionDenied` only after an
**auto-mode classifier** denial. A static `permissions.deny` rule denies without
firing it, so absence of these events is not evidence that nothing was denied.

### T7 — a re-reported turn keeps its span id

The turn cursor deliberately re-reads a window after a crash — over-report into a
server that deduplicates rather than lose a turn. That is only safe if the id is
stable.

```bash
reset
export OPENBOX_CONTENT_CAPTURE=1
for i in 1 2; do
  rm -rf /tmp/obx-t18/spool/turns        # simulate the cursor never advancing
  echo '{"hook_event_name":"Stop","session_id":"t7","cwd":"/tmp/obx-t18/proj","transcript_path":"/tmp/obx-t18/transcript.jsonl","last_assistant_message":"same turn, reported twice"}' | hook Stop
done
echo '{"hook_event_name":"SessionEnd","session_id":"t7","cwd":"/tmp/obx-t18/proj","reason":"other"}' | hook SessionEnd

sent 'select(.spans) | .spans[0].span_id' | sort -u | wc -l | tr -d ' '
```

**Expect `1`** — both reports derive the same id, so core's `(span_id, stage)`
dedupe absorbs the second instead of storing the reply twice.

### T8 — finops off removes the turn entirely

```bash
reset
export OPENBOX_FINOPS=0 OPENBOX_CONTENT_CAPTURE=1
echo '{"hook_event_name":"Stop","session_id":"t8","cwd":"/tmp/obx-t18/proj","transcript_path":"/tmp/obx-t18/transcript.jsonl","last_assistant_message":"CANARY-REPLY-must-not-egress"}' | hook Stop
echo '{"hook_event_name":"SessionEnd","session_id":"t8","cwd":"/tmp/obx-t18/proj","reason":"other"}' | hook SessionEnd

curl -s http://127.0.0.1:8098/ | grep -c -e llm_completion -e CANARY-REPLY
```

**Expect `0`.** The reply rides the turn event, and turn events exist only under
usage capture — so `finops: false` removes both. Worth knowing before someone
turns off finops for cost reasons and wonders why alignment went quiet.

```bash
export OPENBOX_FINOPS=1   # restore
```

### T9 — the automated gates

The checks above are about behaviour; these are the ones CI enforces.

```bash
cd <repo>
for m in client adapters/claude-code adapters/codex contracts/dev-event/conformance decision; do (cd $m && go test ./... -race) || echo "FAILED: $m"; done
(cd client && go test . -run Golden)                     # wire bytes unchanged
(cd adapters/claude-code && go test . -run "C2[0-6]" -v)  # the 7 conformance cases
```

---

## Live-stack checklist

**Not a record — a checklist.** Nothing below has been run.

### P0 — preconditions, blocking

Check these before concluding anything failed. Two of the three are outside this
repo and each produces "the feature is broken" as its symptom.

| # | Precondition | If missing |
|---|---|---|
| 1 | **`LlamaFirewallHost` is set** in core's config | `performTraceCheck` returns nil (`llama_firewall.go:31-34`) and **both alignment widgets stay empty with a perfect client** |
| 2 | **Redis is up** | no goal session store; alignment silently no-ops |
| 3 | **A FRESH agent id** | an existing agent carries accumulated `tool.<name>.failed` from before `status` shipped, so SUCCESS% shows partial recovery, not 100% — and reads as a broken fix |
| 4 | Content capture and finops both ON (defaults) | the span is absent by design; see T4/T8 |

### The checks

Widgets are agent-scoped GETs — no UI needed.

| # | Check | How | If empty |
|---|---|---|---|
| L1 ⭐ | Tool Health success | `GET /agent/{id}/observability` → `tools[].success_calls > 0` | is `workflow_status` set on the stored row? Set ⇒ core-side metric path; unset ⇒ client (re-check T1) |
| L2 ⭐ | The span stored | `select span_type, stage from spans where session_id=…` ⇒ one `llm_completion`/`completed` per turn | row present but `span_type` wrong ⇒ the synthesized attributes stopped satisfying `isLLMCall`; row absent ⇒ capture off, or the client sent none (T3) |
| L3 ⭐ | Alignment | `GET /agent/{id}/goal-alignment/trend` → non-empty | span row exists but trend empty ⇒ P0.1/P0.2, or `prompt_submitted` carried no `signal_args` (capture off ⇒ no goal to score against) |
| L4 | Drift | a row in `age_evaluations` with `span_id IS NULL` | expected unless LlamaFirewall actually reported misalignment — **a drift verdict is not producible on demand**, so the honest criterion is that an evaluation happened |
| L5 | No regression | the tools widget's span CTE selects `span_type='mcp_tool_call'` only | if `llm_completion` appears there, the CTE needs scoping — a core-side fix |
| L6 | Signals | `governance_events` rows for `subagent_started`, `permission_denied`, `api_error`, each with `signal_args` **NULL** | a non-null `signal_args` means the alignment goal is being overwritten by telemetry |

`MAPPING.md` §7 items 15–21 are the same list in the contract's own words; treat
that as the single source and this table as its operator-facing form.

---

## Cleanup

```bash
pkill -f obx-t18/stub.py
rm -rf /tmp/obx-t18
```

**The stub's captured bodies contain prompt and assistant text.** They are
content-bearing — the `rm -rf` above is the point, not housekeeping. Nothing
touched your real `~/.openbox` **provided you exported `OPENBOX_HOME` and
`OPENBOX_ENFORCEMENT_FILE` in every shell**; if you forgot, check
`<os-config-dir>/openbox/`.

Never `cat ~/.openbox/.env` as a debugging step. It is plaintext by design
and the habit is the problem.

---

## What this does not cover

| Not covered | Why | Needs |
|---|---|---|
| **The testbed** | has not been run for any of this | a live local stack |
| That core STORES any of it | the stub counts POSTs, not rows | core + its database |
| That the widgets render | the live section is a checklist, not a record | full stack + a fresh agent |
| **Codex** | no failure hook, no assistant-text field, per-session usage — so no tool status and no alignment feed, by design | a Codex install; and a provider change for the gaps |
| Subagent turns feeding alignment | depends on `last_assistant_message` being present on `SubagentStop`; the field is optional in the provider schema | a real session that spawns a subagent |
| `StopFailure` in a real session | not producible on demand — its payload shape is schema-verified only | a forced provider-side API error |
| The lost-200 double-store window | irreducible client-side | server-side dedupe on developer events |
| Server-side retention of the new content class | outside this repo | a backend decision |

The repo's own rule applies: unit tests are not evidence that a hook works, and
this guide is not evidence that a stack ingests it.
