---
title: "Fix stale hook registration and the posture wire gap"
description: "Make `openbox init` replace its own hook entries at a stale engine path, and put decision_authority/failure_policy on the wire while deleting the five dead bundle keys."
status: complete
priority: P1
effort: 6.5h
branch: main
tags: [bugfix, hooks, installer, posture, the decision record, doctor, docs]
created: 2026-08-14
---

# Goal

Two verified defects from the first post-that decision live session. No new table/endpoint/service
⇒ no new decision record (CLAUDE.md "Core principle").

**Issue 1 (high).** `init` matches its own hook entries by exact command string
(`adapters/claude-code/localhooks.go:133-147`), so an entry from a different engine path reads as
foreign and is kept; re-init appends beside it (`:100-104`), both engines fire, every governed
tool call stores twice. Fix: recognize by argv shape, replace, print.

**Issue 2 (low-moderate).** `Posture.Metadata()` (`adapters/common/devconfig/posture.go:242-271`),
the only path onto the wire, omits `decision_authority`/`failure_policy` that that
decision:237-239 promises, and still lists five dead bundle-era keys. Fix: add two, delete five.

**Context:** `plans/reports/issue-260814-0106-stale-hook-registration-and-posture-gap.md`;
`research/researcher-01-hook-registration.md`; `research/researcher-02-posture-wire.md`

# Phases

| # | Phase | Status | Effort | Depends on |
|---|---|---|---|---|
| 01 | [Stale-engine-aware hook merge](phase-01-stale-engine-hook-merge.md) | complete | 2.5h | — |
| 02 | [Posture wire keys + dead-key deletion](phase-02-posture-wire-keys.md) | complete | 1.5h | — (parallel with 01) |
| 03 | [`openbox doctor` duplicate-engine check](phase-03-doctor-duplicate-engine-check.md) | complete | 1.5h | 01 (reuses its classifier) |
| 04 | [Docs reconciliation + verification sweep](phase-04-docs-and-verification.md) | complete | 1h | 01, 02, 03 |

03 is the issue's optional addendum — cuttable, costing one docs bullet in 04. File ownership is disjoint except 01→03 on `localhooks.go`, hence 03 is sequential.

# Key decisions

- **D1 (user-confirmable): replace-and-print, not warn-and-refuse.** Refusing leaves the
  double-counting install in place and blocks the only command that repairs it. Notice on stderr,
  matching `devconfig.warnDeprecatedKeys` (`devconfig.go:466-477`) and init's plaintext-key
  warning (`cli/cmd/openbox/init.go:197-199`).
- **D2 (user-confirmable): `failure_policy` gets its own key** although `fail_closed` already
rides in `Flags()` (`posture.go:204-205`) and the string is derived 1:1 from that bool
(`:153-156`) — genuinely redundant as information. Adopted so code matches that
decision:237-239 literally; the alternative is amending a shipped decision record to promise
less.
- **D3: the argv-shape classifier stays adapter-local** (ported from
  `adapters/codex/installer.go:262-302`, not shared). The adapters are separate modules
  (`go.work`), owned vocabularies differ (CC also owns `rewake claude-code`), containers differ
  (`map[string]any` vs typed `matcherGroup`, `installer.go:220-260`). A shared home costs a 12th
  module + `go.work` + two `go.mod` edits + CI's module-count check (`ci.yml:41-48`) for ~25
  lines, or a refactor of a shipping installer for no behavior gain. Trigger to revisit: Cursor.
- **D4: delete the `Staleness` type and its 7 consts** (`posture.go:32-55`) with the five keys,
  per the `require_verified_bundle` precedent — deleted outright, absence asserted
  (`posture.go:214-217`, `posture_test.go:166-169`). Only tests read them; 2 enum-only tests get deleted, not rewritten.

# Verification summary

Per touched module, mirroring CI (`.github/workflows/ci.yml:68,81,85`): `go test -race ./...`, then
`GOOS=windows GOARCH=amd64 go build ./...` and `GOOS=linux GOARCH=arm64 go build ./...`.
Unit tests are not evidence a hook works. Unverified until a live stack runs: (a) that a re-inited
settings file still produces events in a real session (01); (b) that `decision_authority` reaches
`governance_events` — `testbed/30-enforce.sh:185-186` already asserts `control_plane`/`fail_open`
on the SessionStarted row and **fails today**, and 02 is what makes it pass; (c) that the
double-count disappears end to end (two engine paths + a live session). Phase 04 adds one dormant
assertion to `testbed/10-onboard.sh`: plant a bogus engine path, re-init, assert one registration
and zero bogus paths.

# Unresolved questions

1. Global/managed scope (`cli/internal/managed/managed.go`) not audited for a differently-shaped
   stale-engine problem — `wouldWeaken`/`applyFile`/`render` unread (research Q4a). Follow-up.
2. How many installs already double-count is unmeasurable from here; 03 makes it
   self-diagnosable, and nothing back-fills already-stored duplicate rows.
3. An unquoted engine path *containing a space* stays unrecognized (01 R2). Accepted: only a
   pre-quoting build could write it, and those hooks never started (`localhooks.go:156-169`).
