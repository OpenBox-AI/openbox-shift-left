# Demo — E2E Shift-Left + Lineage (real install → Claude Code → UAT)

The whole developer journey, nothing rigged: install OpenBox via `install.sh`,
onboard with a single `openbox dev init --provider claude-code --enforce` (which
also pulls the org rules), then run `claude` — all three enforcement tiers fire
**in-process** from the synced backend rules (no separate sidecar daemon; ADR-0006),
and a commit flows through to **commit → session → deploy lineage** in the UAT
dashboard.

> **To run it, follow [`RUN.md`](./RUN.md)** — the raw copy-paste runsheet
> (`install.sh` + the `openbox` binary only; you record the screen with your own
> tool and subtitle with the ffmpeg one-liner in RUN.md step 10). This file is
> background: what it proves, the verified environment, and the tier mechanics.

**Target:** deployed UAT (`*.node.lat`) — data lands in the dashboard natively.
**You run locally:** Claude Code only (enforcement is in-process in the hook;
core/backend are deployed UAT).

---

## What's already provisioned in UAT (verified 2026-07-20)

Onboarding **reuses** this developer's agent (UAT enforces an agent seat cap, so a
fresh create would 402 — reuse via the file secret store is correct and necessary):

| Piece | State |
|---|---|
| Agent | `fedc378a…` (`claude-code-uat-e2e-20260716`, org `openbox.ai`, `signing_required=false`) |
| Local creds | file store `~/.config/openbox/secrets.json` → `ai.openbox.dev / openbox.ai/claude-code/*` → `dev init` reuses it |
| Tier-1 / Tier-3 | guardrail `e2e-secrets-block` (Secrets Detection) active on the agent |
| Tier-2 policy | `e2e-bash-approval` — `REQUIRE_APPROVAL` when `activity_type contains "Bash"` (version `ac860a5`, active) |
| Lineage substrate | `deploy_session_links` table live in the UAT DB (migration `122`) |

---

## The three enforcement tiers (each a distinct on-camera beat)

Wired in `adapters/claude-code/hookrun.go` (`RunHook`).

| Tier | Trigger | Rule source | Observable |
|---|---|---|---|
| **1 · local secret redaction** | a `Write`/`Edit` with a secret in the body | local detector (`decision/secrets.go`), in-process, always on under enforce | written file shows `${OPENBOX_REDACTED_…}`; secret never egresses |
| **2 · sync `/evaluate` escalation** | a high-risk `Bash`/MCP call | synced `e2e-bash-approval` policy — resolves server-side | **advisory** approval prompt (`ask`) — see the honest-limit note below |
| **3 · async findings** | a prior session flushed non-ALLOW advisories | backend guardrail eval → `advisories.jsonl` | content-free "OpenBox governance — N finding(s)…" back in chat |

Tier-3 surfaces on the **next** prompt after a flush → the scenario uses multiple
short sessions (RUN.md steps 6, 7, 7b).

### Advisory vs. hard gate — the honest framing (demo accepts this, RUN.md §7b)
Client-side hooks run on the **developer's own machine**, so anything the hook
decides (`ask`/`deny`) is **bypassable** by the developer (accept the `ask`, edit
`settings.json`, or `claude --dangerously-skip-permissions`). So Tier-2's `ask` is
**deterrence + a tamper-evident record**, *not* a hard block — the demo narrates
that plainly. The **un-bypassable** control is **server-side**: the
`e2e-secrets-block` guardrail HALTs a session on egressed content **regardless of
any local accept** (RUN.md §7b, session #3), and the CI/deploy lineage gate runs
where the developer holds no keys. This mirrors the SDK's async supervisor-approval
flow that OD-HITL deliberately swapped for a synchronous local prompt on the dev
hot path (`enforce.go:570-576`); revisiting that is an open ADR (option in the
prior analysis).

### Why Tier-2 is a genuine escalation (not a local short-circuit)
`dev sync` localizes the policy, but the local decision input has **no
`activity_type` field** (it exposes `event_type`, span `name`/`semantic_type`,
`command`, `file_path`) — so the rule can't match locally → the base decision
stays *allow* → because `Bash` is high-risk, the hook **escalates synchronously to
`/evaluate`**, where `activity_type` resolves and the server returns
`REQUIRE_APPROVAL`.

> **Dry-run confirmed (2026-07-20):** a synthetic Bash `PreToolUse` through the
> real enforce+Tier-2 path returned, in ~1s:
> `permissionDecision:"ask"` · `verdict=REQUIRE_APPROVAL` · `source=tier2:evaluate`
> · `fail_open=false`, reason *"UAT E2E: Bash tool calls require human approval"*.
> The take shows a clean **approval prompt** — the pre-tool sync did **not**
> fail-open. The escalation also persisted a `governance_event` (verdict 2), so the
> Bash shows in the dashboard. (Remember: this `ask` is advisory — see above.)

---

## Dashboard (UAT)

**https://openbox.node.lat**, org `openbox.ai`, agent `claude-code-uat-e2e-20260716`:
**Authorize / Governance feed** (gated Bash verdict + redacted Write) → **Sessions →
Logs** (per-event verdicts) → **Merkle** (leaves sealed ~30s post-session) →
**Lineage** (deploy traced to commit + session).

---

## Troubleshooting

- **`dev verify` 401 "identity rejected".** This box has TWO `openbox.ai/claude-code`
  identities; the default store `secrets.json` is an **orphan** (maps to no agent).
  Point at the real one: `export OPENBOX_SECRET_FILE=~/.config/openbox/secrets-e2e.json`
  (RUN.md step 0). `dev init` also drops `base_url`, so `OPENBOX_BASE_URL` is set in env too.
- **Every Claude Code session gets "request denied … fail-closed".** Only happens if
  you opted into fail-closed (`fail_closed:true` / `OPENBOX_FAIL_CLOSED=1`) AND no
  policy bundle is present. The demo uses the **default fail-OPEN** posture, so a
  missing bundle degrades to observe. To recover: clear `fail_closed` from `dev.json`
  or re-run `openbox dev sync`.
- **`dev init` tries to create + 402s.** Use `--secret-backend file` + `OPENBOX_SECRET_FILE`
  so it reuses the agent instead of hitting the seat cap.
- **Nothing enforced.** `dev.json` is missing `"enforce": true` (re-run `dev init --enforce`),
  or no bundle at `~/.config/openbox/policy-bundle.json` (re-run `dev sync`). No daemon
  is involved — enforcement is in-process.
- **`dev sync` 403.** The control token lacks `read:agent_policy`.
- **Tier-3 empty.** Needs `findings:true` and advisory records newer than the
  cursor; re-run Tier-2 and end the session to produce fresh ones.
- **Deploy `status=inferred`.** Expected (ownership verify is opt-in); the lineage
  row still materializes. Always end the authoring session **before** deploy.
- **No socket/daemon exists** (ADR-0006). Enforcement is in-process; there is
  nothing to bind, start, or keep running.

## Files
| File | Purpose |
|---|---|
| `RUN.md` | the runsheet — raw `install.sh` + `openbox` commands, step by step |
| `README.md` | this background doc (verified env, tier mechanics, dashboard, troubleshooting) |
| `captions.srt` | pre-written subtitles timed to the RUN.md beats |
