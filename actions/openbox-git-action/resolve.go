package gitaction

import (
	"context"
	"fmt"
	"strings"

	obgit "github.com/openbox-ai/openbox-shift-left/adapters/common/git"
)

// DefaultMaxCommits bounds how many commits a single push resolution walks. A
// push range or a large merge is walked in full up to this cap; if the cap is
// hit the Resolution records exactly how many of how many were walked (no silent
// truncation — a governance product must never look like it covered everything).
const DefaultMaxCommits = 2000

// DefaultMaxSessions bounds how many distinct session claims a single
// resolution accumulates (SEC-6-1). A hostile committer could otherwise author
// one commit whose body contains an unbounded number of distinct
// `OpenBox-Session:` lines, inflating memory and the egress payload. Far above
// any real fan-in; a hit is disclosed in the Resolution note, never silent.
const DefaultMaxSessions = 4096

// Source is where a resolved session id came from, in descending trust order.
type Source string

const (
	// SourceTrailer is the authoritative trailing trailer block (S3 R7).
	SourceTrailer Source = "trailer"
	// SourceBodyScan is a mid-body OpenBox-Session line recovered by SL6-SCAN
	// (a pre-install squash left it where the trailer parser can't see it).
	SourceBodyScan Source = "body-scan"
	// SourceNote is the git-notes mirror (refs/notes/openbox) — recovered only
	// when the commit's own trailer is gone, i.e. the trailer was stripped.
	SourceNote Source = "note-mirror"
)

// Status is the INV-6 attribution outcome for a deploy.
type Status string

const (
	// StatusAttributed: >=1 session VERIFIED as owned by the authenticated pusher.
	StatusAttributed Status = "attributed"
	// StatusInferred: session id(s) recovered but not verified — a claim, not proof.
	StatusInferred Status = "inferred"
	// StatusUnattributed: no session id resolved.
	StatusUnattributed Status = "unattributed"
)

// Reason explains a non-attributed outcome (INV-6's required marker reason).
type Reason string

const (
	ReasonNoTrailer       Reason = "no-trailer"       // no OpenBox-Session anywhere in scope
	ReasonTrailerStripped Reason = "trailer-stripped" // recovered from the notes mirror; trailer gone

	// ReasonNonAgent is a RESERVED reason value, declared for contract
	// completeness (the story enumerates it) but NOT produced server-side: a
	// bare git commit cannot tell "a human with no agent session wrote this"
	// apart from "the trailer is absent" — both surface as ReasonNoTrailer. It
	// becomes producible only when an external author-identity signal is wired
	// (e.g. a verifier that knows the pusher is a non-agent principal).
	ReasonNonAgent Reason = "non-agent"
)

// SessionClaim is one resolved session id and how much we trust it.
type SessionClaim struct {
	SessionID string `json:"session_id"`
	Source    Source `json:"source"`
	Commit    string `json:"commit"`           // the commit the id was resolved from
	Verified  bool   `json:"verified"`         // owned by the authenticated pusher (SL5-SEC-1)
	Reason    string `json:"reason,omitempty"` // verification note when not Verified
}

// Resolution is the full server-side attribution of a pushed commit (INV-6).
type Resolution struct {
	CommitSHA   string         `json:"commit_sha"`   // the real, verified pushed SHA
	Scope       []string       `json:"scope"`        // commits considered (newest first)
	ScopeWalked int            `json:"scope_walked"` // commits actually walked (== len(Scope))
	ScopeTotal  int            `json:"scope_total"`  // commits in range before any MaxCommits cap
	Sessions    []SessionClaim `json:"sessions"`     // deduped, order-stable
	Status      Status         `json:"status"`
	Reason      Reason         `json:"reason,omitempty"`
	Note        string         `json:"note,omitempty"` // honest human-readable detail
}

// SessionIDs returns just the resolved session ids, order-stable.
func (r Resolution) SessionIDs() []string {
	ids := make([]string, 0, len(r.Sessions))
	for _, s := range r.Sessions {
		ids = append(ids, s.SessionID)
	}
	return ids
}

// Resolver turns a pushed rev into a Resolution.
type Resolver struct {
	Repo        Repo
	Verifier    OwnershipVerifier // SL5-SEC-1; nil => NoopVerifier (verifies nothing)
	MaxCommits  int               // 0 => DefaultMaxCommits
	MaxSessions int               // 0 => DefaultMaxSessions
	// notes reads the SL-5 git-notes mirror (refs/notes/openbox) for
	// trailer-stripped recovery. Reuses the SL-5 reader by construction.
	notes obgit.Git
}

// NewResolver builds a Resolver over a repository directory.
func NewResolver(dir string, v OwnershipVerifier) *Resolver {
	if v == nil {
		v = NoopVerifier{}
	}
	return &Resolver{
		Repo:     Repo{Dir: dir},
		Verifier: v,
		notes:    obgit.Git{Dir: dir},
	}
}

func (r *Resolver) maxCommits() int {
	if r.MaxCommits > 0 {
		return r.MaxCommits
	}
	return DefaultMaxCommits
}

func (r *Resolver) maxSessions() int {
	if r.MaxSessions > 0 {
		return r.MaxSessions
	}
	return DefaultMaxSessions
}

func (r *Resolver) verifier() OwnershipVerifier {
	if r.Verifier != nil {
		return r.Verifier
	}
	return NoopVerifier{}
}

// Resolve resolves the session set for a pushed commit. target is the pushed rev
// (resolved to the real pushed SHA, INV-6). base is optional: when set, the
// scope is base..target; when empty, a merge target resolves its introduced
// commits and any other commit resolves itself. A bad/unknown target is a
// precondition error the caller must fix (not a fail-open drop).
func (r *Resolver) Resolve(ctx context.Context, target, base string) (Resolution, error) {
	sha, err := r.Repo.verifyCommit(target)
	if err != nil {
		return Resolution{}, fmt.Errorf("resolve target: %w", err)
	}

	scope, total, note, err := r.scope(sha, base)
	if err != nil {
		return Resolution{}, err
	}

	res := Resolution{
		CommitSHA:   sha,
		Scope:       scope,
		ScopeWalked: len(scope),
		ScopeTotal:  total,
		Note:        note,
	}

	// Gather claims across the whole scope: trailers (authoritative) first, then
	// body-scan (SL6-SCAN), then a per-commit notes-mirror fallback for any
	// commit that yielded neither. Deduped, order-stable; a trailer source is
	// never downgraded by a later body-scan hit for the same id, ANYWHERE in the
	// scope (C2). Bounded by MaxSessions (SEC-6-1).
	claims, capped, truncated, err := r.gatherClaims(scope)
	if err != nil {
		return Resolution{}, err
	}
	if capped {
		res.Note = appendNote(res.Note, fmt.Sprintf(
			"session set capped at MaxSessions=%d; additional distinct claims dropped", r.maxSessions()))
	}
	if truncated {
		res.Note = appendNote(res.Note,
			"one or more commit messages exceeded the read bound and were truncated; some claims may be missing")
	}

	// SL5-SEC-1: bind each claim to the authenticated pusher. Only positively
	// owned ids become Verified; the rest stay claims.
	r.verify(ctx, claims)
	res.Sessions = claims

	r.classify(&res)
	return res, nil
}

// scope computes the commits to consider, the total before any MaxCommits cap,
// and a human note describing how. The rev-list reads are bounded to
// maxCommits+1 (SEC-6-1) so a huge range is never buffered whole; a cap is
// disclosed in the note (never silent).
func (r *Resolver) scope(sha, base string) (commits []string, total int, note string, err error) {
	limit := r.maxCommits() + 1
	cap := func(list []string, how string) ([]string, int, string, error) {
		total := len(list)
		if total > r.maxCommits() {
			list = list[:r.maxCommits()]
			how = appendNote(how, fmt.Sprintf(
				"scope capped: walked %d of at least %d commits (MaxCommits=%d); earlier commits not resolved",
				len(list), total, r.maxCommits()))
		}
		return list, total, how, nil
	}

	if strings.TrimSpace(base) != "" {
		baseSHA, verr := r.Repo.verifyCommit(base)
		if verr != nil {
			return nil, 0, "", fmt.Errorf("resolve base: %w", verr)
		}
		rng, rerr := r.Repo.rangeCommits(baseSHA, sha, limit)
		if rerr != nil {
			return nil, 0, "", rerr
		}
		if len(rng) == 0 {
			// base == target, or target is an ancestor of base: nothing in the
			// range. Resolve the tip itself so a re-push of the same SHA still
			// attributes rather than silently returning empty.
			return []string{sha}, 1, "empty base..target range; resolved tip only", nil
		}
		return cap(rng, fmt.Sprintf("range %s..%s", short(baseSHA), short(sha)))
	}

	parents, perr := r.Repo.parents(sha)
	if perr != nil {
		return nil, 0, "", perr
	}
	if len(parents) > 1 {
		intro, ierr := r.Repo.mergeIntroduced(sha, limit)
		if ierr != nil {
			return nil, 0, "", ierr
		}
		if len(intro) == 0 {
			intro = []string{sha}
		}
		return cap(intro, "merge commit; attributing reachable original(s)")
	}
	return []string{sha}, 1, "", nil
}

// gatherClaims collects session ids across the whole scope in trust order:
//
//	pass 1 — authoritative trailer blocks (S3 R7), across ALL commits;
//	pass 2 — body-scan recovery (SL6-SCAN), skipping ids already trailer-claimed;
//	pass 3 — per-commit notes-mirror recovery, but ONLY for commits that yielded
//	         nothing in passes 1-2 (C3: a trailer-stripped sibling in a mixed
//	         range is recovered, not silently dropped).
//
// Deduped by id (first occurrence wins) and order-stable. Running the trailer
// pass over the entire scope BEFORE the body pass guarantees an id that appears
// as a proper trailer anywhere is credited as SourceTrailer, never mislabeled
// SourceBodyScan by a later commit (C2). Bounded by MaxSessions (SEC-6-1):
// capped reports whether distinct claims were dropped; truncated whether a
// commit message was cut by the read bound.
func (r *Resolver) gatherClaims(scope []string) (claims []SessionClaim, capped, truncated bool, err error) {
	seen := map[string]bool{}
	contributed := map[string]bool{} // commit -> had >=1 valid trailer/body id (pre-dedupe)
	max := r.maxSessions()
	add := func(id, commit string, src Source) {
		if seen[id] {
			return
		}
		if len(claims) >= max {
			capped = true
			return
		}
		seen[id] = true
		claims = append(claims, SessionClaim{SessionID: id, Source: src, Commit: commit})
	}

	// Pass 1: authoritative trailer blocks across the whole scope.
	blockByCommit := make(map[string]map[string]bool, len(scope))
	for _, c := range scope {
		block, tr, berr := r.Repo.trailerBlockSessions(c)
		if berr != nil {
			return nil, false, false, berr
		}
		truncated = truncated || tr
		inBlock := map[string]bool{}
		for _, id := range block {
			if validateSessionID(id) != nil {
				continue
			}
			inBlock[id] = true
			contributed[c] = true
			add(id, c, SourceTrailer)
		}
		blockByCommit[c] = inBlock
	}

	// Pass 2: body-scan recovery, skipping ids already in this commit's block.
	for _, c := range scope {
		body, tr, berr := r.Repo.bodySessions(c)
		if berr != nil {
			return nil, false, false, berr
		}
		truncated = truncated || tr
		for _, id := range body {
			if validateSessionID(id) != nil || blockByCommit[c][id] {
				continue
			}
			contributed[c] = true
			add(id, c, SourceBodyScan)
		}
	}

	// Pass 3: per-commit notes-mirror recovery for commits that yielded nothing.
	for _, c := range scope {
		if contributed[c] {
			continue
		}
		ids, nerr := r.notes.ReadNoteMirror(c)
		if nerr != nil {
			continue // a missing note is the normal case
		}
		for _, id := range ids {
			if validateSessionID(id) != nil {
				continue
			}
			add(id, c, SourceNote)
		}
	}
	return claims, capped, truncated, nil
}

// verify runs the ownership check for each claim (SL5-SEC-1). A lookup error is
// treated as "not verified" — never over-attribute on a failure.
func (r *Resolver) verify(ctx context.Context, claims []SessionClaim) {
	v := r.verifier()
	for i := range claims {
		owned, err := v.OwnsSession(ctx, claims[i].SessionID)
		if err != nil {
			claims[i].Verified = false
			claims[i].Reason = "ownership lookup failed; treated as unverified"
			continue
		}
		claims[i].Verified = owned
		if !owned {
			claims[i].Reason = "unverified claim (not bound to the authenticated pusher)"
		}
	}
}

// classify sets Status/Reason from the resolved, verified claims (INV-6). The
// reason for an inferred outcome is derived from the claim sources: an all-notes
// recovery means the trailer was stripped; a mix (or unverified trailers) is
// left reason-free with the detail in Note (none of the three enum reasons fit
// "found but not ownership-verified"; see P3 in the SL-6 review).
func (r *Resolver) classify(res *Resolution) {
	if len(res.Sessions) == 0 {
		res.Status = StatusUnattributed
		res.Reason = ReasonNoTrailer
		res.Note = appendNote(res.Note,
			"no OpenBox-Session trailer, body line, or note mirror in scope")
		return
	}
	verified := 0
	for _, s := range res.Sessions {
		if s.Verified {
			verified++
		}
	}
	if verified > 0 {
		res.Status = StatusAttributed
		if verified < len(res.Sessions) {
			res.Note = appendNote(res.Note, fmt.Sprintf(
				"%d of %d session(s) verified as owned by the pusher; the rest remain unverified claims",
				verified, len(res.Sessions)))
		}
		return
	}
	// Recovered but nothing verified.
	res.Status = StatusInferred
	if allFromNotes(res.Sessions) {
		res.Reason = ReasonTrailerStripped
		res.Note = appendNote(res.Note,
			"recovered from the git-notes mirror; commit trailer absent (likely a history rewrite)")
		return
	}
	res.Note = appendNote(res.Note, fmt.Sprintf(
		"%d unverified session claim(s); ownership not verified (SL5-SEC-1; ownership API deferred/EXT)",
		len(res.Sessions)))
}

// allFromNotes reports whether every claim was recovered from the notes mirror
// (a pure trailer-stripped outcome). A single trailer/body claim among them
// means the trailer was not (wholly) stripped, so the reason does not apply.
func allFromNotes(claims []SessionClaim) bool {
	for _, c := range claims {
		if c.Source != SourceNote {
			return false
		}
	}
	return len(claims) > 0
}

// validateSessionID enforces that a resolved id is opaque, single-line, and not
// secret-shaped before it can enter a governance record. This is the read-side
// mirror of SL-5's validateSessionID: the write side stops bad ids being
// STAMPED; the read side stops a hostile commit's bad id being RESOLVED and
// attributed. Untrusted input, so the same rules apply.
func validateSessionID(id string) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("empty session id")
	}
	if len(id) > maxSessionIDLen {
		return fmt.Errorf("session id too long (%d > %d)", len(id), maxSessionIDLen)
	}
	if strings.ContainsAny(id, "\n\r\x00") {
		return fmt.Errorf("session id contains a line break or NUL")
	}
	if strings.ContainsAny(id, " \t\v\f") {
		return fmt.Errorf("session id contains whitespace")
	}
	if strings.HasPrefix(id, "obx_") {
		return fmt.Errorf("refusing to resolve a secret-shaped value (obx_ prefix)")
	}
	return nil
}

// maxSessionIDLen mirrors SL-5's bound (UUIDs are 36 chars; anything far larger
// is malformed, never resolved).
const maxSessionIDLen = 512

func appendNote(existing, add string) string {
	if existing == "" {
		return add
	}
	return existing + "; " + add
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
