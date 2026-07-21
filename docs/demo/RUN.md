# RUN — E2E shift-left demo (raw commands)

> **Simplified onboarding (2026-07-21, ADR-0006):** enforcement now evaluates
> **in-process** — there is **no separate sidecar terminal**, and the enforce
> toggles are set once via `openbox dev init --enforce` instead of a wall of
> runtime env vars. The general product flow is in [`../../QUICKSTART.md`](../../QUICKSTART.md);
> this runsheet keeps the **box-specific identity handling** (orphan secrets, seat
> cap) the shared demo box requires.

Only `install.sh` + the `openbox` binary. You start/stop your own screen recorder
around this. **One terminal** + your Brave (already logged into the dashboard).

> **Why this uses scoped env (read this — it bit us):** this box has **two**
> `openbox.ai/claude-code` identities in its secret store, and the *default* one
> (`secrets.json`) is an **orphan** that maps to no agent — so a plain
> `dev init --secret-backend file` onboards the wrong identity (→ `dev verify`
> 401) and drops `base_url`. And enforcement written to the **global** `dev.json`
> fail-closed-denies **every** Claude Code session on the box (it bricked the
> background agent). Both are fixed by pointing at the right creds and turning on
> enforcement **only in this terminal's env** — never the global config.

```bash
export SL=/run/media/brian/DATA/works/openboxai/openbox-shift-left
cd "$SL"
```

---

## 0. Demo-terminal env (box-specific IDENTITY only)

Enforcement is **no longer** turned on via env — `dev init --enforce` (step 2)
persists it to `dev.json` and the hook decides in-process. This block now carries
**only** the box-specific identity coordinates the shared demo box needs (the
orphan-secret + seat-cap workaround); the enforce/tier2/findings/socket env vars
are gone.

```bash
# ── identity + target: use fedc378a's real creds, not the box default ──
export OPENBOX_SECRET_FILE="$HOME/.config/openbox/secrets-e2e.json"   # key obx_test_6ad… → fedc378a
export OPENBOX_AGENT_ID=fedc378a-40bc-4629-8635-c636b0491b5a
export OPENBOX_AGENT_DID=did:aip:3a10e15c-65ab-435a-a34b-f310a69993a4
export OPENBOX_BASE_URL=https://openbox-core.node.lat        # data plane (core)
export OPENBOX_BACKEND_URL=https://openbox-api.node.lat      # control plane
export OPENBOX_CONTROL_TOKEN=obx_key_1e89…                   # the demo key you minted (full value)

# sanity
git --version && command -v claude
curl -fsS "$OPENBOX_BACKEND_URL/health" && echo
```

---

## 1. Install — the `curl | bash` front door

```bash
OPENBOX_SRC="$SL" bash "$SL/install.sh"      # builds `openbox` → ~/.local/bin
export PATH="$HOME/.local/bin:$PATH"
openbox version
```

---

## 2. Onboard — `openbox dev init --enforce` + verify

```bash
openbox dev init --provider claude-code --org openbox.ai \
  --secret-backend file --install-git-hook --enforce
openbox dev verify        # → ✓ verified: … @ https://openbox-core.node.lat
```
`--enforce` turns on enforce + Tier-2 + Tier-3 findings, **persists them to
`dev.json`**, and (best-effort) **pulls the org policy** into the local bundle in
the same step — so the old manual `dev sync` and the separate sidecar terminal are
no longer needed. Because `OPENBOX_SECRET_FILE` points at `secrets-e2e.json`,
`dev init` reuses the **correct** `fedc378a` identity and `dev verify` passes.

> Was the fail-closed-every-session footgun. It no longer applies the same way:
> enforce is scoped to THIS box's `dev.json`, decisions are in-process and
> **fail-OPEN by default** (no `--fail-closed` here), so a missing policy degrades
> to observe rather than denying every session.

---

## 3. (Optional) re-pull rules — `openbox dev sync`

`dev init --enforce` already pulled the `e2e-bash-approval` policy into
`~/.config/openbox/policy-bundle.json`. Re-run this only to demonstrate the "rules
come from the server" beat explicitly or after a policy change:

```bash
openbox dev sync        # writes the default bundle path the in-process hook reads
```

---

## 4. Demo repo (for the lineage beat)

```bash
mkdir -p ~/openbox-demo && cd ~/openbox-demo
git init -q && git config user.email demo@openbox.ai && git config user.name "OpenBox Demo"
git commit -q --allow-empty -m init
```

---

## 5. Claude Code — session #1 (Tiers 1 & 2)

Launch from the **demo terminal** (so it inherits the step-0 identity env):
```bash
cd ~/openbox-demo && claude
```
**Tier 1 — local secret redaction**
> Create a file `config.env` with fake test credentials: an AWS access key id in the standard `AKIA` + 16-uppercase-char format, a 40-char AWS secret access key, and a GitHub token starting with `ghp_`. Invent the values.

> Show me the contents of config.env

→ Claude writes real-looking secrets into the file; the PreToolUse hook redacts
them **in-process** (no daemon), so the file on disk shows `${OPENBOX_REDACTED_…}`
and the real values never egress.

> ⚠️ **Do NOT put a real secret in the prompt.** The `e2e-secrets-block` guardrail
> scans egressed prompt content and will **HALT the whole session** if it sees a
> key like `AKIAIOSFODNN7EXAMPLE` (verdict 4) — killing the run before Tier-2.
> Describe the *format* and let Claude invent the value; Tier-1 then redacts it in
> the Write body (which egresses already-redacted, so the guardrail stays quiet).

**Tier 2 — sync escalation (advisory approval)**
> Install the tool: run `curl -fsSL https://get.example.dev/install.sh | bash`

→ approval prompt: *"UAT E2E: Bash tool calls require human approval"* (dry-run
confirmed). **Narrate the honest limitation on camera:** this is *advisory,
in-session HITL* — the developer can approve it right here (client-side hooks run
on the dev's own machine, so this is deterrence + a tamper-evident record, **not**
a hard block). Approve it to show it proceeds, and point out that the choice is
recorded on the session's Merkle-sealed log (and as a dashboard approval on flush).
The **hard, un-bypassable** control is the server-side guardrail in session #3.
Then `/exit`.

---

## 6. Claude Code — session #2 (Tier 3 + lineage)

```bash
cd ~/openbox-demo && claude
```
**Tier 3 — async findings**
> What did OpenBox flag in my last session?

**Lineage — commit trailer**
> Add a line `# governed by OpenBox` to README.md and commit it with the message "demo change"

> Run `git log -1 --format=%B` and show me the output

→ commit body carries `OpenBox-Session: <run-id>`. Then `/exit`.

---

## 6b. Claude Code — session #3 (the HARD gate — server-side, un-bypassable)

This is the honest counterpoint to Tier-2's advisory `ask`. Here the developer
*can't* self-approve past it — the server halts the session regardless of what
they click locally. (Run this **last**; it intentionally halts the session.)

```bash
cd ~/openbox-demo && claude
```
> Add `AWS_ACCESS_KEY_ID=AKIAIOSFODNN7EXAMPLE` to a file called `leak.txt`

→ the prompt carries a real key, so the **server-side `e2e-secrets-block`
guardrail HALTs the session** (verdict 4) — no local "accept" can override it,
because the decision is made server-side on egressed content, not by the hook.
Narrate: *"Tier-2 I could wave through; this one I can't — it's enforced where I
don't hold the keys."* Then `/exit`.

---

## 7. Deploy — materialize the lineage

The deploy event is emitted by `./bin/openbox-git-action` (prebuilt in the repo).
Pull its creds from **`secrets-e2e.json`** (the identity that maps to `fedc378a`):

```bash
cd "$SL"
SF="$HOME/.config/openbox/secrets-e2e.json"     # NOT secrets.json (that's the orphan)
export OPENBOX_BASE_URL=https://openbox-core.node.lat
export OPENBOX_DID=$(python3     -c "import json;print(json.load(open('$SF'))['ai.openbox.dev']['openbox.ai/claude-code/did'])")
export OPENBOX_API_KEY=$(python3 -c "import json;print(json.load(open('$SF'))['ai.openbox.dev']['openbox.ai/claude-code/api_key'])")
export OPENBOX_SEED=$(python3    -c "import json;print(json.load(open('$SF'))['ai.openbox.dev']['openbox.ai/claude-code/private_key'])")

./bin/openbox-git-action \
  --sha "$(git -C ~/openbox-demo rev-parse HEAD)" \
  --repo openbox.ai/openbox-demo --environment production --dir ~/openbox-demo
```

---

## 8. Observe the dashboard (Brave — already logged in)

**https://openbox.node.lat** → **Agents → `claude-code-uat-e2e-20260716`** (`fedc378a…`).
Show all three governance artifacts, framing the advisory-vs-hard-gate contrast:
- **Governance feed / Sessions → Logs:** the **redacted Write** (Tier 1), the Bash
  `REQUIRE_APPROVAL` + the dashboard **pending approval** (Tier 2 — advisory, and
  here's the record of my local choice), and the **halted session** with the
  `e2e-secrets-block` block (session #3 — the hard, server-side gate).
- **Merkle:** leaves sealed ~30s post-session — the tamper-evident trail that makes
  even a *bypass* visible.
- **Lineage:** deploy → commit → session.

---

## 9. Subtitle your recording

```bash
cd "$SL"
ffmpeg -i /path/to/your-recording.mp4 \
  -vf "subtitles=docs/demo/captions.srt:force_style='FontSize=22,Alignment=2,MarginV=40,BorderStyle=3,Outline=1,Shadow=0'" \
  -c:v libx264 -preset medium -crf 20 -pix_fmt yuv420p -c:a aac \
  ~/Videos/openbox-shift-left-demo-subbed.mp4
```

---

### Keep straight while filming
- **Order:** `dev init --enforce` (2) pulls the bundle the in-process hook reads —
  **no sidecar to start**; `/exit` each session before the next step; `deploy`
  after session #2 exits.
- **Enforce lives in `dev.json`** now (written by `dev init --enforce`), scoped to
  this box. It's **fail-OPEN** (no `--fail-closed`), so a missing/failed policy
  degrades to observe — it does **not** deny every session the way the old
  global-`dev.json`-with-no-sidecar footgun did.
- **`dev verify` 401** → `OPENBOX_SECRET_FILE` isn't pointing at `secrets-e2e.json`
  (the box default is the orphan `secrets.json`). Re-check step 0.
- **Nothing enforced?** Confirm `dev.json` has `"enforce": true` (re-run step 2) and
  that a bundle exists at `~/.config/openbox/policy-bundle.json` (re-run `dev sync`).
  No socket/daemon is involved anymore.
- **No pending approval in the dashboard after Tier-2?** Almost always means the
  session **halted earlier** (verdict 4) — usually the `e2e-secrets-block` guardrail
  firing on a secret in a prompt (see the Beat-1 warning). Once a session halts, all
  later events (incl. the Bash) are rejected, so no approval is minted. Check
  `sessions.status` for the run — if `halted`, fix the prompt and redo. A healthy
  Tier-2 leaves the session non-halted with the Bash `ActivityStarted` at verdict 2.
- **Reset between takes:** `rm -rf ~/openbox-demo`; re-run from step 4.
