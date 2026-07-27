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

// Server holds the loaded policy evaluator + Tier-1 secret detector and
// answers a DecisionRequest with no I/O and no network on the decision path
// (INV-3b). It's the in-memory engine behind InProcessDecider (ADR-0006):
// there is no socket, no listener, no resident daemon — the enforce hook
// constructs one, loads the local bundle, and calls decide directly. The
// mutex guards the atomic bundle swap so a decision sees either the whole
// old bundle or the whole new one.
type Server struct {
	mu        sync.RWMutex
	eval      Evaluator // nil until a bundle is loaded → cold-start fail-open
	version   string
	loadedAt  time.Time
	freshness time.Duration

	// scanner is the Tier-1 local secret detector. It runs on every decision
	// that carries content, decoupled from the policy verdict/bundle — set
	// to defaultSecretDetector by NewServer. Immutable + safe for
	// concurrent use; nil disables local redaction (tests).
	scanner *secretDetector

	now func() time.Time
	log client.Logger
}

// ServerConfig configures a Server. All fields are optional.
type ServerConfig struct {
	Freshness time.Duration // default 5m
	Logger    client.Logger // default discards
	now       func() time.Time
}

// NewServer builds an engine with no policy loaded yet: until SetBundle (or
// SetEvaluator) runs, every decision is a cold-start fail-open allow. That is the
// honest default — an engine that has not yet loaded a policy must not block.
func NewServer(cfg ServerConfig) *Server {
	s := &Server{
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
func (s *Server) SetBundle(b *Bundle) {
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

// SetEvaluator installs a custom Evaluator (the native-evaluator seam, ADR-0005)
// with a version tag. Concurrency-safe.
func (s *Server) SetEvaluator(e Evaluator, version string) {
	s.mu.Lock()
	s.eval = e
	s.version = version
	s.loadedAt = s.now()
	s.mu.Unlock()
}

// decide is the pure decision function: no I/O, no network. It maps a
// DecisionRequest to a DecisionResponse against the loaded evaluator, flags
// staleness, and attaches any Tier-1 secret redaction.
func (s *Server) decide(req DecisionRequest) DecisionResponse {
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
	loadedAt := s.loadedAt
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
		if !loadedAt.IsZero() && s.now().Sub(loadedAt) > s.freshness {
			resp.Stale = true
		}
	}

	// Tier-1 secret redaction runs decoupled from the policy verdict and
	// bundle: it fires in the cold-start branch too, and never alters
	// resp.Evaluation — redact-and-continue is a proceed-path rewrite,
	// orthogonal to the deny/ask verdict. On a BLOCK verdict the tool never
	// runs, so the enforce hook simply ignores the attached redaction.
	s.applySecretRedaction(req, &resp)
	return resp
}

// applySecretRedaction runs the local secret detector over the request's
// content (when present) and attaches any redaction to resp. It's the only
// producer of RedactedContent today. Pure/local (INV-1/INV-2/INV-3b): no
// I/O, no logging of the content or the secret; the result rides only the
// local response.
func (s *Server) applySecretRedaction(req DecisionRequest, resp *DecisionResponse) {
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
