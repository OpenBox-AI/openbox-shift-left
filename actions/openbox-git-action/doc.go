// Package gitaction is the OpenBox git action: the server-side, push-time
// resolver that binds a pushed commit to the OpenBox session(s) that
// produced it and registers a Deploy governance event provably linked to
// those sessions.
//
// It is the read/resolve counterpart to the write side in
// adapters/common/git. That package's prepare-commit-msg hook writes an
// `OpenBox-Session:` trailer into the commit object locally; this action
// reads it back at push, against the real pushed SHA (never a pre-push SHA
// — git hooks are local and SHAs are unstable until push), dedups to a
// session set, and emits a Deploy event through the shared client
// (client.Emit).
//
// # Resolution model (INV-6)
//
// A pushed commit resolves to exactly one of:
//
//   - Attributed  — >=1 session verified as owned by the authenticated pusher.
//   - Inferred    — session id(s) recovered (from the trailer block, a mid-body
//     OpenBox-Session line, or the git-notes mirror) but not verified against
//     the pusher. A claim, not proof (see Security below).
//   - Unattributed — no session id resolved; carries a Reason
//     (no-trailer | trailer-stripped | non-agent).
//
// It never emits a silent wrong attribution: an unverifiable or missing
// binding is always marked, with a reason, in the Deploy event metadata.
//
// # Scope walking
//
// A single non-merge commit resolves from its own trailers. A push range
// (base..target) or a merge commit resolves the union over the commits it
// introduces (merge commits attribute reachable originals): for a merge
// with no explicit base we walk `rev-list <merge> --not <merge>^1` (the
// merged-in topic commits plus the merge itself). No silent caps: if the
// walk hits MaxCommits the Resolution records how many of how many were
// walked.
//
// # Squash defense-in-depth
//
// The authoritative read is the trailing trailer block via
// `%(trailers:key=OpenBox-Session,...)`. But a squash performed before the
// commit hook was installed leaves earlier `OpenBox-Session:` lines
// mid-body, where the trailer parser cannot see them. This action
// therefore also full-body-scans each commit for column-0
// `OpenBox-Session:` lines and folds any not already in the trailer block
// into the set as Source "body-scan" (inferred). This recovers fan-in from
// pre-install squashes that the in-hook healing could not reach.
//
// # Security — the trailer is an untrusted claim
//
// Anyone who can author a commit can write (or, via squash healing, hoist
// from the message body) an `OpenBox-Session:` line naming any session id,
// including a victim's — victim session ids are visible in that victim's
// already-pushed commits. A raw trailer is therefore a claim, never proof.
// Each resolved session id is passed through an OwnershipVerifier that must
// confirm the id belongs to a session owned by the authenticated pusher
// before it is marked Verified (→ Attributed). Unverified ids stay Inferred
// and are flagged verified=false in the Deploy metadata; they are recorded,
// never trusted as attribution.
//
// The default NoopVerifier verifies nothing, so a well-formed deploy
// resolves as Inferred with every claim flagged verified=false — honest
// about what is proven.
//
// The real verifier (apiVerifier, verifier.go) reads openbox-backend's
// existing, org-scoped endpoint GET /agent/<agentID>/sessions?search=<id>
// with an org X-API-Key, and promotes a claim to Attributed only when a
// returned session's run_id matches the trailer value — fail-closed on
// every fault. The deploy agent's UUID is supplied directly
// (OPENBOX_AGENT_ID) and bound to its DID via uuidv5 at startup (INV-4: the
// read can only ever see the deploy principal's own sessions). It is off by
// default (OPENBOX_OWNERSHIP_VERIFY=1 opts in); flipping it on promotes
// owned sessions with zero change to this resolver — owned ids simply
// become Verified.
//
// # Emission
//
// The Deploy event is emitted via the shared client with
// EventType=client.EventDeploy, serialized onto the base wire model's
// SignalReceived (signal_name="deploy", ADR-0004) with the resolved session
// set carried in `metadata` — no accept-list dependency. The Deploy DID
// (`did:aip:deploy-<shortsha>-<unixts>`) is a synthetic lineage label
// carried in metadata — openbox-core has no deploy-DID primitive; the
// client's signing identity remains the git-action agent's real
// `did:aip:<uuid>`.
package gitaction
