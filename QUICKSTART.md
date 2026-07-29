# Quickstart — OpenBox for Claude Code in 3 steps

Governance for your Claude Code sessions, ambient after one command. No daemon to
run, no environment variables to keep set.

## 1. Install the engine

```bash
curl -fsSL https://raw.githubusercontent.com/OpenBox-AI/openbox-shift-left/main/install.sh | bash
```

Downloads the prebuilt `openbox` binary for your platform (linux/macOS,
amd64/arm64), verifies its checksum, and drops it on your PATH (`~/.local/bin` by
default). No Go toolchain required. (If no prebuilt release matches your platform,
it transparently falls back to building from source, which then needs Go 1.23+.)

## 2. Onboard

```bash
export OPENBOX_CONTROL_TOKEN=<keycloak-jwt-or-obx_key_…>   # your org credential; never a flag (INV-1)
openbox dev init --provider claude-code --backend-url https://<your-openbox-backend> --enforce
```

That single command:

- registers a `developer` agent and stores its `obx_` key + signing seed in your OS
  keychain (never printed, never in a config file — INV-1);
- installs the Claude Code plugin into `~/.claude/plugins/openbox-observe` and copies
  the engine into it, so every `claude` session is governed;
- pulls your org policy into a local bundle;
- persists your enforce posture into `~/.config/openbox/dev.json`.

Drop `--enforce` for **observe-only** (telemetry + lineage, never blocks). With
`--enforce` the session also **blocks/asks/redacts** per your org policy — evaluated
**in-process** by the hook (ADR-0006), so there is nothing extra to start.

`OPENBOX_CONTROL_TOKEN` and the backend URL are needed **only here, at onboarding**.
Preview without touching anything: add `--dry-run`.

## 3. Use Claude Code — that's it

```bash
claude
```

Governance is ambient. Every session emits normalized telemetry to OpenBox, commits
are stamped for lineage, and (with `--enforce`) risky tool calls are gated locally.
Confirm the wiring any time:

```bash
openbox dev verify        # → ✓ verified: did:aip:… @ https://…
```

---

### No moving parts

After `dev init` there is nothing to keep running or exporting:

- **No daemon** — enforcement is evaluated in-process by the hook (ADR-0006).
- **No runtime env vars** — your posture lives in `~/.config/openbox/dev.json`.
- **No manual policy sync** — `dev init` pulls the policy; each session re-checks staleness.

### Turning enforcement off / on later

Edit `~/.config/openbox/dev.json` (`"enforce": false`) or re-run `openbox dev init
--provider claude-code` (without `--enforce` = observe). No env var required either
way; `OPENBOX_ENFORCE` still works as a one-session override if you want it.

### Privacy

Content capture is **ON by default** (as of 2026-07-15): prompt text is egressed so
governance can act on it. Opt out with `content_capture:false` in `dev.json` (or
`OPENBOX_CONTENT_CAPTURE=0`) for metadata-only. Tool commands, file bodies, and tool
output are **never** egressed on observe events regardless. See the README privacy
note.

### What this setup does and doesn't guarantee

Enforcement here is a hook in **your own** config, so it prevents mistakes but does not
withstand someone deliberately removing it — org-wide assurance needs the provider's
managed configuration (`allowManagedHooksOnly` / `allow_managed_hooks_only`). Likewise the
`OpenBox-Session` commit trailer is an *inferred* claim about which session was live, not
proof that the session produced the diff. The README's **Assurance** section spells out
each guarantee and its current limit.
