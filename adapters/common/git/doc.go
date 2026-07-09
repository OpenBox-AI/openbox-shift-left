// Package git is the provider-INDEPENDENT write side of session->commit
// attribution (STORY-SL-5). It binds a git commit to the OpenBox session(s)
// that produced it by stamping an `OpenBox-Session:` commit-message trailer,
// exactly as spike S3 (R1-R6) prescribes.
//
// Why a trailer (S3 §3): the commit-message trailer is the single authoritative
// carrier. It is copied verbatim by rebase/cherry-pick/amend and aggregated by
// squash (multi-session fan-in), and GitHub already honors the same mechanism
// for Co-Authored-By. git notes are only a best-effort local breadcrumb
// (orphaned by any rewrite, not pushed by default) — see notes.go.
//
// Division of labor with SL-6 (the git action): this package only WRITES the
// trailer, locally and idempotently, in a `prepare-commit-msg` hook. The
// durable, authoritative binding is resolved SERVER-SIDE at push against the
// real pushed SHA (S3 R7) — never a pre-push SHA — by the SL-6 git action.
// Because git hooks are local and never travel (S3 §1), this write side is
// best-effort: its only job is to put the opaque session id into data that
// lives inside the commit object so SL-6 can resolve it later.
//
// SECURITY — the trailer is an UNTRUSTED CLAIM, not proof (SL5-SEC-1). Anyone
// who can author a commit can write (or, via squash healing, hoist from the
// message body) an `OpenBox-Session:` line naming ANY session id — including a
// victim's, which is visible in that victim's already-pushed commits. This write
// side cannot prevent that; it only records the claim. The authoritative binding
// is made SERVER-SIDE at push by SL-6, which MUST treat each trailer value as a
// claim to verify: bind a value only to a session owned by the authenticated
// pusher, and mark others unattributed/inferred (mirrors how SL-3 cross-binds
// the DID). Never treat a raw trailer as proven attribution.
//
// Invariants:
//   - INV-1: the trailer carries ONLY the opaque session id, never a secret
//     (the obx_ key / Ed25519 seed). See validateSessionID.
//   - INV-6 (write side): multiple distinct sessions => multiple trailer lines
//     (genuine fan-in, mirroring Co-Authored-By, S3 R3); idempotent under
//     re-fire/`--amend` (identical id never duplicated, S3 R2).
//
// Observe-only safety (mirrors SL-4's INV-3 exit-0 contract, applied to git):
// a `prepare-commit-msg` hook that exits non-zero ABORTS the developer's
// commit. This package therefore NEVER fails a commit — every hook path exits
// 0. A stamping failure logs to stderr and leaves the commit unstamped (SL-6
// then marks it unattributed); it must never break `git commit`.
package git
