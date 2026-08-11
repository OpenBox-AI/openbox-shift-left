# Phase 05 — Posture flip and privacy docs

## Context links

- Parent: [plan.md](plan.md) · depends on Phase 03
- Precedent: content capture flipped to on-by-default 2026-07-15 (`CLAUDE.md`
  "Working conventions")
- Owner surface: `docs/data-and-privacy.md`, `adapters/common/devconfig/`
- Accepted decision: **default ON**
- The blocker found in validation: `Finops bool` + a resolver that returns `&b`
  unconditionally (`devconfig.go:94` field, `:263` resolver) — an absent config
  field is indistinguishable from an explicit `false`. Compare
  `ContentCapture *bool` (`:91`, resolver `:273`), where absent yields the default.

## Overview

- Date: 2026-08-11 (revised after validation round 2)
- Description: make `Finops` tri-state, then turn usage capture on by default,
  with the opt-out and the docs that make the new egress honest.
- Priority: P1 (without it the feature exists and emits nothing)
- Implementation status: **complete**
- Review status: reviewed (code-reviewer, 2026-08-11) — findings applied

## Key insights

- **The flip does not work without the `*bool` change.** `ResolveFinops`'s closure
  returns a pointer to the plain bool unconditionally, so `resolveBool` never
  falls through to its default — flipping the default parameter alone produces a
  flip that silently does nothing for every existing config file, and worse,
  turns every absent field into `false` forever. Change `Finops bool` →
  `Finops *bool` first, mirroring `ContentCapture`; then the default flip is one
  argument.
- This is the phase that makes the feature real. Everything before it is
  capability; `ResolveFinops()` default-off is why 130 live event rows contain no
  usage at all.
- **A default flip is an egress-posture change, not a config tweak.** New data
  leaves developer machines for orgs that never asked. It gets the same treatment
  content capture got: an explicit opt-out, a documented default, and a
  privacy-docs update in the same change.
- What actually egresses is narrow and worth stating: four integers and a model
  id per turn (per session for Codex; per subagent when `SubagentStop` fires).
  No prompt, no completion, no command, no file body, no cost. Defensible
  precisely because the payload is small and identifier-class — but the claim
  must be written where users read it.
- `ResolveFinops` is the existing name and it is now wrong-ish — it gates model
  identity too, not just token counts. Renaming the config key is a user-visible
  break; **document the widened meaning at the definition** (recommended) unless
  a rename proves free.
- Both an env var and a managed-config key exist for other flags. `EnvFinops`
  already exists; mirror content capture's precedence exactly.

## Requirements

1. `Finops *bool` (tri-state); `ResolveFinops()` defaults to **on**; opt-out via
   managed config `finops:false` and env `OPENBOX_FINOPS=0`; env wins either way,
   matching `ResolveContentCapture`.
2. A test that asserts the absent-field case resolves to on — stated explicitly
   so a future flip is deliberate — plus both opt-out paths and their precedence.
3. Posture reporting includes the effective usage-capture state, so a session's
   SessionStarted evidence records it (mirrors how posture already carries
   staleness).
4. `docs/data-and-privacy.md` states exactly what egresses, when (per turn /
   per subagent / per Codex session), and how to stop it.
5. `README.md` / `docs/architecture.md` updated where they describe telemetry;
   no doc anywhere still claims usage capture is opt-in.
6. `capabilities.go` in both adapters reflects the new default.

## Architecture

```
devconfig.Finops *bool                       (was bool — tri-state is the fix)
devconfig.ResolveFinops()   default true     (was false)
  ├── managed config: finops:false           → disables
  ├── env: OPENBOX_FINOPS=0                  → disables (wins either way)
  └── posture.effectivePosture()             → records the resolved state
```

The resolution order already exists for other flags; this adds no new mechanism —
only the tri-state field, a changed default, and a posture field.

## Related code files

| File | Change |
|---|---|
| `adapters/common/devconfig/devconfig.go` | `Finops *bool`; resolver passes the pointer through; default → on; widened-meaning doc comment |
| `adapters/common/devconfig/devconfig_test.go` | absent-field ⇒ on; config off; env off; precedence |
| `adapters/common/devconfig/posture.go` (+test) | usage-capture in the posture record |
| `adapters/claude-code/capabilities.go` | default + per-turn claim |
| `adapters/codex/capabilities.go` | same, with Codex's per-session caveat |
| `docs/data-and-privacy.md` | new section: what usage capture sends |
| `docs/architecture.md`, `README.md` | telemetry description refreshed |

## Implementation steps

1. Change `Finops` to `*bool`; the resolver closure becomes
   `func(c DevConfig) *bool { return c.Finops }`; flip the default argument to
   true. Grep for direct `.Finops` readers that assumed a plain bool.
2. Tests: absent field ⇒ on (stated as the pinned default); `finops:false` ⇒ off;
   `OPENBOX_FINOPS=0` ⇒ off; env-beats-config both directions — the
   content-capture test matrix, applied here.
3. Add usage capture to the posture record so it rides SessionStarted as
   evidence.
4. Write `docs/data-and-privacy.md`: the four counts, the model id, the cadence
   per provider, on by default, both opt-outs, and the explicit negative list —
   no prompt, no completion, no thinking, no command, no file body, no cost.
5. Record the rename-vs-document decision; if documenting (recommended), the doc
   comment at the definition says it gates model identity too.
6. Update both `capabilities.go` claims and cross-check `docs/architecture.md`'s
   assurance section; grep the tree for any remaining "opt-in" usage-capture
   claim.

## Outcome

**Implemented 2026-08-11.** `Finops` is `*bool` with a pass-through resolver and the default flipped to on; the posture list carries the same pass-through so the record and the resolver cannot disagree. `TestResolveFinops_DefaultOn` pins the absent-field case explicitly — the case the plain bool could not express, and the reason the flip would otherwise have been a silent no-op — plus both opt-outs and their precedence. `TestPostureReportsEffectiveFinopsState` pins the record against the resolver.

Docs: `docs/data-and-privacy.md` gained a Usage-capture section with the positive list (four integers, one model id, index, duration, subagent id) and the negative one (no prompt, completion, thinking, stop reason, command, output, file body, **or cost**), plus the allowlist note. `README.md`, `docs/architecture.md` (four new assurance limits), `COVERAGE.md`, the Codex README, and `CLAUDE.md` are updated. A tree-wide grep for stale finops "opt-in"/"default off" claims found ten and fixed all of them — including one in `hookrun.go` that still described CONTENT capture as default-off, long after that flipped.

Rename-vs-document decision: **documented**. `ResolveFinops` keeps its name (renaming a config key is a user-visible break) and the widened meaning — it gates model identity too — is stated at the field and the resolver.

## Todo list

- [x] `Finops *bool` + resolver pass-through
- [x] Default flipped to on; absent-field test pins it
- [x] Managed-config opt-out verified
- [x] Env opt-out verified
- [x] Precedence between the two verified
- [x] Posture records usage-capture state
- [x] `docs/data-and-privacy.md` section written (positive + negative list)
- [x] `capabilities.go` × 2 updated
- [x] `README.md` / `docs/architecture.md` cross-checked; "opt-in" claims gone
- [x] Rename-vs-document decision recorded

## Success criteria

- A fresh install — and an existing config file with no `finops` key — emits
  per-turn usage with no configuration.
- Either opt-out silences it completely — no usage, no model, no residue in
  metadata or `activity_output`.
- The posture on SessionStarted says which state was in effect.
- `docs/data-and-privacy.md` describes the egress accurately enough that a
  privacy reviewer needs no code reading.
- No doc anywhere still claims usage capture is opt-in.

## Risk assessment

| Risk | Likelihood | Impact | Mitigation |
|---|---|---|---|
| Flip lands without the `*bool` change and silently does nothing | was certain | high | The `*bool` change is step 1 and its absent-field test is the pin |
| Orgs receive new data unannounced | **certain** by design | medium | Documented default + two opt-outs + posture evidence; this is the accepted decision |
| Opt-out leaves partial residue (model but no tokens) | medium | high | Test asserts *both* absent when disabled |
| Docs still say "opt-in" somewhere | high | medium | Tree-wide grep is a checklist item |
| `ResolveFinops` name misleads maintainers | high | low | Widened meaning documented at the definition |
| Default flip lands without the docs | medium | high | Same commit, or do not ship |

## Security considerations

- Enumerate what egresses in the docs, positively and negatively. The negative
  list is the valuable half: no prompt, no completion, no thinking, no command,
  no file body, no cost.
- The opt-out must be complete. A disabled flag that still leaks the model id
  would be worse than the old default because it contradicts its own
  documentation.
- Posture evidence means an auditor can tell from the event stream whether
  capture was on for a given session — it is what makes the default defensible
  after the fact; do not skip it.
- INV-1 untouched: no credential is involved anywhere in this path.

## Next steps

Phase 06 — prove it against a live stack, which is the only evidence that counts.
