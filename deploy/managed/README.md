# Managed provider configuration

Reference configuration that makes OpenBox governance an **org mandate** rather than
a per-developer opt-in. These are the files an MDM/config-management system deploys;
`openbox managed install` writes them for a single machine.

Everything here is the *provider's* mechanism, not OpenBox's. We ship the payload;
distributing it is your fleet-management plane's job.

## Why this matters more than it looks

Without managed configuration, OpenBox enforcement is a hook in the developer's own
config file. It prevents mistakes, and it is genuinely useful for that. It does not
withstand someone who does not want it: they can delete the hook, edit `dev.json`,
or start the tool with bypass flags. Any enterprise claim of the form "our coding
agents are governed" rests on the files in this directory, because they are what
stop a local edit from removing the gate.

That is also why `posture.provider_managed` exists (E8-S5): the control plane can
see which sessions actually ran under a mandate instead of taking it on faith.

## What each file guarantees — and does not

### Claude Code — `claude-code/managed-settings.json`

| Setting | Guarantee |
|---|---|
| `hooks` | OpenBox hooks are defined by the admin, not the user. |
| `allowManagedHooksOnly: true` | User- and project-level hooks are ignored entirely. Without this, a user hook config coexists with yours. |
| `allowManagedPermissionRulesOnly: true` | Permission rules come only from managed settings. |
| `disableBypassPermissionsMode: "disable"` | Removes the "skip all permission checks" escape. |
| `disableSideloadFlags: true` | Blocks CLI flags that would sideload alternative settings. |
| `strictPluginOnlyCustomization: true` | Customization is limited to approved plugins. |
| `sandbox.failIfUnavailable: true` | The tool refuses to run unsandboxed rather than silently degrading. |
| `sandbox.allowUnsandboxedCommands: false` | Makes `dangerouslyDisableSandbox` a no-op. |
| `permissions.deny` | Belt-and-braces deny rules that do not depend on OpenBox policy being fresh. |

**Not a guarantee.** `minimumVersion` / `requiredMinimumVersion` **fail open by
design** upstream — an invalid or unmet value is stripped rather than enforced. Treat
version floors as hygiene, never as a control. Deploy this file with filesystem
permissions that make it root-owned and user-unwritable; a managed settings file the
user can edit is not managed.

Target paths (deploy read-only, root-owned):

- Linux — `/etc/claude-code/managed-settings.json`
- macOS — `/Library/Application Support/ClaudeCode/managed-settings.json`
- Windows — `C:\ProgramData\ClaudeCode\managed-settings.json`

Orgs on a plan with **server-managed settings** should prefer that channel: it
refreshes hourly and, with `forceRemoteSettingsRefresh: true`, the CLI exits rather
than starting without policy — a stronger property than any local file, because it
fails closed at startup.

### Codex — `codex/requirements.toml` and `codex/managed_config.toml`

| Setting | Guarantee |
|---|---|
| `[hooks]` in requirements | The OpenBox hook is mandated by the admin. |
| `allow_managed_hooks_only = true` | User hook config is ignored. |
| `allowed_approval_policies` | Pins which approval modes are selectable — the important one, since `never` would let tool calls auto-run. |
| `allowed_sandbox_modes` | Pins sandboxing so a session cannot opt out. |
| `[features] hooks = true` | Hooks cannot be feature-flagged off locally. |

Target paths:

- Linux/macOS — `/etc/codex/requirements.toml`, `/etc/codex/managed_config.toml`
- macOS MDM — the `com.openai.codex` preference domain, base64-encoded TOML
  (Jamf/Fleet/Kandji); see `codex/README-mdm.md`
- Cloud-managed requirement bundles are the stronger channel where available, for
  the same reason as Claude Code's server-managed settings.

### Cursor

Not shipped: the SL-8 adapter does not exist yet. Cursor gained a hook surface in
v3.11 (2026-07-10), so this becomes a real template when that adapter lands.

## Install

```bash
# Preview exactly what would be written, and where. Never needs privileges.
openbox managed install --provider claude-code,codex --dry-run

# Apply (needs root/Administrator for the system paths).
sudo openbox managed install --provider claude-code,codex
```

The installer is deliberately conservative:

- **Idempotent** — re-running with the same templates changes nothing.
- **Backs up** — an existing file is copied to `<name>.openbox-backup-<timestamp>`
  before being replaced.
- **Refuses to weaken** — if the file already present has a stricter setting than the
  template (managed-hooks-only already on, sandbox already required), the install
  aborts rather than relaxing it. Overriding that is a deliberate `--force`.
- **Unprivileged is not a failure** — without permission to write, it prints the exact
  paths and contents for the MDM team and exits 0.

## Verify

```bash
openbox doctor      # effective posture, and whether provider config is managed
```

`openbox doctor` reports `provider_managed` per provider, which is the same value the
session posture carries — so what you see locally is what the control plane sees.
