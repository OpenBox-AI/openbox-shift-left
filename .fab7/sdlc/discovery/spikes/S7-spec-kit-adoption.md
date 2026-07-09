# Spike S7 — GitHub spec-kit deep-dive: what shift-left should adopt

**Question (one sentence):** What should shift-left learn/adopt from GitHub's spec-kit (spec-driven development toolkit)?

**Status:** DONE (2026-07-09). **Method:** primary-source research (repo, docs site, SDD blog, templates, `specify` CLI source), cited. **Owner:** brian.
**Recency:** spec-kit v0.12.8 (2026-07-08), MIT, GitHub-owned, ~106k–119k stars (fast-moving), Python. Version skew observed: commands namespaced `/speckit.*`; per-agent "commands" → "skills" migration in v0.12.x.

## What spec-kit is
Spec-Driven Development: *intent is the source of truth* — the **spec is an executable artifact that determines what's built**, front-loading structured intent so the agent validates incrementally. Author-time + advisory; enforces nothing at runtime. ([SDD blog](https://github.blog/ai-and-ml/generative-ai/spec-driven-development-with-ai-get-started-with-a-new-open-source-toolkit/), [repo](https://github.com/github/spec-kit))

## Workflow & artifacts
Loop: **Constitution → Specify → (Clarify) → Plan → Tasks → (Analyze) → Implement** (+ `checklist`, `taskstoissues`, `converge`), each phase a Markdown artifact feeding the next. Namespaced `/speckit.*` commands. Layout:
- Control plane `.specify/`: `memory/constitution.md`, `scripts/`, `templates/` (spec/plan/tasks + overrides), `extensions/` (`extensions.yml` hook registry).
- Per-feature `specs/[###-feature]/`: `spec.md` (WHAT/WHY; `FR-###`, `SC-###`, `[NEEDS CLARIFICATION]`, P1/P2/P3 stories, Given/When/Then), `plan.md` (HOW; **Constitution Check gate**; research/data-model/contracts/quickstart; Complexity Tracking), `tasks.md` (`[T###] [P?] [US#]`, test-first enforced).

## Constitution (governance-relevant)
`.specify/memory/constitution.md` — versioned (semver: MAJOR=remove/redefine, MINOR=add, PATCH=clarify; ratification/amendment dates), MUST-principles + rationale + Governance (amendment/compliance). Constrains downstream two ways: (1) **propagation on amendment** re-aligns templates + emits a **Sync Impact Report** (auto HTML comment, version bump + affected templates); (2) **enforcement at gates** — `plan.md` Constitution Check blocks; `/analyze` treats constitution MUST-violations as **CRITICAL**. Extension hooks fire around updates via `.specify/extensions.yml`. ([constitution cmd](https://github.com/github/spec-kit/blob/main/templates/commands/constitution.md), [spec-driven.md](https://github.com/github/spec-kit/blob/main/spec-driven.md))

## `/analyze` — the closest thing to a governance engine
Read-only cross-artifact consistency over spec↔plan↔tasks↔constitution: 6 passes (Duplication, Ambiguity, Underspecification, Constitution Alignment, Coverage Gaps, Inconsistency); **severity CRITICAL/HIGH/MEDIUM/LOW**; **deterministic stable finding IDs** (clean re-run diffs); **bidirectional coverage tables** (requirement→task, task→requirement, coverage %). ([analyze cmd](https://github.com/github/spec-kit/blob/main/templates/commands/analyze.md))

## Agent-agnostic integration = file scaffolding, NO runtime
`specify init` writes `.specify/` once, then projects one canonical command set into each agent's native folder/format + a context file. 30+ agents. Mapping ([integrations ref](https://github.github.io/spec-kit/reference/integrations.html)): Claude Code `.claude/skills/` (older `.claude/commands/*.md`) + `CLAUDE.md`; Copilot `.github/prompts/*.prompt.md`; Cursor `.cursor/skills/` + `.cursor/rules/`; Codex `.agents/skills/` + `AGENTS.md`; Gemini `.gemini/commands/` + `GEMINI.md`; etc. **Same content, N renderings** (differ only by dir + extension + invocation). `specify` CLI: `init` (`--integration`, `--integration-options "--skills"`, `--script`, `--here`, `--force`), `check`/`self check` (verify agent CLIs installed), `self upgrade`, and newer `extension`/`preset`/`bundle` marketplace subcommands. CLI validates *tooling*, not artifact content (that's agent-run `/analyze`).

---

# SYNTHESIS — adopt / learn (opinionated)

**Framing:** spec-kit and shift-left are complementary opposites — spec-kit is **author-time + advisory** (structures intent, makes it checkable, enforces nothing); shift-left is **observe-then-enforce at developer runtime with real lineage**. Spec-kit is the best available source of *artifact schemas + prompt patterns* to give shift-left's lineage something meaningful to bind to.

### (a) Methodology / process
1. **The "constitution" pattern IS governance-as-code** — a versioned repo-level MUST-principles doc with propagation + Sync Impact Report + semver. Treat a repo's constitution (or an OpenBox-native equivalent) as a **first-class governed object**: observe amendments (Phase 1), and make constitution MUST-violations the CRITICAL gate (Phase 2).
2. **Steal `/analyze`'s severity + coverage model** — CRITICAL/HIGH/MEDIUM/LOW (constitution-MUST = CRITICAL), **stable finding IDs**, bidirectional coverage %. Align shift-left's runtime findings vocabulary with author-time findings.
3. **Advisory-gates-first validates observe-first** — spec-kit gates are adopted *because* they don't block. Confirms shift-left's Phase-1 stance; make checks blocking only in Phase 2.
4. **Forced-clarification & test-first are observable signals** — `[NEEDS CLARIFICATION]` and "tests must FAIL first" are machine-detectable in the session/diff stream → cheap high-signal metrics ("committed with unresolved clarification markers"; "code without a preceding failing test") with no enforcement.

### (b) Artifact / traceability — HIGHEST-VALUE BORROW
Spec-kit defines the **upstream half of the lineage chain shift-left is missing**. Today shift-left = *session → commit → deploy*; spec-kit lets it extend **leftward to intent**:
> `constitution@v2.1 → spec FR-014 → task T027 → session S → commit abc123 → deploy` — a novel, defensible lineage graph.
- **Bind commits to the tasks/spec that drove them:** tasks carry stable `T###`, stories `US#`, and `/speckit.taskstoissues` maps tasks→GitHub issues. Capture the constitution/spec/task versions active during a session and stamp them into the commit's lineage (extends the `OpenBox-Session` trailer idea, S3).
- **Stable detection anchors, zero agent cooperation:** `specs/[###-feature]/` and `.specify/memory/constitution.md` are conventional paths shift-left's observer can watch for presence/changes — works across all 30+ agents because spec-kit standardizes the *files*, not the agent.
- **Sync Impact Report → governance event:** ingest each constitution amendment (already a structured versioned diff) into the OpenBox pipeline for policy-change lineage.
- **Coverage % as a deploy-gate metric:** attach `/analyze`'s requirement→task coverage to a deploy ("shipped at 82% coverage, 2 HIGH open").

### (c) Integration mechanism (vs shift-left plugin/CLI hybrid, OD18)
- **Adopt spec-kit's "one canonical source, N per-agent renderings" installer discipline** for shift-left's own bundles — a per-agent registry (key → folder → format → requires_cli) so the `openbox` CLI's Claude Code plugin / Codex / Cursor bundles don't drift. Consider mirroring `integration`/`bundle`/`preset` subcommand UX.
- **Reuse spec-kit's standardized folder conventions** (`.claude/`, `.cursor/`, `.agents/`+`AGENTS.md`, `.gemini/`) as shift-left install targets; lets shift-left *observe spec-kit-driven sessions specifically*.
- **Converge on `AGENTS.md`** as a supported cross-agent context surface rather than inventing another.
- **Ship an `openbox check`/doctor command** (à la `specify check`) verifying the bundled binary, hook registration, per-agent wiring.

### (d) Do NOT adopt
1. **Not spec-kit's advisory-only, agent-executed enforcement as an end state** — its gates run inside the agent via prompts and can be ignored; no runtime interception. Shift-left should *consume* spec-kit's checkable artifacts but keep enforcement in its own out-of-band pipeline (hooks/binary).
2. **Don't bind to the churny `speckit.` command names / commands→skills / marketplace layer** — bind to the **stable artifact files/paths** (`spec.md`/`plan.md`/`tasks.md`/`constitution.md`, `specs/[###]/`, `.specify/`).
3. **Don't inherit spec-kit's prescriptive constitution *content*** (Library-First, max-3-projects, real-DBs-NON-NEGOTIABLE) — adopt the *mechanism* (versioned principles + propagation + Sync Impact Report), not the opinions.
4. **Don't dilute shift-left's cost/tool observability** to imitate spec-kit's stateless model — that telemetry is shift-left's differentiator.
5. **Don't adopt file-scaffolding-only delivery** — it can't observe or enforce; the plugin+binary+managed-hooks hybrid (OD18) is correct.

## Decisions

- **OD21 — DECIDED (2026-07-09): DO NOT adopt spec-kit artifacts / intent-lineage.** Shift-left governs the developer **runtime**, not the authoring method. spec-kit's constitution/spec/tasks and the "extend lineage leftward to intent" idea are **out of scope**; shift-left's lineage stays session→commit→deploy. This spike is retained as **reference/learning only** — no stories, no dependency on spec-kit's files, command surface, or CLI.
- **OD22 / OD23 — not adopted as spec-kit dependencies.** The generic engineering patterns they name (findings severity + stable IDs; a `check`/doctor command; one-canonical-source→N-renderings installer discipline; `AGENTS.md` as a context surface) remain available as **ordinary good practice** the SL-2/SL-4 builders may apply on their own merits — but they are NOT tracked as spec-kit adoption and carry no decision gate. Not pursued further unless raised.

**Net:** no change to the shift-left backlog, architecture, or lineage design as a result of this spike.

## Unknowns
Exact star count (~106k–119k); whether Windsurf/Amazon Q/Roo still supported; current Claude Code default (`.claude/skills` vs `.claude/commands`) at v0.12.8; `/speckit.converge` internals.

## Sources
[repo](https://github.com/github/spec-kit) · [SDD blog](https://github.blog/ai-and-ml/generative-ai/spec-driven-development-with-ai-get-started-with-a-new-open-source-toolkit/) · [docs](https://github.github.com/spec-kit/) · [integrations](https://github.github.io/spec-kit/reference/integrations.html) · [templates/](https://github.com/github/spec-kit/tree/main/templates) · [spec-driven.md](https://github.com/github/spec-kit/blob/main/spec-driven.md) · [analyze cmd](https://github.com/github/spec-kit/blob/main/templates/commands/analyze.md) · [CLI src](https://github.com/github/spec-kit/blob/main/src/specify_cli/__init__.py).
