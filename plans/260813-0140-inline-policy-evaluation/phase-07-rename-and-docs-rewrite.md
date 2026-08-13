# Phase 07 — Three named features; rewrite the docs that are now untrue

## Context links

- Parent: [plan.md](plan.md) · Depends on: phase 6 (docs describe what shipped)
- Blocks: phase 8 · Authorized by: ADR-0017
- Repo rule (`CLAUDE.md`): "a governance product that overstates itself is the failure it
  exists to prevent, so prefer an honest limit over a confident sentence."

## Overview

- **Date:** 2026-08-13
- **Description:** Replace tier numbers with three plain-language features, rewrite the
  privacy documentation for content-on-enforcement, and fix the claims that are now false.
- **Priority:** P1 · **Implementation status:** done · **Review status:** pending

## Key Insights

- **Tier numbers leak implementation into the user's head.** Three sentences replace them,
  and each names where it happens, which implies its own failure mode:

  | Say this | Was |
  |---|---|
  | "OpenBox decides before the tool runs. Can't reach OpenBox? Your org's failure policy applies." | Tier 1 + Tier 2 |
  | "Secrets in file writes are redacted on your machine, before anything leaves it." | Tier 1 secret detection |
  | "Findings from OpenBox are surfaced back into your session." | Tier 3 |

- **There is an existing doc bug to fix while here.** `architecture.md:82` says "Within
  enforce there are three tiers", but the findings loop is enforce-independent — the call site
  says so: "Orthogonal to enforce — findings are advisory feedback in both observe and enforce
  sessions" (`adapters/claude-code/hookrun.go:254`). Renaming without fixing this would
  preserve the error in new words.
- **Two README claims become false and one is a privacy claim**, which makes it the serious
  one: "Tool commands and file bodies are **never** sent on ordinary telemetry — only on an
  approval request" and "Credentials never leave the OS keychain" (the latter already handled
  by the auth/init plan). The new truth: on enforcement, the tool's content is sent for
  evaluation when content capture is on, with secrets redacted first.
- **Existing `content_capture:true` installs change behaviour on upgrade** — bodies that never
  left start leaving. A release note is the minimum; whether affected orgs must be *notified*
  is a product/legal call this phase surfaces rather than decides.
- **`openbox dev sync` disappearing needs an entry too** — anyone with it in a pipeline gets
  an error, and the note should say what replaced it (nothing: policy is fetched per call now).

## Requirements

1. Tier vocabulary removed from code comments, CLI help, config docs and all user docs.
   ADR history keeps its own words — ADRs are records, not current docs.
2. `docs/architecture.md`: rewrite the enforcement section to the three named features; fix
   the findings-orthogonality error; keep the mode table (observe / advisory / enforce) since
   modes still exist.
3. `docs/data-and-privacy.md`: **rewrite** the content section. State exactly what egresses on
   an enforced call — structural fields always, content when capture is on, redacted first —
   and that `content_capture:false` yields structural-only enforcement.
4. Disclose the 64KB evaluation cap (`capBody`) where content policy is described: content
   matching sees at most the first 64KB of a large write.
5. Disclose the combination secret-detection-off + content-capture-on ⇒ unredacted bodies
   egress.
6. `README.md`: fix the "never sent on ordinary telemetry" claim; keep the limits list
   truthful, including reachability-dependent enforcement and the fail-open bypass.
7. `docs/getting-started.md`: no `dev sync` step; explain the failure policy in one sentence
   and where to edit it (`fail_closed` in dev.json, org-lockable).
8. `docs/adr/ADR-0008-signed-policy-bundles.md`: mark superseded-in-part, pointing at
   ADR-0017. Do not edit its reasoning.
9. Release note: content egress change, `dev sync` removal, deprecated config fields, and
   that leftover bundle/pin files on disk are inert and may be deleted.
10. Reconcile ADR-0017 and the phase-2 assurance limits against what actually shipped; fix any
    drift.

## Architecture

Documentation only. The one structural decision is which document owns which claim: the ADR
owns *why*, `architecture.md` owns the model, `data-and-privacy.md` owns the field-level
truth, README owns the summary and the limits, getting-started owns the flow.

## Related code files

| Path | Action |
|---|---|
| `docs/architecture.md:78-95` | enforcement section rewrite + findings-orthogonality fix |
| `docs/data-and-privacy.md` | content section rewrite |
| `README.md` | "never sent" claim; limits list |
| `docs/getting-started.md` | drop `dev sync`; failure policy in one sentence |
| `docs/adr/ADR-0008-*.md` | superseded-in-part header |
| `cli/cmd/openbox/main.go` usage/help | tier vocabulary out |
| `adapters/common/hookflow/*.go` comments | tier vocabulary out |

## Implementation Steps

1. Grep the tier vocabulary across code and docs; list every hit before editing, so the sweep
   is provably complete rather than best-effort.
2. Rewrite `architecture.md`'s enforcement section to the three features; fix the findings
   error in the same edit.
3. Rewrite `data-and-privacy.md`'s content section; add the 64KB cap and the
   detection-off combination.
4. Fix README's privacy claim and limits list.
5. Update getting-started; remove `dev sync`.
6. Mark ADR-0008 superseded-in-part.
7. Write the release note.
8. Re-read ADR-0017 against the shipped code; fix drift in either direction.

## Todo list

- [x] Tier-vocabulary hit list produced, then cleared
- [x] `architecture.md` rewritten; findings-orthogonality bug fixed
- [x] `data-and-privacy.md` content section rewritten
- [x] 64KB cap disclosed; detection-off combination disclosed
- [x] README "never sent" claim fixed; limits list truthful
- [x] getting-started drops `dev sync`; failure policy explained
- [x] ADR-0008 marked superseded-in-part
- [x] Release note written (content egress, `dev sync`, deprecated fields, inert files)
- [x] ADR-0017 reconciled with shipped behaviour

## Success Criteria

- `grep -ric "tier" --include=*.go --include=*.md` outside `docs/adr/` returns 0.
- A developer can state the enforcement model in three sentences after reading
  `architecture.md`.
- A privacy reviewer can determine from `data-and-privacy.md` alone whether their file bodies
  leave the machine, under which setting, and how much of them.
- No document claims content never egresses, a signature that is not checked, or governance
  that survives an unreachable control plane.
- Nobody upgrading discovers the content change from behaviour rather than from the note.

## Risk Assessment

| Risk | L×I | Observable signal it broke | Pre-decided response |
|---|---|---|---|
| Rename lands but a stale privacy claim survives | M×H | README or data-and-privacy still says content never egresses | **Stop:** this is the exact failure `CLAUDE.md` names. Treat as a release blocker. |
| The findings-orthogonality bug is renamed rather than fixed | M×M | new wording still nests findings inside enforce | **Adjust:** cite `hookrun.go:254` in the doc edit. |
| Release note written but never surfaced to existing orgs | M×H | orgs learn from traffic | **Escalate:** notification is a product/legal call; this phase names it as an open question, not a done item. |
| Docs describe intended rather than shipped behaviour | M×M | a claim contradicts the code | **Adjust:** step 8 exists for this; any contradiction is a blocker. |
| "Three features" framing hides the failure policy | M×M | a reader cannot tell what happens offline | **Adjust:** the first sentence carries it deliberately — do not trim it for elegance. |

## Security Considerations

The docs must state, where a user will actually read them: content egresses on enforced calls
when capture is on; secrets are redacted first but the rest of the body is not; the server's
content view is capped at 64KB; enforcement depends on reachability and under the default
fail-open can be disabled by blocking one host. Each of those is a real property of the
shipped system, and each is the kind of sentence a governance product is tempted to omit.

## Next steps

Phase 8 verifies all of it against a real stack.
