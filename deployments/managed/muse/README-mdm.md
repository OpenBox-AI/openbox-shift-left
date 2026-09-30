# Deploying OpenBox for Muse Code to a fleet

This is a **fleet opt-in**. `openbox init --provider muse` never writes any of
it: it registers OpenBox's hooks in the developer's own
`~/.config/muse/settings.json`, where the developer can edit or delete them.
What is here is what an org deploys with its own MDM or configuration
management, so that Muse itself requires the hook lane.

**Read this first: none of this makes OpenBox tamper-resistant.** A managed
file is only as managed as its permissions. Deploy it root-owned and
user-unwritable; a policy or hooks file the developer can edit is not a
mandate. And a local administrator can always remove hooks, the policy or Muse
itself without going through your MDM. What the bundle buys is that the
*default* user, and Muse's own `--yolo` and approval judge, cannot switch the
gate off by accident or by a flag; it does not stop someone with root who means
to.

## Files

| File | What it is |
|---|---|
| `hooks.json` | The managed hooks file: one catch-all handler per event OpenBox observes, with a deny-only `onFailure` successor on the four gated ones (`UserPromptSubmit`, `PreToolUse`, `PermissionRequest`, `PreLLMCall`). The same handlers, timeouts and successors `openbox init` writes into user settings (a test holds the two equal), except the engine path is the placeholder `OPENBOX_BIN`. |
| `policy.json` | The policy that requires the managed hook lane and pins Muse's own approval layer. |

Before deploying, replace `OPENBOX_BIN` in `hooks.json` with the absolute path
of the `openbox` binary on the fleet (every occurrence; it is already inside
quotes, so a path with spaces is fine). A managed hook cannot rely on the
working directory or on `PATH`.

**A non-default `OPENBOX_HOME` needs `--home` in these commands.** Muse clears a
hook's environment, so the variable does not reach the hook and a hook that
cannot find its home binds the default one and governs nothing. On a fleet that
keeps the OpenBox home anywhere but `~/.openbox`, write `--home "<absolute dir>"`
after `hook muse` in every command and successor in `hooks.json`
(`"OPENBOX_BIN" hook muse --home "/opt/openbox" PreToolUse`), the way
`openbox init` does for the user-level install. Listing `OPENBOX_HOME` in
`allowed_env_vars` is an unverified second route; do not rely on it alone.

## What the policy asks for

| Key | Intent |
|---|---|
| `extensions.hooks.require_managed_lane: true` | Sessions are refused without the managed hook lane, and a locally disabled required managed hook is refused. This is Muse's counterpart of Claude Code's `allowManagedHooksOnly`. |
| `execution.approval_modes` | Pins which approval modes a session may select. |
| `execution.allow_user_approval_override: false` | A user cannot widen that set. |
| `extensions.hooks.allowed_env_vars` | The environment names a managed hook may receive. Muse clears a hook's environment; `OPENBOX_HOME` is listed so a non-default OpenBox home can reach it. **No credential goes here**, and Muse refuses provider credential names in it anyway. |

**The envelope is verified; the setting keys are not.** Muse 1.4.1 rejects a
policy document with no `schema_version` (`enterprise_document_invalid:
plane=policy reason=missing_schema_version`), so `policy.json` is
`{"schema_version": 1, "settings": {...}}`. The keys inside `settings` come from
Muse's 1.4.0 changelog, which names the members but has no reference page: the
exact key for "managed lane required" and the value names in `approval_modes`
are guesses. **Run `muse config validate --plane policy --file policy.json` on
the file before you roll it out** and fix what it rejects (exit 0 valid, 4 valid
with inactive members, 1 rejected; exit 4 means a member did nothing).

Where Muse reads the policy and managed hooks from is documented for Windows
only (`%ProgramFiles%\muse\`). On macOS and Linux the location is not
documented: find it on a real install (`muse config status` is the place to
start) before deploying, and do not trust a path written here.

## Verify

```bash
muse config validate --plane policy --file policy.json
muse config status
openbox doctor
```

`muse config validate` checks one enterprise plane document (`--plane
defaults|policy`), not `settings.json`. `muse config status` lists the four
sources Muse reads (`plane=defaults|policy` by `source_class=system_file` or
`macos_managed_preferences`) and whether each is present or absent. `openbox
doctor` runs `muse config status` and reports, per source, whether a policy is
present; it says "managed lane required: unknown" unless one is, because the
status output does not say what a policy requires. It cannot see whether Muse
actually loads the managed hooks file; the `load probe` row only confirms
OpenBox's handlers ran.

## Open questions

- **Two registrations.** Muse runs every hook source, managed then user, and
  `openbox init` (which a developer still runs to get an identity) also writes
  the user-level handlers. A machine with both evaluates each gated call twice.
  Core dedupes the recorded events on their idempotency key, but nothing here
  has been measured on a Muse binary. Until it is, treat a managed deployment
  as additive to the user-level install, not a replacement for it.
- **Identity is still per developer.** The managed hooks run `openbox hook muse`,
  which resolves the agent in `~/.openbox/muse/`. A developer without one gets
  the same "governance is inactive" notice as on an unmanaged machine, not a
  denial: the hook logs and exits 0 on a missing identity by design.
