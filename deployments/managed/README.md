# Managed provider configuration

Reference configuration that makes OpenBox governance an **org mandate** rather
than a per-developer opt-in. These are the files an MDM/config-management system
deploys. OpenBox ships them and does not install them: putting a root-owned
file on a fleet is your config-management plane's job, and a CLI that wrote it
for one machine was a worse version of that.

Everything here is the *provider's* mechanism, not OpenBox's. We ship the
payload; distributing it is your fleet-management plane's job.

## Why this matters more than it looks

Without managed configuration, OpenBox enforcement is a hook in the developer's
own config file. It prevents mistakes, and it is genuinely useful for that. It
does not withstand someone who does not want it: they can delete the hook, edit
`dev.json`, or start the tool with bypass flags. Any enterprise claim of the
form "our coding agents are governed" rests on the files in this directory,
because they are what stop a local edit from removing the gate.

That is also why `posture.provider_managed` exists: the control plane
can see which sessions actually ran under a mandate instead of taking it on
faith.

## What each file guarantees; and does not

### Claude Code; `claude-code/managed-settings.json`

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
design** upstream; an invalid or unmet value is stripped rather than enforced.
Treat version floors as hygiene, never as a control. Deploy this file with
filesystem permissions that make it root-owned and user-unwritable; a managed
settings file the user can edit is not managed.

Target paths (deploy read-only, root-owned):

- Linux; `/etc/claude-code/managed-settings.json`
- MacOS; `/Library/Application Support/ClaudeCode/managed-settings.json`
- Windows; `C:\Program Files\ClaudeCode\managed-settings.json`
  (the older `C:\ProgramData\ClaudeCode\` path is no longer read)

Orgs on a plan with **server-managed settings** should prefer that channel: it
refreshes hourly and, with `forceRemoteSettingsRefresh: true`, the CLI exits
rather than starting without policy; a stronger property than any local file,
because it fails closed at startup.

### Codex; `codex/requirements.toml` and `codex/managed_config.toml`

| Setting | Where | Guarantee |
|---|---|---|
| `allowed_approval_policies` | requirements, top level | Pins which approval modes are selectable; the important one, since `never` would let tool calls auto-run. |
| `allowed_sandbox_modes` | requirements, top level | Pins sandboxing so a session cannot opt out. `danger-full-access` is excluded. |
| `approval_policy`, `sandbox_mode` | managed_config | The defaults a session starts with, inside the allowed sets above. |
| `[features] hooks = true` | managed_config | Hooks are on by default. A *default*, not a pin; see the gap below. |

**Every requirement key must be top level.** TOML binds a bare key written after
a table header to that table, so keys listed below a `[hooks]` header are loaded
as `hooks.allow_managed_hooks_only` and silently ignored. An earlier revision of
this template did exactly that: the mandate was inert while `openbox doctor` and
session posture both reported `managed`. `openbox doctor` now decides from
top-level keys (`devconfig.TopLevelTOMLKeys`), so a mis-nested file reports
*"present but imposes no OpenBox mandate"* instead of claiming assurance it does
not have.

#### Remaining gap; the hook itself is not yet mandated

`allow_managed_hooks_only = true` is shipped **commented out**, and this is the
honest state of the Codex mandate rather than an oversight:

- `hooks` is **not** a `requirements.toml` key. The accepted top-level set in
  Codex 0.145.0 is `allowed_approval_policies`, `allowed_approvals_reviewers`,
  `allowed_sandbox_modes`, `allowed_permission_profiles`, `default_permissions`,
  `remote_sandbox_config`, `allowed_web_search_modes`,
  `allow_managed_hooks_only`, `allow_appshots`, `allow_remote_control`,
  `computer_use`, `windows`, `feature_requirements`, `mcp_servers`, `plugins`,
  `marketplaces`, `rules`, `enforce_residency`, `experimental_network`,
  `permissions`, `models`, `guardian_policy_config`. Hook *definitions* live in
  a config layer (`hooks.managed_dir` → a managed hooks file in codex-rs
  `HooksFile` shape) or in a cloud requirements bundle.
- So enabling hook exclusivity today would make Codex ignore the
  `~/.codex/hooks.json` that `openbox init --provider codex` writes and run **no
  OpenBox hook at all**; governance off, with posture still reporting a mandate.
  Strictly worse than not mandating.

Until a managed hook definition ships, the Codex mandate constrains **approval
and sandbox modes**, and the hook remains user-level and removable. Claude Code
has no equivalent gap: its `managed-settings.json` defines the hook directly.

Target paths:

- Linux/macOS; `/etc/codex/requirements.toml`, `/etc/codex/managed_config.toml`
- MacOS MDM; the `com.openai.codex` preference domain, base64-encoded TOML
  (Jamf/Fleet/Kandji); see `codex/README-mdm.md`
- Cloud-managed requirement bundles are the stronger channel where available,
  for the same reason as Claude Code's server-managed settings.

### Muse Code; `muse/hooks.json` and `muse/policy.json`

A managed hooks file with OpenBox's handlers (and the deny-only `onFailure`
successors Muse's fail-open runtime needs) and a policy that asks Muse to
require that lane. The policy keys come from Muse's 1.4.0 changelog, not from a
reference page, and are **unverified**; validate with `muse config validate`
before deploying. The macOS and Linux paths Muse reads them from are not
documented. See `muse/README-mdm.md`, which also says what this does not stop:
a local administrator can still remove hooks without your MDM.

### Cursor

Not shipped: a Cursor adapter does not exist yet. Cursor gained a hook surface
in v3.11, so this becomes a real template when that adapter lands.

## Deploying

Deploy the files in this directory to the paths above with whatever your fleet
already uses; they are static artefacts with no substitution step. The one value
to fill in is the absolute path to the `openbox` binary in each hook command.

What to preserve when you do:

- **Back up what is already there.** These paths are shared: another tool's
  managed settings may be in the same file.
- **Do not weaken a stricter setting.** If the file already present has
  managed-hooks-only on, or a sandbox required, and this template does not, the
  merge must keep the stricter one. Relaxing a mandate is not an upgrade.
- **A commented-out key is not a setting.** `allow_managed_hooks_only` appearing
  only inside a `//` comment must not read as strict, or it would silently
  replace an operator's live value.
- **Deploy the whole file, not a fragment.** Claude Code merges
  `managed-settings.json` with every `*.json` in `managed-settings.d/`, so a
  partial file in one place and a lock in another is a working combination —
  and one `openbox doctor` reports on.

## Verify

```bash
openbox doctor      # effective posture, and whether provider config is managed
```

`openbox doctor` reports `provider_managed` per provider, which is the same
value the session posture carries; so what you see locally is what the control
plane sees.
