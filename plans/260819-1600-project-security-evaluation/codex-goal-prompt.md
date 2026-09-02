# Codex `/goal` implementation prompt

Paste this as one `/goal` submission:

```text
/goal Implement plans/260819-1600-project-security-evaluation sequentially with lean, clean, evidence-backed code.

Sources and scope
- Treat plan.md and phase-00…phase-08 as the sole delivery/progress ledger. Obey AGENTS.md, CLAUDE.md, dc/security-evaluate.md, and accepted ADRs; resolve conflicts before coding.
- Preserve unrelated dirty/untracked work. Never reset, commit, publish, deploy, upload audit data, use production credentials/endpoints, or write to an external control plane without my explicit authorization.
- Stay inside plan scope and reuse existing code. Add no module, endpoint, table, service, SDK scan mode/fork, or compatibility shim unless an accepted ADR requires it.

Execution loop
1. Select the earliest dependency-ready task in the earliest phase lacking a verified exit gate.
2. Read only the parent summary, current phase, relevant code/contracts, and current primary docs. Avoid reloading the whole plan.
3. Inspect status and owned paths. Mark exactly one root task `in_progress`; only the root edits plan status/logs.
4. Implement the smallest complete change. Keep assurance under `cli/internal/assurance`, outside `provider.HookEngine` and `adapters/common/hookflow`.
5. Run focused unit/contract/adversarial tests, affected module tests, then the phase exit proof. Check `git status --short`, changed/untracked scope, and `git diff --check` separately. Docs/mocks are not live proof.
6. Mark `implemented` only with code and passing scoped tests; mark `verified` only after required real evidence. Missing or conflicting evidence is `inconclusive`/`not_runnable`, never a pass.
7. Update the phase log and parent rollup together with commands, versions, artifacts, limits, and changed assumptions. Continue automatically to the next ready task/phase.

Sub-agents
- You are explicitly authorized to use at most 2 concurrent sub-agents for bounded independent work: current source qualification, one isolated package with disjoint files, fixture construction, or independent review.
- Give each one task ID, deliverable, allowed paths, read/write authority, tests, and return format: findings, files changed, commands/results, risks/blockers. Never overlap edits or let a sub-agent edit the ledger.
- Prefer `fork_turns:"none"` with a self-contained brief; use 2–3 recent turns only when essential and full history only when unavoidable. Reuse agents for follow-ups. Root reviews every result/diff and owns integration.
- Keep ADR/authority decisions, cross-phase architecture, final judgment, release claims, and skill interpretation with the root.

Architecture gates
- Existing framework SDK remains normal middleware; fixtures supply poisoned data and safe sinks. Missing SDK events mean unknown/missing coverage, not “no action.”
- Before project launch, the selected native sandbox must pass exact-version/config parent+child filesystem, network, loopback, credential, fallback, timeout, and cleanup probes. Never retry unsandboxed or switch drivers mid-run.
- Baseline is loopback/test-identity only and returns ALLOW. Only an explicitly authorized isolated real OpenBox decision path can prove `blocked`; mock BLOCK, refusal, crash, or sandbox denial cannot.
- Phase 08 is discovery/ADR/separate planning for OpenBox Sandbox ProjectRun v2. Do not weaken v1 or implement v2 in this goal.

Ask me only for a real owner decision, unsafe ambiguity, missing authority, paid/live run, or upstream blocker; report task, evidence, options, and recommendation. Complete the goal only when every in-scope exit gate is verified or formally `not_applicable`; difficulty or duration is not a blocker. Final handoff: task totals, changed paths, tests/live proofs, unsupported tuples, remaining risks, and separately authorized work.
```
