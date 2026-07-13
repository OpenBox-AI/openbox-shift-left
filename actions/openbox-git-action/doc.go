// Package gitaction is the OpenBox git action (STORY-SL-6): the SERVER-SIDE,
// push-time resolver that binds a pushed commit to the OpenBox session(s) that
// produced it and registers a Deploy governance event provably linked to those
// sessions (PRD FR-6; spike S3 R7-R14; architecture INV-6).
//
// It is the read/resolve counterpart to the write side in
// adapters/common/git (STORY-SL-5). SL-5's prepare-commit-msg hook WRITES an
// `OpenBox-Session:` trailer into the commit object locally; this action READS
// it back at push, against the REAL pushed SHA (never a pre-push SHA — git hooks
// are local and SHAs are unstable until push, S3 §1), dedups to a session set,
// and emits a Deploy event through the shared SL-3 client (client.Emit).
//
// # Resolution model (INV-6)
//
// A pushed commit resolves to exactly one of:
//
//   - Attributed  — >=1 session verified as OWNED BY the authenticated pusher.
//   - Inferred    — session id(s) recovered (from the trailer block, a mid-body
//     OpenBox-Session line, or the git-notes mirror) but NOT verified against
//     the pusher. A claim, not proof (see SECURITY below).
//   - Unattributed — no session id resolved; carries a Reason
//     (no-trailer | trailer-stripped | non-agent).
//
// It NEVER emits a silent wrong attribution: an unverifiable or missing binding
// is always marked, with a reason, in the Deploy event metadata (INV-6).
//
// # Scope walking
//
// A single non-merge commit resolves from its own trailers. A push RANGE
// (base..target) or a MERGE commit resolves the union over the commits it
// introduces ("merge commits attribute reachable originals", per the story):
// for a merge with no explicit base we walk `rev-list <merge> --not <merge>^1`
// (the merged-in topic commits plus the merge itself). No silent caps: if the
// walk hits MaxCommits the Resolution records how many of how many were walked.
//
// # SL6-SCAN (squash defense-in-depth)
//
// The authoritative read is the trailing trailer block via
// `%(trailers:key=OpenBox-Session,...)` (S3 R7, the exact command SL-5's tests
// assert). But a squash performed BEFORE SL-5's hook was installed leaves
// earlier `OpenBox-Session:` lines mid-body, where the trailer parser cannot see
// them. This action therefore ALSO full-body-scans each commit for column-0
// `OpenBox-Session:` lines and folds any not already in the trailer block into
// the set as Source "body-scan" (inferred). This recovers fan-in from
// pre-install squashes that SL-5's in-hook healing could not reach.
//
// # SECURITY — the trailer is an UNTRUSTED CLAIM (SL5-SEC-1)
//
// Anyone who can author a commit can write (or, via squash healing, hoist from
// the message body) an `OpenBox-Session:` line naming ANY session id, including
// a victim's — victim session ids are visible in that victim's already-pushed
// commits. A raw trailer is therefore a CLAIM, never proof. This action mirrors
// how SL-3 cross-binds the DID: each resolved session id is passed through an
// OwnershipVerifier that must confirm the id belongs to a session owned by the
// AUTHENTICATED PUSHER before it is marked Verified (→ Attributed). Unverified
// ids stay Inferred and are flagged verified=false in the Deploy metadata; they
// are recorded, never trusted as attribution.
//
// Phase-1 default: the NoopVerifier verifies nothing, so a well-formed deploy
// resolves as Inferred with every claim flagged verified=false — honest about
// what is proven.
//
// STORY-SL-15 wires the real verifier (apiVerifier, verifier.go). OD-OWNER-API
// (brian, 2026-07-13): it reads openbox-backend's existing, org-scoped endpoint
// GET /agent/<agentID>/sessions?search=<id> with an org X-API-Key, and promotes a
// claim to Attributed only when a returned session's run_id matches the trailer
// value — fail-closed on every fault. The deploy agent's UUID is supplied
// directly (OPENBOX_AGENT_ID) and bound to its DID via uuidv5 at startup (INV-4:
// the read can only ever see the deploy principal's own sessions). It is OFF by
// default (OPENBOX_OWNERSHIP_VERIFY=1 opts in); flipping it on promotes owned
// sessions with zero change to this resolver — owned ids simply become Verified.
//
// # Emission and the EXT-core dependency
//
// The Deploy event is emitted via the shared SL-3 client with
// EventType=client.EventDeploy and the resolved session set carried in
// `metadata` (the S6 §4 metadata-JSONB stopgap — no external schema needed to
// WRITE it; the queryable session->commit->deploy JOIN is FR-7, external and
// deferred). The Deploy DID (`did:aip:deploy-<shortsha>-<unixts>`) is a
// synthetic lineage label carried in metadata — openbox-core has no deploy-DID
// primitive (verified cross-repo); the client's SIGNING identity remains the
// git-action agent's real `did:aip:<uuid>`.
//
// Like SL-3/SL-4/SL-5, end-to-end ingestion is gated on EXT-core: openbox-core's
// `/evaluate` accept-list (isValidGovernanceEventType) does not yet include the
// developer-runtime types, so it currently answers `Deploy` with HTTP 400. That
// is the documented additive core extension (architecture D4 / INV-8), assumed
// satisfied. Until it lands the fail-open client (INV-3) logs and drops the 400
// — the action never breaks CI over it.
package gitaction
