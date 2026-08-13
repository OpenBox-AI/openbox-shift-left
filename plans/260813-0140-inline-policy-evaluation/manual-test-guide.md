# Manual test guide — inline policy evaluation, without a local stack

For a developer outside OpenBox who wants to check this change behaves as
documented. Every command here was executed while writing it; the outputs shown
are real.

**The idea:** you do not need OpenBox's seven-container stack to test the
*client*. You need something that answers `POST /api/v1/governance/evaluate`.
That is ~15 lines of Python, and it gets you most of the way — you can drive real
hooks, see real payloads, and check real verdicts.

**What this cannot tell you:** whether OpenBox's actual policy engine agrees with
the stub, and in particular whether a **raw-rego org policy** is now enforced.
That is ADR-0017's headline claim and it needs the real backend. Everything else
below is genuinely checkable.

---

## Setup (5 minutes)

### 1. Build

```bash
go build -o /tmp/obx-test/openbox ./cli/cmd/openbox
```

### 2. Fake credentials in an isolated home

`openbox auth` registers against the real control plane, which you do not have.
Hand-write what it would have written instead. **`OPENBOX_HOME` keeps all of this
out of your real `~/.openbox`** — set it in every shell you use below.

```bash
export OPENBOX_HOME=/tmp/obx-test/home
mkdir -p "$OPENBOX_HOME" && chmod 700 "$OPENBOX_HOME"

cat > "$OPENBOX_HOME/.env" <<'EOF'
OPENBOX_API_KEY=${OPENBOX_REDACTED_SECRET_ASSIGNMENT}
OPENBOX_AGENT_PRIVATE_KEY=${OPENBOX_REDACTED_ENTROPY}=
EOF
chmod 600 "$OPENBOX_HOME/.env"

cat > "$OPENBOX_HOME/dev.json" <<'EOF'
{"developer_did":"did:aip:7f3c9b2e-0000-5000-a000-000000000001","base_url":"http://127.0.0.1:8099"}
EOF
```

The signing key is 32 zero bytes, base64. It is a *test* key: the client signs
with it and the stub does not check. `http://` is accepted because 127.0.0.1 is
loopback — the client refuses plaintext to any other host.

### 3. The stub control plane

```bash
cat > /tmp/obx-test/stub.py <<'PY'
import http.server, json, sys
VERDICT = json.loads(sys.argv[1]) if len(sys.argv) > 1 else {"verdict": "allow"}
BODIES = []
class H(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        BODIES.append(self.rfile.read(int(self.headers.get("Content-Length", 0))).decode())
        b = json.dumps(VERDICT).encode()
        self.send_response(200); self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(b))); self.end_headers(); self.wfile.write(b)
    def do_GET(self):          # GET / replays everything it received
        b = json.dumps(BODIES).encode()
        self.send_response(200); self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(b))); self.end_headers(); self.wfile.write(b)
    def log_message(self, *a): pass
http.server.HTTPServer(("127.0.0.1", 8099), H).serve_forever()
PY
```

Start it with whatever verdict you want it to give:

```bash
python3 /tmp/obx-test/stub.py '{"verdict":"allow"}' &
```

Restart it to change the verdict (`pkill -f obx-test/stub.py` first).

### 4. Govern a scratch project

```bash
mkdir -p /tmp/obx-test/proj && cd /tmp/obx-test/proj
/tmp/obx-test/openbox init --provider claude-code
```

Expect it to say **`mode: ENFORCE`** and **`Governed: THIS PROJECT ONLY`**. Both
are ADR-0016 defaults and worth confirming you saw.

Finally, keep the audit out of your real config dir:

```bash
export OPENBOX_ENFORCEMENT_FILE=/tmp/obx-test/enf.jsonl
```

---

## The tests

Each one invokes the hook exactly the way Claude Code does: the subcommand, with
the hook payload on stdin.

### T1 — a Write is decided by the server ⭐

**Why it matters:** before this change, a `Write` was decided by a local
evaluator and never reached the server at all. This is the behavioural heart of
ADR-0017, and it is the test that catches a regression to a risk-selected subset.

Restart the stub with a deny:

```bash
pkill -f obx-test/stub.py; sleep 1
python3 /tmp/obx-test/stub.py '{"verdict":"block","reason":"manual test deny","policy_id":"manual-pol"}' &
```

```bash
echo '{"hook_event_name":"PreToolUse","session_id":"t1","cwd":"/tmp/obx-test/proj","tool_name":"Write","tool_input":{"file_path":"/tmp/obx-test/proj/a.txt","content":"hello"}}' \
  | /tmp/obx-test/openbox hook claude-code PreToolUse
```

**Expect** — a deny carrying the *server's* reason and policy id:

```json
{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"OpenBox governance: manual test deny (policy: manual-pol)"}}
```

and on stderr `source=evaluate fail_open=false`. **`source=evaluate` is the
assertion** — it means the control plane decided, not anything local.

Try the same with `"tool_name":"Bash"` and `"tool_name":"Read"`. All classes go
the same way now.

### T2 — a secret is redacted BEFORE it is sent ⭐

**Why it matters:** file bodies egress now, which they did not before. Local
redaction is the only thing standing between a secret in a file and the control
plane's event storage. This asserts on the actual bytes, which is the only form
of this test that means anything — a correct redaction applied to the tool call
while the *original* body is sent would look fine everywhere else.

```bash
echo '{"hook_event_name":"PreToolUse","session_id":"t2","cwd":"/tmp/obx-test/proj","tool_name":"Write","tool_input":{"file_path":"/tmp/obx-test/proj/.env","content":"AWS_ACCESS_KEY_ID=${OPENBOX_REDACTED_AWS_KEY}"}}' \
  | /tmp/obx-test/openbox hook claude-code PreToolUse >/dev/null 2>&1

curl -s http://127.0.0.1:8099/ | python3 -c '
import json,sys
b = json.load(sys.stdin)
print("content on the wire:", json.loads(b[-1])["activity_input"].get("content"))
print("RAW SECRET PRESENT:", "${OPENBOX_REDACTED_AWS_KEY}" in json.dumps(b))'
```

**Expect:**

```
content on the wire: {"content":"AWS_ACCESS_KEY_ID=${OPENBOX_REDACTED_AWS_KEY}","file_path":"/tmp/obx-test/proj/.env"}
RAW SECRET PRESENT: False
```

`RAW SECRET PRESENT: True` is a **stop-everything** result.

### T3 — content capture is a hard gate

**Why it matters:** it is the one control an org has over the change in T2.

```bash
echo '{"hook_event_name":"PreToolUse","session_id":"t3","cwd":"/tmp/obx-test/proj","tool_name":"Write","tool_input":{"file_path":"/tmp/obx-test/proj/b.txt","content":"CANARY-abc123"}}' \
  | OPENBOX_CONTENT_CAPTURE=0 /tmp/obx-test/openbox hook claude-code PreToolUse >/dev/null 2>&1

curl -s http://127.0.0.1:8099/ | python3 -c '
import json,sys
b = json.load(sys.stdin); d = json.loads(b[-1])
print("canary present:", "CANARY-abc123" in b[-1])
print("structural axes still sent:", sorted(d["activity_input"].keys()))'
```

**Expect** `canary present: False`, and the structural axes (`file_path`,
`file_operation`, `kind`, `tool_name`) still present — no `content` key. That is
the honest trade: enforcement gets coarser, not broken.

### T4 — both outage branches ⭐

**Why it matters:** this is the trade ADR-0017 makes, and the limit the README
documents. Stop the stub first — an unreachable control plane is the whole point.

```bash
pkill -f obx-test/stub.py; sleep 1
export OPENBOX_ENFORCEMENT_FILE=/tmp/obx-test/enf-outage.jsonl
PAY='{"hook_event_name":"PreToolUse","session_id":"t4","cwd":"/tmp/obx-test/proj","tool_name":"Bash","tool_input":{"command":"echo hi"}}'

echo "--- default (fail-open) ---"
echo "$PAY" | /tmp/obx-test/openbox hook claude-code PreToolUse 2>/dev/null
echo "--- fail_closed ---"
echo "$PAY" | OPENBOX_FAIL_CLOSED=1 /tmp/obx-test/openbox hook claude-code PreToolUse 2>/dev/null
```

**Expect** empty output from the first (the call proceeds) and a deny from the
second, reason `…no governance decision could be obtained and this session is
fail-closed (evaluation undelivered)`.

Now the part that matters more than either verdict — **the ungoverned call must
still be recorded**, or the bypass is invisible:

```bash
python3 -c '
import json
for l in open("/tmp/obx-test/enf-outage.jsonl"):
    d = json.loads(l)
    print({k: d.get(k) for k in ("verdict","applied_decision","source","fail_open")})'
```

**Expect:**

```
{'verdict': '', 'applied_decision': None, 'source': 'evaluate:fail-open', 'fail_open': True}
{'verdict': 'HALT', 'applied_decision': 'deny', 'source': 'evaluate:fail-open', 'fail_open': True}
```

The first line is the fail-open call: it proceeded, and it is on the record as
ungoverned.

**Also check the timing.** With the stub down the hook returned in under a
second — connection-refused is immediate. A hung control plane is the slower
case, and the bound there is the provider's 30s hook ceiling.

### T5 — redaction survives the outage

**Why it matters:** it is the one control that depends on reaching nothing.

With the stub still down:

```bash
echo '{"hook_event_name":"PreToolUse","session_id":"t5","cwd":"/tmp/obx-test/proj","tool_name":"Write","tool_input":{"file_path":"/tmp/obx-test/proj/c.env","content":"AWS_ACCESS_KEY_ID=${OPENBOX_REDACTED_AWS_KEY}"}}' \
  | /tmp/obx-test/openbox hook claude-code PreToolUse 2>/dev/null
```

**Expect** an `updatedInput` carrying `${OPENBOX_REDACTED_AWS_KEY}` — the tool
call is still rewritten even though nothing could be reached.

### T6 — the retired command fails loudly

**Why it matters:** a pipeline still calling it must break visibly rather than
appear to work.

```bash
/tmp/obx-test/openbox dev sync; echo "exit=$?"
```

**Expect** exit **1** and a message naming ADR-0017 and saying the leftover
`policy-bundle.json` is inert.

### T7 — deprecated keys say so

```bash
OPENBOX_TIER2=0 /tmp/obx-test/openbox doctor 2>&1 >/dev/null | head -2
```

**Expect** ``openbox: `tier2` set but ignored — …``. Same on SessionStart. This
one is worth checking specifically: `tier2:false` used to *disable* enforcement,
so an org upgrading with it set is the population most at risk of being silently
governed when they think they are not.

### T8 — doctor tells the truth

```bash
/tmp/obx-test/openbox doctor
```

**Expect** a `Policy decisions` block — `decided by control_plane`, `if
unreachable fail_open`, and the warning that gated calls proceed. **Expect NO
"Policy bundle" section** and no `require_verified_bundle` in the flags: both
describe checks that no longer happen.

`last decision` reads the machine's real audit at
`<os-config-dir>/openbox/enforcements.jsonl` unless you set
`OPENBOX_ENFORCEMENT_FILE`. On a machine that ran the old build, old lines carry
the pre-rename `tier2:fail-open` source — that is history, not a bug.

---

## Optional: a real Claude Code session

Everything above drives the hook directly. To see it in a real session:

```bash
cd /tmp/obx-test/proj
python3 /tmp/obx-test/stub.py '{"verdict":"block","reason":"manual test deny","policy_id":"manual-pol"}' &
claude
```

Then ask it to write a file. Expect the model to report being blocked by
governance. Note that `OPENBOX_HOME` must be exported in that shell, or the hook
reads your real `~/.openbox`.

---

## Cleanup

```bash
pkill -f obx-test/stub.py
rm -rf /tmp/obx-test
```

Nothing touched your real `~/.openbox` or config dir **provided you exported
`OPENBOX_HOME` and `OPENBOX_ENFORCEMENT_FILE` in every shell**. If you forgot,
check `<os-config-dir>/openbox/enforcements.jsonl` for stray lines.

---

## What this does not cover

| Not covered | Why | Needs |
|---|---|---|
| **A raw-rego org policy is enforced** | the stub is not OPA; this is ADR-0017's headline claim | the real backend + OPA |
| One `ActivityStarted` stored per gated call | the stub counts requests, not stored rows | core + its database |
| The approval hold and rewake | needs a real approval record to poll | full stack |
| Codex | same shape, but no Codex session was driven | a Codex install |
| Windows | cross-compile only | a Windows machine |

The first row is the important one. If it matters to you, the testbed phase that
proves it is written and waiting: `testbed/30-enforce.sh` §A publishes a raw-rego
deny through the backend and asserts the call is blocked.
