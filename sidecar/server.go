package sidecar

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"sync"
	"time"

	"github.com/openbox-ai/openbox-shift-left/client"
)

// defaultMaxRequestBytes bounds a single decision request (and, symmetrically, the
// response the Client reads back). The verdict axes are metadata-only (INV-2) and
// tiny, but the OPTIONAL Content field carries a bounded tool body for LOCAL
// redaction (E6-S4 / E6-S9 secret detection); the enforce hook caps that body
// (adapters/claude-code maxRedactBody) so a real request stays well under this. The
// cap defends the daemon against a malformed/oversized peer without ever touching
// the decision path (INV-1 — bounded read, no crash). The 0600 socket restricts
// peers to the owning user, so the only DoS this bounds is a same-user self-DoS.
const defaultMaxRequestBytes = 1 << 20 // 1 MiB

// defaultFreshness is how long a synced bundle is considered current. Past it,
// decisions are still served (fail-open never denies on staleness) but flagged
// Stale so E6-S3/telemetry can observe sync drift.
const defaultFreshness = 5 * time.Minute

// defaultMaxInFlight bounds concurrent connection handlers (G_SEC F3). The 0600
// socket already restricts peers to the owning user, so this only guards against
// a same-user runaway/self-DoS; a flood queues in the kernel backlog and the
// hook's own timeout fails it open. Ample for the per-tool-call dial rate.
const defaultMaxInFlight = 128

// Server is the resident Unix-socket decision daemon. It answers a
// DecisionRequest from its in-memory Evaluator with NO network I/O on the
// decision path (INV-3b) and is safe for concurrent connections.
type Server struct {
	mu        sync.RWMutex
	eval      Evaluator // nil until a bundle is loaded → cold-start fail-open
	version   string
	loadedAt  time.Time
	freshness time.Duration

	// scanner is the Tier-1 local secret detector (STORY-E6-S9). It runs on every
	// decision that carries content, DECOUPLED from the policy verdict/bundle
	// (OD-SYNC-10) — set to defaultSecretDetector by NewServer. Immutable + safe for
	// concurrent use; nil disables local redaction (tests).
	scanner *secretDetector

	maxReq      int64
	maxInFlight int
	now         func() time.Time
	log         client.Logger
}

// ServerConfig configures a Server. All fields are optional.
type ServerConfig struct {
	Freshness       time.Duration // default 5m
	MaxRequestBytes int64         // default 64 KiB
	MaxInFlight     int           // default 128; concurrent handler cap
	Logger          client.Logger // default discards
	now             func() time.Time
}

// NewServer builds a decision server with no policy loaded yet: until SetBundle
// (or SetEvaluator) runs, every decision is a cold-start fail-open allow. That is
// the honest default — a daemon that has not yet synced a policy must not block.
func NewServer(cfg ServerConfig) *Server {
	s := &Server{
		freshness:   cfg.Freshness,
		scanner:     defaultSecretDetector,
		maxReq:      cfg.MaxRequestBytes,
		maxInFlight: cfg.MaxInFlight,
		now:         cfg.now,
		log:         cfg.Logger,
	}
	if s.freshness == 0 {
		s.freshness = defaultFreshness
	}
	if s.maxReq == 0 {
		s.maxReq = defaultMaxRequestBytes
	}
	if s.maxInFlight <= 0 {
		s.maxInFlight = defaultMaxInFlight
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

// SetBundle installs a new local policy bundle (the out-of-band Sync loop calls
// this). The swap is atomic w.r.t. in-flight decisions: a decision either sees
// the whole old bundle or the whole new one, never a partial. Passing nil clears
// the policy (back to cold-start fail-open).
func (s *Server) SetBundle(b *Bundle) {
	var e Evaluator
	var version string
	if b != nil {
		e = newBundleEvaluator(b)
		version = b.Version
	}
	s.SetEvaluator(e, version)
}

// SetEvaluator installs a custom Evaluator (the embedded-OPA evaluator seam,
// ADR-0003) with a version tag. Concurrency-safe.
func (s *Server) SetEvaluator(e Evaluator, version string) {
	s.mu.Lock()
	s.eval = e
	s.version = version
	s.loadedAt = s.now()
	s.mu.Unlock()
}

// Serve accepts connections on ln until ctx is cancelled or ln is closed, then
// drains in-flight handlers. Each connection carries exactly one decision
// request/response (the enforce hook dials per tool call — matching the Phase-1
// fork/exec-per-hook model). Serve returns nil on a clean context-cancel
// shutdown.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	// Closing the listener unblocks Accept on shutdown.
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	// sem bounds concurrent handlers (G_SEC F3). Acquiring before Accept's spawn
	// applies backpressure: under a same-user flood, new connections wait in the
	// kernel backlog and the peer's own timeout fails it open.
	sem := make(chan struct{}, s.maxInFlight)
	var wg sync.WaitGroup
	for {
		conn, err := ln.Accept()
		if err != nil {
			// A closed listener during shutdown is the expected exit, not an error.
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				wg.Wait()
				return nil
			}
			// Transient accept error: log and keep serving (a dead accept loop would
			// silently disable enforcement).
			s.log.Printf("openbox sidecar: accept: %v", err)
			continue
		}
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			s.handleConn(conn)
		}()
	}
}

// handleConn reads one bounded JSON request, decides, writes one JSON response,
// and closes. It never panics out to the accept loop (a bad peer must not take
// the daemon down) and never blocks unboundedly (read deadline + size cap).
func (s *Server) handleConn(conn net.Conn) {
	defer conn.Close()
	defer func() { _ = recover() }() // a malformed peer never crashes the daemon

	// A short read/write deadline keeps a slow/stuck peer from pinning a handler;
	// the enforce hook writes its request immediately and reads the reply.
	_ = conn.SetDeadline(s.now().Add(2 * time.Second))

	var req DecisionRequest
	dec := json.NewDecoder(io.LimitReader(conn, s.maxReq))
	if err := dec.Decode(&req); err != nil {
		// No real verdict → VerdictUnknown (set explicitly for parity with the decide
		// paths, though VerdictUnknown is the zero value) + sourceFailOpenNoBundle so
		// the client marks the Decision FailOpen (E6-S7).
		s.writeResponse(conn, DecisionResponse{
			Protocol:   ProtocolVersion,
			Evaluation: client.Evaluation{Verdict: client.VerdictUnknown},
			Source:     sourceFailOpenNoBundle,
			Error:      "malformed request",
		})
		return
	}
	s.writeResponse(conn, s.decide(req))
}

// decide is the pure decision function: no I/O, no network. Exposed for tests.
func (s *Server) decide(req DecisionRequest) DecisionResponse {
	if req.Protocol != 0 && req.Protocol != ProtocolVersion {
		// Unknown protocol → no real verdict, never mis-decode a request we don't
		// understand into a block. VerdictUnknown (honest: "not evaluated") +
		// sourceFailOpenNoBundle lets the client mark this FailOpen (E6-S7).
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
		// Cold start: no policy synced yet → NO real verdict. VerdictUnknown records
		// honestly that OpenBox did not evaluate this call (matching the client's own
		// allowFailOpen), and sourceFailOpenNoBundle lets the client mark the Decision
		// FailOpen so the failure policy engages (E6-S7 / E6-S3 INFO-1): fail-open
		// (OD9 default) proceeds; fail-closed denies. Never deny HERE — the daemon
		// does not know the policy, so the block decision belongs to the client-side
		// failure policy, not this fail-open primitive.
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

	// Tier-1 secret redaction (STORY-E6-S9) runs DECOUPLED from the policy verdict
	// and bundle (OD-SYNC-10): it fires here in the cold-start branch too, and never
	// alters resp.Evaluation — redact-and-continue is a proceed-path rewrite,
	// orthogonal to the deny/ask verdict. On a BLOCK verdict the tool never runs, so
	// the enforce hook simply ignores the attached redaction.
	s.applySecretRedaction(req, &resp)
	return resp
}

// applySecretRedaction runs the local secret detector over the request's content
// (when present) and attaches any redaction to resp. It is the ONLY producer of
// RedactedContent today. Pure/local (INV-1/INV-2/INV-3b): no I/O, no logging of the
// content or the secret; the result rides only the LOCAL response.
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

// writeResponse marshals and writes one response. A write failure is logged
// (non-secret) and dropped — the peer will fail open on the missing reply.
func (s *Server) writeResponse(conn net.Conn, resp DecisionResponse) {
	b, err := json.Marshal(resp)
	if err != nil {
		s.log.Printf("openbox sidecar: marshal response: %v", err)
		return
	}
	b = append(b, '\n')
	if _, err := conn.Write(b); err != nil {
		s.log.Printf("openbox sidecar: write response: %v", err)
	}
}
