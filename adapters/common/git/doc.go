// Package git is the provider-independent write side of session->commit
// attribution. It binds a git commit to the OpenBox session(s) that
// produced it by stamping an `OpenBox-Session:` commit-message trailer.
//
// Why a trailer: the commit-message trailer is the single authoritative
// carrier. It is copied verbatim by rebase/cherry-pick/amend and
// aggregated by squash (multi-session fan-in), and GitHub already honors
// the same mechanism for Co-Authored-By. git notes are only a best-effort
// local breadcrumb (orphaned by any rewrite, not pushed by default) — see
// notes.go.
//
// Division of labor with the git action (actions/openbox-git-action):
// this package only writes the trailer, locally and idempotently, in a
// `prepare-commit-msg` hook. The durable, authoritative binding is
// resolved server-side at push against the real pushed SHA — never a
// pre-push SHA — by the git action. Because git hooks are local and
// never travel, this write side is best-effort: its only job is to put
// the opaque session id into data that lives inside the commit object so
// the git action can resolve it later.
//
// Security — the trailer is an untrusted claim, not proof. Anyone who can
// author a commit can write (or, via squash healing, hoist from the
// message body) an `OpenBox-Session:` line naming any session id —
// including a victim's, which is visible in that victim's already-pushed
// commits. This write side cannot prevent that; it only records the
// claim. The authoritative binding is made server-side at push by the
// git action, which must treat each trailer value as a claim to verify:
// bind a value only to a session owned by the authenticated pusher, and
// mark others unattributed/inferred. Never treat a raw trailer as proven
// attribution.
//
// Invariants:
//   - INV-1: the trailer carries only the opaque session id, never a
//     secret (the obx_ key / Ed25519 seed). See validateSessionID.
//   - INV-6 (write side): multiple distinct sessions => multiple trailer
//     lines (genuine fan-in, mirroring Co-Authored-By); idempotent under
//     re-fire/`--amend` (identical id never duplicated).
//
// Observe-only safety (mirrors the adapters' INV-3 exit-0 contract,
// applied to git): a `prepare-commit-msg` hook that exits non-zero aborts
// the developer's commit. This package therefore never fails a commit —
// every hook path exits 0. A stamping failure logs to stderr and leaves
// the commit unstamped (the git action then marks it unattributed); it
// must never break `git commit`.
package git
