# Spike S3 — Git commit → coding-agent session attribution

**Question (one sentence):** What are the correct rules for attributing a git commit to the session(s) that produced it, when one commit can span multiple sessions and git history can be rewritten?

**Status:** DONE (2026-07-07). **Owner:** brian@openbox.ai.
**Method:** git man pages (git-interpret-trailers(1), githooks(5), git-notes(1), git-rebase(1), git-cherry-pick(1), git-merge(1), gitformat-commit) + GitHub merge docs + local code-graph evidence. git in env: 2.53.0.

---

## 0. Local evidence — what exists to reuse

- **Greenfield for git handling:** no commit-trailer or git-hook code exists in any of the four repos. Grep for `OpenBox-Session|interpret-trailers|prepare-commit-msg|Co-Authored-By|git notes` hits only the design/discovery docs. The `*hooks*` files found are unrelated (fab7-sdlc skill hooks; Temporal runtime-governance hooks).
- **Reusable primitives for the FR-6 Deploy DID (server-side):** `didFor(agentId)` — `openbox-backend/src/modules/did/naming.ts:8`; SHA-256 hashing — `api-key.service.ts:258`, `AgentEntity.createHash` (`agent.service.ts:691`); tamper-evident store `SessionMerkleLeafEntity` (`openbox-core/internal/datastore/session_merkle_leaf_pgx.go`).

## 1. Trailer mechanics (observed, cited)

- Trailers are the final `Key: value` paragraph of the commit message; `git interpret-trailers` edits only that block. — git-interpret-trailers(1), gitformat-commit(5).
- **Multiple same-key trailers are valid and normal** (`Signed-off-by`, `Co-Authored-By`); GitHub renders each `Co-Authored-By` as a distinct author. → **Multiple `OpenBox-Session:` lines is the intended multi-session representation.** — GitHub "commit with multiple authors".
- Idempotency via `--if-exists`/`trailer.ifexists` (`addIfDifferent`, `replace`, `doNothing`, …). Register `git config trailer.openbox-session.key "OpenBox-Session"`. — git-interpret-trailers(1).
- Read all values: `git log -1 --format='%(trailers:key=OpenBox-Session,valueonly,separator=%x0A)' <sha>`. — git-log(1).
- `prepare-commit-msg` runs before the editor with a source arg; **`--amend` re-fires it (source `commit`)** → hook must be idempotent. — githooks(5).
- **CRITICAL: git hooks are LOCAL and are not cloned/transferred.** They don't exist on teammates' machines, CI, or GitHub. → **cannot rely on any hook to re-stamp during rewrites elsewhere; attribution must be resolvable from data already in the commit.** — githooks(5).

## 2. Behavior under rewrites (observed)

| Operation | New SHA? | Trailer fate | Effect |
|---|---|---|---|
| `--amend` | yes | reused msg keeps trailer; hook re-fires → dedupe needed | same session(s) |
| plain/`--onto` rebase, clean pick | yes | message copied verbatim → **survives** | 1:1 under new SHA |
| interactive reword/edit | yes | editor may drop block | preserved unless human edits out |
| **squash** | yes | **concatenates ALL messages** → every session line aggregates | **multi-session fan-in (good path)** |
| **fixup** | yes | **discards fixup commit's message** → trailer **LOST** | **silent loss** |
| cherry-pick | yes | message copied → preserved (`-x` adds provenance) | preserved under new SHA |
| non-ff merge | yes | merge commit has own (trailer-less) msg; originals stay reachable | attribute originals, not merge node |
| ff merge | no | intact | unchanged |
| **GitHub "Squash and merge"** | yes | body defaults to concatenated commit msgs **but editable/often replaced by PR desc** → can drop | **highest-risk loss**; re-attaches Co-Authored-By |
| force-push | replaces SHAs | pushed = rewrite output | pre-push SHA bindings dangle |

**Two hard conclusions:**
1. **Never bind session→commit to a pre-push SHA** — amend/rebase/squash/force-push all change SHAs. Resolve at push (exactly where FR-6 runs).
2. **Only two loss modes:** `fixup`/message-replacement, and GitHub-squash body-replace. Both handled by the unattributed+heuristic rule (§6).

## 3. Carrier choice — trailer vs git notes

- git notes are keyed by SHA, **not part of the commit object**, **orphaned by any rewrite** (new SHA), and **not pushed/fetched by default**; they never survive PR→GitHub-squash. — git-notes(1).
- **Verdict: commit-message trailer is the primary authoritative carrier** (copied by rebase/cherry-pick/amend, aggregated by squash, GitHub already honors the mechanism for Co-Authored-By). git notes = best-effort local breadcrumb only. Durable binding is created **server-side at push** (Deploy event + `SessionMerkleLeafEntity`).

## 4. Assumptions (confirm)

- A-S3-1: Claude Code adapter can run a `prepare-commit-msg` git hook and/or stamp on `Stop`/`SessionEnd`.
- A-S3-2: `session_id` is opaque, safe in plaintext trailer (never the `obx_` key). ✓ matches architecture §5.
- A-S3-3: git action runs where it sees real pushed SHAs (CI/server hook/webhook) — push-time resolution feasible.
- A-S3-4: multiple distinct `OpenBox-Session:` values = genuine fan-in, not a dedupe target (identical duplicates are deduped).

## 5. Unknowns (verify before freezing FR-5/FR-6)

- U-1: Is the pilot repo GitHub-squash-heavy? Trailer preservation then depends on repo settings — **validate on the pilot repo (OD10).**
- U-2: Does Claude Code's own commit stamping collide/duplicate with the OpenBox trailer? Interop/idempotency test needed.
- U-3: Exact hook re-firing for cherry-pick/rebase in 2.53.0 is operation-dependent — **do not design on it** (R7).
- U-4: Adopt a stable `OpenBox-Change-Id:` (Gerrit-style) to re-link across body-replacing squash? Feasible; flag for design.

## 6. RECOMMENDED RULE SET

**Write (adapter / `prepare-commit-msg`, local):**
- **R1** Trailer is the carrier: `git interpret-trailers --if-exists=addIfDifferent --if-missing=add --trailer "OpenBox-Session=<id>"`.
- **R2** Idempotent & additive — append distinct ids, never overwrite, never duplicate identical (amend re-fires).
- **R3** Multi-session = multiple lines, order preserved (mirrors Co-Authored-By); optional `OpenBox-Session-Root:` for the fork/resume graph root, but the set of lines is the truth.
- **R4** Never put secrets in the trailer (opaque `session_id` only). ✓
- **R5** Optional local `refs/notes/openbox` mirror — explicitly best-effort, non-authoritative.
- **R6** Guard loss paths: prefer interactive **squash over fixup**; keep GitHub squash-merge body defaulted to commit messages; best-effort re-stamp on rebase completion (never depended upon).

**Resolve (git action at push — authoritative):**
- **R7** Resolve from committed data at push time, against the real pushed SHA; never trust a pre-push binding or assume a hook re-fired. `sessions = dedupe(git log -1 --format='%(trailers:key=OpenBox-Session,valueonly)' <sha>)`.
- **R8** A commit maps to a **set** of sessions (0..N); bind every session to the Deploy DID (`DID = git hash + timestamp` via existing `createHash`/`didFor`). Squash fan-in ⇒ many sessions on one commit — record all.
- **R9** Merge commits: attribute reachable original commits (walk the pushed range), not the trailer-less merge node.
- **R10** Persist each commit→session(s)→deploy binding as a `SessionMerkleLeafEntity` leaf (INV-6) for tamper-evidence.

**Multi-session:** **R11** store the full session set losslessly; apportion cost/tokens (FR-7) across the set (equal or by per-session span metrics from S1 §A4) only after recording the set.

**Unattributed:** **R12** zero trailers ⇒ still create the Deploy event with `attribution=unattributed` + `reason` enum (`no-trailer` | `trailer-stripped` | `non-agent`) — satisfies INV-6. **R13** optional low-confidence heuristic recovery (author + timestamp proximity, Anthropic's 21-day precedent) emitted as `attribution=inferred` with confidence — **never** promoted to authoritative. **R14** if a pushed commit lacks a trailer but a parent in the same push had one, mark `reason=trailer-stripped` (re-link via `OpenBox-Change-Id` if U-4 adopted).

**INV-6 satisfied:** every pushed commit → non-empty deduped session set (bound + Merkle-anchored) OR explicit `unattributed`/`inferred` marker with reason. squash=fan-in, fixup/GitHub-squash=strip-detected, fork/resume=session-set, amend/rebase/cherry-pick=preserved-under-new-SHA (resolved at push, R7).

---

**Bottom line:** commit-message trailer = single source of truth; allow multiple `OpenBox-Session:` lines (like Co-Authored-By); write idempotently in `prepare-commit-msg`; **resolve server-side at push against the real pushed SHA**. Trailers survive rewrites only by message-copy (hooks don't travel), the only loss modes are fixup/body-replacing squash, and SHAs are unstable until push. git notes are a non-authoritative local extra. All git handling is greenfield; only DID/hash/Merkle primitives are reusable.
