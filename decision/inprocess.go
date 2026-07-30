package decision

import (
	"context"
	"crypto/ed25519"
	"time"

	"github.com/openbox-ai/openbox-shift-left/client"
)

// Decider obtains a pre-execution governance decision for a tool call. There
// is one implementation — InProcessDecider — kept behind an interface so the
// enforce hook depends on a seam, not a concrete type (and tests can
// substitute a fake). It never returns an error: every fault path yields a
// fail-open allow, so an infra failure never blocks the developer (INV-3b).
type Decider interface {
	Decide(ctx context.Context, req DecisionRequest) Decision
}

var _ Decider = (*InProcessDecider)(nil)

// Decision is the enforce hook's result: the local Evaluation plus whether it
// came from a real evaluated verdict or a fail-open fallback.
//
// FailOpen==true means no real verdict was obtained (no bundle synced yet,
// missing session, bad protocol) and the Evaluation is a synthesized allow —
// the caller treats it as "proceed, degrade to observe" under fail-open, or
// denies under the opt-in fail-closed policy. Only a resident-evaluator
// verdict (Source==sourceLocalBundle) is FailOpen==false (see
// isRealVerdictSource).
type Decision struct {
	Evaluation client.Evaluation
	// FailOpen reports that OpenBox did not govern this call — either no
	// bundle was loaded (cold start) or the request was unusable (missing
	// session / bad protocol). The failure policy engages on it: fail-open
	// (default) proceeds, fail-closed denies.
	FailOpen bool
	// Source echoes the evaluator's DecisionResponse.Source.
	Source string
	// Stale echoes the staleness flag (bundle older than the freshness
	// window).
	Stale bool
	// RedactedContent carries the local Tier-1 secret-redaction of the tool
	// content; the enforce hook reconstructs tool_input from it (content
	// field only) and applies it via Claude Code's updatedInput. nil when
	// nothing was redacted. INV-2: content-bearing but local — it never
	// leaves this process.
	RedactedContent *client.Content
	// RedactionCategories echoes the content-free category names that fired
	// (INV-2), for the durable enforcement audit. Never the secret text.
	RedactionCategories []string
}

// isRealVerdictSource reports whether a DecisionResponse.Source denotes a
// real evaluated verdict (as opposed to a degraded no-verdict reply the
// failure policy must handle). Every resident-evaluator decision is tagged
// sourceLocalBundle; any other source is treated as "no real verdict" →
// FailOpen, the safe direction.
func isRealVerdictSource(source string) bool {
	return source == sourceLocalBundle
}

// InProcessDecider evaluates decisions in-process, reusing the
// Server.decide path — the native builder/rules evaluator (ADR-0005) plus
// the Tier-1 secret detector — with no socket and no separate process. It's
// the only decision transport (ADR-0006): the evaluator is pure-Go and
// in-memory (no OPA, no cgo, no network on the decision path), so the
// enforce hook — itself a short-lived process spawned per tool call — loads
// the local bundle file `openbox dev sync` writes and evaluates it directly
// in microseconds. There is no daemon to start.
//
// Safety properties:
//   - a missing/unreadable/invalid bundle → cold-start fail-open (Server
//     with no evaluator answers VerdictUnknown / sourceFailOpenNoBundle);
//   - the failure policy still engages on that no-verdict outcome via the
//     Source → isRealVerdictSource mapping;
//   - staleness is surfaced (Server.decide flags Stale past Freshness);
//   - the tool body handed in for redaction stays in-process — it never
//     crosses any boundary (INV-2).
type InProcessDecider struct {
	// integrity records how the loaded bundle verified, for posture reporting.
	integrity Integrity
	srv       *Server
}

// InProcessConfig configures an InProcessDecider. All fields are optional.
type InProcessConfig struct {
	// BundlePath is the local policy bundle to evaluate. Empty →
	// DefaultBundlePath() (the same file `openbox dev sync` writes).
	BundlePath string
	// Freshness marks decisions Stale past this bundle age. Zero →
	// defaultFreshness.
	Freshness time.Duration
	// Logger receives non-secret diagnostics (INV-1). Nil discards.
	Logger client.Logger
	// SigningPubKey is the org's pinned policy-bundle signing key (E8-S6).
	// Nil means no key is pinned: a signed bundle then reports IntegrityNoKey
	// and is not trusted, while an unsigned bundle keeps working as before.
	SigningPubKey ed25519.PublicKey
}

// NewInProcessDecider builds an in-process decider that has already loaded
// the local bundle. It never fails: an absent/unreadable/invalid bundle
// leaves the underlying Server at cold-start fail-open, so the enforce
// failure policy — not a hard error — decides what happens. The bundle is
// loaded once here; a hook process is short-lived and session-start
// staleness drives re-sync, so there is no in-process re-poll.
func NewInProcessDecider(cfg InProcessConfig) *InProcessDecider {
	log := cfg.Logger
	if log == nil {
		log = nopLogger{}
	}
	srv := NewServer(ServerConfig{Freshness: cfg.Freshness, Logger: log})

	bp := cfg.BundlePath
	if bp == "" {
		bp = DefaultBundlePath()
	}
	// Integrity gate (E8-S6). A verified bundle is evaluated as re-derived from
	// its signed bytes; an UNVERIFIABLE one — unsigned, or signed with no org key
	// pinned — is evaluated as before (the compatibility path); anything that
	// actually FAILS verification is NOT loaded, so the decider stays at
	// cold-start fail-open.
	//
	// Not loading is detection, not prevention: if the tamper made policy MORE
	// permissive, fail-open lands where the attacker wanted anyway. What this
	// buys is that the outcome is recorded in session posture instead of passing
	// silently. Turning an unverifiable bundle into a deny for high-risk tool
	// classes is the posture change OD-E8-3 gates.
	trusted, integrity := VerifyBundleFile(bp, VerifyOptions{
		PublicKey: cfg.SigningPubKey,
		MinEpoch:  ReadEpochPin(bp),
	})
	switch {
	case trusted != nil:
		srv.SetBundle(trusted)
		if integrity == IntegrityVerified {
			WriteEpochPin(bp, trusted.Epoch())
		}
		if integrity == IntegrityNoKey {
			// Enforcement continues, but say plainly that it is unverified — the
			// operator's deployment is one step short, and silence here is what
			// makes an unpinned fleet look identical to a verified one.
			log.Printf("openbox enforce: local policy bundle at %s is signed but no org signing key is pinned — "+
				"enforcing it UNVERIFIED (a local edit would not be detectable); pin org_signing_pubkey in dev.json", bp)
		}
	case integrity == IntegrityMalformed && cfg.SigningPubKey == nil:
		// Indistinguishable from the pre-signing "no bundle" case, so keep the
		// long-standing message operators already recognize.
		log.Printf("openbox enforce: no local policy bundle at %s — decisions fail-open until `openbox dev sync` runs", bp)
	default:
		log.Printf("openbox enforce: local policy bundle at %s is not trusted (%s) — decisions fail-open; run `openbox dev sync`", bp, integrity)
	}
	return &InProcessDecider{srv: srv, integrity: integrity}
}

// Integrity reports how the loaded bundle verified, so a caller can record it
// in session posture (E8-S5) rather than having to re-read and re-verify.
func (d *InProcessDecider) Integrity() Integrity { return d.integrity }

// Decide evaluates req against the loaded bundle in-process and returns a
// Decision. ctx is accepted for interface parity; the evaluation is
// synchronous and in-memory (no deadline to honor).
func (d *InProcessDecider) Decide(_ context.Context, req DecisionRequest) Decision {
	if req.Protocol == 0 {
		req.Protocol = ProtocolVersion
	}
	resp := d.srv.decide(req)
	return Decision{
		Evaluation:          resp.Evaluation,
		FailOpen:            !isRealVerdictSource(resp.Source),
		Source:              resp.Source,
		Stale:               resp.Stale,
		RedactedContent:     resp.RedactedContent,     // local-only, never left the process
		RedactionCategories: resp.RedactionCategories, // content-free audit signal (INV-2)
	}
}
