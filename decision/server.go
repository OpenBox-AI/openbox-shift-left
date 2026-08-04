package decision

import (
	"sync"
	"time"

	"github.com/openbox-ai/openbox-shift-left/client"
)

// defaultFreshness is how long a loaded bundle is considered current. Past
// it, decisions are still served (fail-open never denies on staleness) but
// flagged Stale so telemetry can observe sync drift.
const defaultFreshness = 5 * time.Minute

// engine holds the loaded policy evaluator + Tier-1 secret detector and
// answers a DecisionRequest with no I/O and no network on the decision path
// (INV-3b). It's the in-memory engine behind InProcessDecider (ADR-0006):
// there is no socket, no listener and nothing resident — the enforce hook
// constructs one, loads the local bundle, and calls decide directly. The
// mutex guards the atomic bundle swap so a decision sees either the whole
// old bundle or the whole new one.
type engine struct {
	mu      sync.RWMutex
	eval    Evaluator // nil until a bundle is loaded → cold-start fail-open
	version string
	// bundleWrittenAt is when the loaded policy was last written to disk, NOT
	// when this process loaded it (OD-RF-4). Load time made the flag inert: the
	// decider is built per tool call in a short-lived hook process, so the
	// freshness window could never elapse and Stale was always false. What the
	// flag is meant to answer is "was this decided against current policy",
	// which is a property of the bundle, not of the process.
	bundleWrittenAt time.Time
	freshness       time.Duration

	// scanner is the Tier-1 local secret detector. It runs on every decision
	// that carries content, decoupled from the policy verdict/bundle — set
	// to defaultSecretDetector by newEngine. Immutable + safe for
	// concurrent use; nil disables local redaction (tests).
	scanner *secretDetector

	now func() time.Time
	log client.Logger
}

// engineConfig configures a engine. All fields are optional.
type engineConfig struct {
	Freshness time.Duration // default 5m
	Logger    client.Logger // default discards
	now       func() time.Time
}

// newEngine builds an engine with no policy loaded yet: until SetBundle (or
// SetEvaluator) runs, every decision is a cold-start fail-open allow. That is the
// honest default — an engine that has not yet loaded a policy must not block.
func newEngine(cfg engineConfig) *engine {
	s := &engine{
		freshness: cfg.Freshness,
		scanner:   defaultSecretDetector,
		now:       cfg.now,
		log:       cfg.Logger,
	}
	if s.freshness == 0 {
		s.freshness = defaultFreshness
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.log == nil {
		s.log = nopLogger{}
	}
	return s
}

type nopLogger struct{}

func (nopLogger) Printf(string, ...any) {}

// SetBundle installs a new local policy bundle. The swap is atomic w.r.t. in-flight
// decisions. Passing nil clears the policy (back to cold-start fail-open).
func (s *engine) SetBundle(b *Bundle) {
	var e Evaluator
	var version string
	if b != nil {
		version = b.Version
		switch {
		case b.PolicyBuilder != nil:
			// A builder-authored policy: first-match native evaluator
			// (ADR-0005). Distinct from the legacy Rules max-severity path
			// so its rule-order precedence is preserved.
			e = newBuilderEvaluator(b.PolicyBuilder, b.PolicyID)
		default:
			// Legacy hand-authored Rules bundle, an empty no-policy bundle
			// (data==null → allow), or a rules-less bundle (→ allow): the
			// max-severity bundleEvaluator. An empty/rules-less bundle
			// yields a real local ALLOW (sourceLocalBundle), so it proceeds
			// under both fail-open and fail-closed — honest
			// under-blocking, never over-blocking.
			e = newBundleEvaluator(b)
		}
	}
	s.SetEvaluator(e, version)
}

// SetBundleWrittenAt records when the loaded policy was written to disk, which
// is what freshness is measured from.
func (s *engine) SetBundleWrittenAt(t time.Time) {
	s.mu.Lock()
	s.bundleWrittenAt = t
	s.mu.Unlock()
}

// SetEvaluator installs a custom Evaluator (the native-evaluator seam, ADR-0005)
// with a version tag. Concurrency-safe.
func (s *engine) SetEvaluator(e Evaluator, version string) {
	s.mu.Lock()
	s.eval = e
	s.version = version
	// Left zero unless the caller supplies the bundle's write time; a decider
	// built from an in-memory bundle has no file to age.
	s.mu.Unlock()
}

// decide is the pure decision function: no I/O, no network. It maps a
// DecisionRequest to a DecisionResponse against the loaded evaluator, flags
// staleness, and attaches any Tier-1 secret redaction.
//
// Redaction is applied here rather than inside evaluate so that no verdict path
// can skip it. It used to sit at the end of the evaluation body, which the
// unsupported-protocol and missing-session-id branches returned before ever
// reaching — so a request that failed those checks was written out with no
// secret scan at all, contradicting the decoupling the code claimed. Both are
// degraded paths that should still be scanned: the scan does not depend on the
// verdict, the bundle, or the session.
func (s *engine) decide(req DecisionRequest) DecisionResponse {
	resp := s.evaluate(req)
	s.applySecretRedaction(req, &resp)
	return resp
}

// evaluate produces the verdict half of a decision. Callers go through decide.
func (s *engine) evaluate(req DecisionRequest) DecisionResponse {
	if req.Protocol != 0 && req.Protocol != ProtocolVersion {
		// Unknown protocol → no real verdict, never mis-decode a request we
		// don't understand into a block. VerdictUnknown (honest: "not
		// evaluated") + sourceFailOpenNoBundle lets the caller mark this
		// FailOpen.
		return DecisionResponse{Protocol: ProtocolVersion,
			Evaluation: client.Evaluation{Verdict: client.VerdictUnknown},
			Source:     sourceFailOpenNoBundle,
			Error:      "unsupported protocol version"}
	}
	if req.SessionID == "" {
		return DecisionResponse{Protocol: ProtocolVersion,
			Evaluation: client.Evaluation{Verdict: client.VerdictUnknown},
			Source:     sourceFailOpenNoBundle,
			Error:      "missing openbox_session_id"}
	}

	s.mu.RLock()
	eval := s.eval
	writtenAt := s.bundleWrittenAt
	s.mu.RUnlock()

	var resp DecisionResponse
	if eval == nil {
		// Cold start: no policy loaded yet → no real verdict. VerdictUnknown
		// records honestly that OpenBox did not evaluate this call, and
		// sourceFailOpenNoBundle lets the caller mark the Decision FailOpen
		// so the failure policy engages: fail-open (default) proceeds;
		// fail-closed denies. Never deny here — the engine doesn't know the
		// policy, so the block decision belongs to the failure policy, not
		// this fail-open primitive.
		resp = DecisionResponse{
			Protocol:   ProtocolVersion,
			Evaluation: client.Evaluation{Verdict: client.VerdictUnknown},
			Source:     sourceFailOpenNoBundle,
		}
	} else {
		resp = DecisionResponse{
			Protocol:   ProtocolVersion,
			Evaluation: eval.Evaluate(req),
			Source:     sourceLocalBundle,
		}
		if !writtenAt.IsZero() && s.now().Sub(writtenAt) > s.freshness {
			resp.Stale = true
		}
	}

	return resp
}

// applySecretRedaction runs the local secret detector over the request's
// content (when present) and attaches any redaction to resp. It's the only
// producer of RedactedContent today. Pure/local (INV-1/INV-2/INV-3b): no
// I/O, no logging of the content or the secret; the result rides only the
// local response.
//
// It never alters resp.Evaluation — redact-and-continue is a proceed-path
// rewrite, orthogonal to the deny/ask verdict. On a BLOCK the tool never runs,
// so the enforce hook simply ignores the attached redaction.
func (s *engine) applySecretRedaction(req DecisionRequest, resp *DecisionResponse) {
	if s.scanner == nil || req.Content == nil || req.Content.FileText == "" {
		return
	}
	red, cats, changed := s.scanner.Redact(req.Content.FileText)
	if !changed {
		return
	}
	resp.RedactedContent = &client.Content{FileText: red}
	resp.RedactionCategories = cats
}
