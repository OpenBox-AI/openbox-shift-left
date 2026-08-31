package gitaction

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	obgit "github.com/openbox-ai/openbox-shift-left/internal/adapters/common/git"
)

// DefaultMaxCommits bounds how many commits a single push resolution walks.
const DefaultMaxCommits = 2000

// DefaultMaxSessions bounds how many distinct session claims a single
// resolution accumulates. Far above any real fan-in; a hit is disclosed in the
// Resolution note, never silent.
const DefaultMaxSessions = 4096

// Source is where a resolved session id came from, in descending trust order.
type Source string

const (
	// SourceTrailer is the authoritative trailing trailer block.
	SourceTrailer Source = "trailer"
	// SourceBodyScan is a mid-body OpenBox-Session line recovered by the body
	// scan (a pre-install squash left it where the trailer parser can't see it).
	SourceBodyScan Source = "body-scan"
	// SourceNote is the git-notes mirror (refs/notes/openbox); recovered only
	// when the commit's own trailer is gone, i.e. The trailer was stripped.
	SourceNote Source = "note-mirror"
)

// Status is the INV-6 attribution outcome for a deploy.
type Status string

const (
	// StatusAttributed: >=1 session verified as owned by the authenticated
	// pusher.
	StatusAttributed Status = "attributed"
	// StatusInferred: session id(s) recovered but not verified; a claim, not
	// proof.
	StatusInferred Status = "inferred"
	// StatusUnattributed: no session id resolved.
	StatusUnattributed Status = "unattributed"
)

// Reason explains a non-attributed outcome (INV-6's required marker reason).
type Reason string

const (
	ReasonNoTrailer       Reason = "no-trailer"       // no OpenBox-Session anywhere in scope
	ReasonTrailerStripped Reason = "trailer-stripped" // recovered from the notes mirror; trailer gone

	// ReasonNonAgent is a reserved reason value, declared for contract
	// completeness (the story enumerates it) but NOT produced server-side: a bare
	// git commit cannot tell "a human with no agent session wrote this" apart
	// from "the trailer is absent"; both surface as ReasonNoTrailer.
	ReasonNonAgent Reason = "non-agent"
)

// SessionClaim is one resolved session id and how much we trust it.
type SessionClaim struct {
	SessionID string `json:"session_id"`
	Source    Source `json:"source"`
	Commit    string `json:"commit"`           // the commit the id was resolved from
	Verified  bool   `json:"verified"`         // owned by the authenticated pusher
	Reason    string `json:"reason,omitempty"` // verification note when not Verified
	// Attestation, when present, is the signed statement read from the commit's
	// git note (E8-S10): the session keyholder asserting that this exact commit
	// came from this session, together with the policy that was in force. It is
	// carried verbatim and is deliberately not verified here.
	Attestation *obgit.Attestation `json:"attestation,omitempty"`
}

// Resolution is the full server-side attribution of a pushed commit (INV-6).
type Resolution struct {
	CommitSHA   string   `json:"commit_sha"`   // the real, verified pushed SHA
	Scope       []string `json:"scope"`        // commits considered (newest first)
	ScopeWalked int      `json:"scope_walked"` // commits actually walked (== len(Scope))
	// ScopeTotal is how many commits the range holds, counted up to the walk cap
	// plus one. Past the cap it is a lower bound, not the true total; the
	// resolver deliberately does not walk further to find out.
	ScopeTotal int            `json:"scope_total"`
	Sessions   []SessionClaim `json:"sessions"` // deduped, order-stable
	Status     Status         `json:"status"`
	Reason     Reason         `json:"reason,omitempty"`
	Note       string         `json:"note,omitempty"` // honest human-readable detail
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
	Verifier    OwnershipVerifier // nil => NoopVerifier (verifies nothing)
	MaxCommits  int               // 0 => DefaultMaxCommits
	MaxSessions int               // 0 => DefaultMaxSessions
	notes       obgit.Git
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

// Resolve resolves the session set for a pushed commit. Target is the pushed
// rev (resolved to the real pushed SHA, INV-6). Base is optional: when set,
// the scope is base..target; when empty, a merge target resolves its
// introduced commits and any other commit resolves itself.
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

	// Deduped, order-stable; a trailer source is never downgraded by a later
	// body-scan hit for the same id, anywhere in the scope.
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

	r.verify(ctx, claims)
	r.attachAttestations(claims)
	res.Sessions = claims

	r.classify(&res)
	return res, nil
}

// scope the rev-list reads are bounded to maxCommits+1 so a huge range is
// never buffered whole; a cap is disclosed in the note (never silent).
func (r *Resolver) scope(sha, base string) (commits []string, total int, note string, err error) {
	limit := r.maxCommits() + 1
	capAt := func(list []string, how string) ([]string, int, string, error) {
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
			// Resolve the tip itself so a re-push of the same SHA still attributes
			// rather than silently returning empty.
			return []string{sha}, 1, "empty base..target range; resolved tip only", nil
		}
		return capAt(rng, fmt.Sprintf("range %s..%s", short(baseSHA), short(sha)))
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
		return capAt(intro, "merge commit; attributing reachable original(s)")
	}
	return []string{sha}, 1, "", nil
}

// gatherClaims running the trailer pass over the entire scope before the body
// pass guarantees an id that appears as a proper trailer anywhere is credited
// as SourceTrailer, never mislabeled SourceBodyScan by a later commit.
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

	blockByCommit := make(map[string]map[string]bool, len(scope))
	for _, c := range scope {
		block, tr, berr := r.Repo.trailerBlockSessions(c)
		if berr != nil {
			return nil, false, false, berr
		}
		truncated = truncated || tr
		inBlock := map[string]bool{}
		for _, id := range block {
			if obgit.ValidateSessionID(id) != nil {
				continue
			}
			inBlock[id] = true
			contributed[c] = true
			add(id, c, SourceTrailer)
		}
		blockByCommit[c] = inBlock
	}

	for _, c := range scope {
		body, tr, berr := r.Repo.bodySessions(c)
		if berr != nil {
			return nil, false, false, berr
		}
		truncated = truncated || tr
		for _, id := range body {
			if obgit.ValidateSessionID(id) != nil || blockByCommit[c][id] {
				continue
			}
			contributed[c] = true
			add(id, c, SourceBodyScan)
		}
	}

	for _, c := range scope {
		if contributed[c] {
			continue
		}
		ids, nerr := r.notes.ReadNoteMirror(c)
		if nerr != nil {
			continue // a missing note is the normal case
		}
		for _, id := range ids {
			if obgit.ValidateSessionID(id) != nil {
				continue
			}
			add(id, c, SourceNote)
		}
	}
	return claims, capped, truncated, nil
}

// verify a lookup error is treated as "not verified"; never over-attribute on
// a failure.
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
	res.Status = StatusInferred
	if allFromNotes(res.Sessions) {
		res.Reason = ReasonTrailerStripped
		res.Note = appendNote(res.Note,
			"recovered from the git-notes mirror; commit trailer absent (likely a history rewrite)")
		return
	}
	res.Note = appendNote(res.Note, fmt.Sprintf(
		"%d unverified session claim(s); ownership not verified", len(res.Sessions)))
}

func allFromNotes(claims []SessionClaim) bool {
	for _, c := range claims {
		if c.Source != SourceNote {
			return false
		}
	}
	return len(claims) > 0
}

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

// attachAttestations best-effort throughout: a missing note is the common
// case, and a malformed one is skipped rather than failing the deploy;
// telemetry and lineage must never break a release.
func (r *Resolver) attachAttestations(claims []SessionClaim) {
	seen := map[string]*obgit.Attestation{}
	for i := range claims {
		commit := claims[i].Commit
		if commit == "" {
			continue
		}
		att, cached := seen[commit]
		if !cached {
			att, _ = r.notes.ReadAttestation(commit) // nil on absent/malformed
			if !withinAttestationSizeLimit(att) {
				att = nil // oversized reads as absent (see maxAttestationBytes)
			}
			seen[commit] = att
		}
		if att == nil {
			continue
		}
		payload, err := att.Payload()
		if err != nil || payload.CommitSHA != commit {
			continue
		}
		if !slices.Contains(payload.SessionIDs, claims[i].SessionID) {
			continue
		}
		claims[i].Attestation = att
	}
}

const maxAttestationBytes = 32 << 10

func withinAttestationSizeLimit(a *obgit.Attestation) bool {
	if a == nil {
		return false
	}
	b, err := json.Marshal(a)
	return err == nil && len(b) <= maxAttestationBytes
}
