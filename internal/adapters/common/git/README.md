# OpenBox commit-trailer stamping (STORY-SL-5)

The **provider-independent** write side of session→commit attribution. It binds
a git commit to the OpenBox session(s) that produced it by stamping an
`OpenBox-Session:` commit-message **trailer**, exactly as spike S3 (R1–R6)
prescribes. Lives in `internal/adapters/common/` because "which session made
this commit" must not depend on any one tool (Claude Code, Codex, Cursor).

```
git commit / amend / rebase-squash
   └─ .git/hooks/prepare-commit-msg          # installed by the CLI / adapter
        └─ openbox-git-hook prepare-commit-msg <msgFile> [source] [sha]
             ├─ resolve session(s)            # env OPENBOX_SESSION / OPENBOX_SESSION_FILE
             ├─ harvest mid-body sessions     # squash healing (see below)
             └─ git interpret-trailers        # idempotent, additive (S3 R1)
        exit 0  ALWAYS                         # never abort the developer's commit
```

The durable, authoritative binding is **not** created here. It is resolved
**server-side at push against the real pushed SHA** by the SL-6 git action (S3
R7); git hooks are local and never travel (S3 §1), so this write side is
best-effort: its only job is to place the opaque session id inside the commit
object so SL-6 can resolve it later.

## Why a trailer (not git notes)

The commit-message trailer is the single authoritative carrier (S3 §3): it is
copied verbatim by rebase/cherry-pick/amend, aggregated by squash, and GitHub
already honors the same mechanism for `Co-Authored-By`. Multiple distinct
sessions → **multiple `OpenBox-Session:` lines** (genuine fan-in, mirroring
`Co-Authored-By`). Git notes are orphaned by any rewrite and not pushed by
default, so they are only an optional, non-authoritative local breadcrumb
(`refs/notes/openbox`, see `notes.go`).

## Idempotency (S3 R1/R2)

Stamping uses `git interpret-trailers --if-exists=addIfDifferent
--if-missing=add`:

- First session id creates the trailer block;
- A **distinct** id is appended as a new line (fan-in);
- An id **already present** is never duplicated; which is what makes hook
  re-fire and `git commit --amend` safe.

## Squash healing (a finding beyond S3)

S3 R7 resolves sessions via `%(trailers)`, which only parses the **trailing**
trailer block. But a squash concatenates each source message, leaving earlier
`OpenBox-Session:` lines **mid-body**, where the trailer parser cannot see them
- so naive stamping would silently lose squashed-in sessions. Before stamping,
this package therefore **harvests every `OpenBox-Session:` line from the whole
message** and re-asserts it into the trailing block (`addIfDifferent` → no
duplication). The multi-session fan-in is thus resolvable via `%(trailers)`
regardless of who ran the squash; even a human with no session of their own
heals the agent sessions they squashed together. See
`TestStamp_HealsSquashConcatenation` / `TestE2E_SquashFansInAllSessions`.

## The rewrite matrix (S3 §2, exercised against real git)

| Operation | Behavior | Test |
|---|---|---|
| plain commit | stamped | `TestE2E_PlainCommitStamps` |
| `--amend` (re-fire) | idempotent, no duplicate | `TestE2E_AmendIsIdempotent` |
| plain rebase | message copied → trailer survives | `TestE2E_PlainRebasePreservesTrailer` |
| **squash** | fan-in of all sessions (healed) | `TestE2E_SquashFansInAllSessions` |
| **fixup** | source message discarded → **session lost** (documented loss; SL-6 detects `trailer-stripped`) | `TestE2E_FixupDropsItsSession` |
| human commit (no session) | unattributed, unstamped | `TestE2E_HumanCommitIsUnattributed` |

## Safety: never fail a commit (INV-3, the git analog)

A `prepare-commit-msg` hook that exits non-zero **aborts the commit**. This
package never does that: the hook script and `openbox-git-hook` binary **always
exit 0**, and a missing engine binary degrades to a no-op commit (the hook
script guards on the binary being resolvable). A stamping failure is logged to
stderr and the commit proceeds unstamped (SL-6 records it as unattributed).

## Security (INV-1)

The trailer carries the **opaque session id only**; never a secret.
`validateSessionID` rejects empty/over-long/multi-line values (trailer-injection
safety) and anything shaped like an OpenBox API key (`obx_` prefix), so a secret
can never be written into commit history.

## How the hook learns the session id (parallel-safe)

Claude Code does **not** expose the session id to a git subprocess; no env var,
and no pid the hook could walk (confirmed against the hooks docs). So resolution
has two tiers:

1. **Explicit override**; `OPENBOX_SESSION` / `OPENBOX_SESSION_FILE`. A provider
   or CI job that *can* inject the id into the git environment (or a human) wins
   outright; one or more ids = genuine fan-in. Used by non-Claude-Code
   providers.
2. **Session registry** (the default for tools that can't inject env); the
   adapter writes a per-session liveness record `{session_id, cwd, updated_at}`
   on each of its hooks (`registry.go`), and the resolver attributes the commit
   to the **most-recently-updated session whose `cwd` is within the commit's git
   worktree**.

**Why it survives multiple concurrent sessions** (the hard case):

- Sessions in **different worktrees** never collide; the worktree filter is
  exact, so each commit resolves to its own repo's session.
- Sessions in the **same worktree** resolve by recency: a `git commit` via the
  Bash tool is always immediately preceded by that session's `PreToolUse`, which
  refreshes its record; so the committing session is the freshest. This is
  best-effort (a tight interleaving race is possible); acceptable because
  Phase-1 is observe-only and SL-6 makes the authoritative binding at push (S3
  R7).
- Stale records (a crashed session that never wrote `SessionEnd`) are ignored
  past a TTL (`OPENBOX_SESSION_TTL`, default 8h), so a much-later human commit
  is never falsely attributed.

A commit attributes to a **single** session; multi-session fan-in comes from
squash healing, not from parallel liveness.

### Who writes the registry / installs the hook

- The **Claude Code adapter** (`openbox hook claude-code <event>`)
  writes/refreshes the record on `SessionStart`/`PreToolUse`/`PostToolUse` and
  removes it on `SessionEnd`.
- **Ambient hook install** is opt-in (`OPENBOX_INSTALL_GIT_HOOK=1`): on
  `SessionStart` the adapter installs this `prepare-commit-msg` hook into the
  session's repo (idempotent, foreign-safe). It is **off by default** because it
  modifies a repo's `.git/hooks`. Per-repo manual install: `openbox-git-hook
  install`.

Per od17 (single `openbox` engine) the standalone `cmd/openbox-git-hook` binary
is folded into `openbox hook git prepare-commit-msg` in a follow-up; the hook
script's command is already parameterized (`HookConfig`) so that move needs no
change here (mirrors how SL-4 shipped `cmd/openbox-cc-hook`, later absorbed by
SL4-wire-2).

## Validate

```
go build ./internal/adapters/common/git/... && go vet ./internal/adapters/common/git/... && go test -race ./internal/adapters/common/git/...
```
