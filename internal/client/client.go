package client

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/cenkalti/backoff/v5"
	"github.com/openbox-ai/openbox-shift-left/internal/client/workloadauth"
)

const evaluatePath = "/api/v3/governance/evaluate"

// The client must match these exactly or core rejects the request.
const (
	headerAuthorization = "Authorization"
	headerSDKVersion    = "X-OpenBox-SDK-Version"
	headerUserAgent     = "User-Agent"

	// headerIdempotencyKey carries the event's idempotency key (==
	// DevEvent.EventID == metadata.event_id) as a standard request header
	// (INV-5).
	headerIdempotencyKey = "Idempotency-Key"

	// headerWorkloadToken carries the exchanged Keycloak bearer. The
	// Authorization header still carries the obx_ API key
	// (setWorkloadHeaders below), never the workload token.
	headerWorkloadToken = "X-OpenBox-Workload-Token"
)

// Fail-open (INV-3): these bound delay, never whether Emit proceeds.
const (
	defaultTimeout    = 30 * time.Second
	defaultMaxRetries = 2
	defaultRetryBase  = 150 * time.Millisecond
)

// Logger sinks fail-open diagnostics: event ids, types, transport errors only;
// never secrets or content (INV-1/INV-2).
type Logger interface {
	Printf(format string, args ...any)
}

type nopLogger struct{}

func (nopLogger) Printf(string, ...any) {}

// Config configures a Client.
type Config struct {
	BaseURL string // openbox-core base, e.g. https://core.openbox.ai
	APIKey  string // obx_(live|test)_… runtime key (INV-1)

	// WorkloadPrivateKey is the workload agent's RSA private key, a PEM
	// (PKCS#8) or single-line base64 DER, in either form
	// workloadauth.ParsePrivateKey accepts. Required: New parses it (and
	// builds the Authenticator) locally, before any network call.
	WorkloadPrivateKey string

	// TokenCachePath is where the workload bearer token is cached on disk;
	// "" selects an in-memory cache (lane daemons, git-action, doctor).
	TokenCachePath string

	// ContentCaptureEnabled is the org's content posture; default false strips
	// content before egress (INV-2).
	ContentCaptureEnabled bool

	HTTP *http.Client // optional; a 30s-timeout client is built if nil

	// MaxRetries and RetryBase are *T so zero is expressible.
	MaxRetries *int
	RetryBase  *time.Duration

	Logger Logger // optional; default discards
	now    func() time.Time

	// tokens is a test seam beside now: substituting a fake tokenSource lets a
	// test drive a workload-token acquisition failure without a real
	// bootstrap/exchange round trip. Left nil, New builds a real
	// *workloadauth.Authenticator whenever WorkloadPrivateKey is set.
	tokens tokenSource
}

// tokenSource is the workload-identity acquisition seam attempt() and
// validateV3 use. *workloadauth.Authenticator satisfies it structurally.
type tokenSource interface {
	// Token returns a bearer token: fromCache=true for a cache hit, false for
	// a cold bootstrap+exchange. err is never an *httpError -- an acquisition
	// failure is not a judgment on any particular event, so it must never be
	// mistaken for one (isRefusal).
	Token(ctx context.Context) (token string, fromCache bool, err error)
	// Invalidate clears the cached token (file and/or memory), so the next
	// Token call goes cold.
	Invalidate() error
}

// Client is the shared /evaluate transport: authenticated on /api/v3 with a
// workload identity bearer.
type Client struct {
	baseURL    string
	apiKey     string
	tokens     tokenSource
	contentOn  bool
	http       *http.Client
	maxRetries int
	retryBase  time.Duration
	log        Logger
	now        func() time.Time
}

// New builds a Client.
func New(cfg Config) (*Client, error) {
	if cfg.BaseURL == "" {
		return nil, errors.New("client: BaseURL is required")
	}
	if err := checkBaseURL(cfg.BaseURL); err != nil {
		return nil, err
	}
	if cfg.APIKey == "" {
		return nil, errors.New("client: APIKey (obx_ runtime key) is required")
	}
	if cfg.WorkloadPrivateKey == "" {
		return nil, errors.New("client: WorkloadPrivateKey is required")
	}

	httpc := cfg.HTTP
	if httpc == nil {
		httpc = &http.Client{Timeout: defaultTimeout}
	}

	var tokens tokenSource
	if cfg.tokens != nil {
		tokens = cfg.tokens
	} else {
		// ParsePrivateKey runs inside NewAuthenticator, before any network
		// call: a malformed key fails New, never a later Emit.
		auth, aerr := workloadauth.NewAuthenticator(cfg.APIKey, cfg.WorkloadPrivateKey, cfg.TokenCachePath, cfg.BaseURL, httpc)
		if aerr != nil {
			return nil, aerr
		}
		tokens = auth
	}

	maxRetries := defaultMaxRetries
	if cfg.MaxRetries != nil {
		if *cfg.MaxRetries < 0 {
			return nil, errors.New("client: MaxRetries must not be negative")
		}
		maxRetries = *cfg.MaxRetries
	}
	retryBase := defaultRetryBase
	if cfg.RetryBase != nil {
		if *cfg.RetryBase < 0 {
			return nil, errors.New("client: RetryBase must not be negative")
		}
		retryBase = *cfg.RetryBase
	}
	var log Logger = nopLogger{}
	if cfg.Logger != nil {
		log = cfg.Logger
	}
	now := cfg.now
	if now == nil {
		now = time.Now
	}
	return &Client{
		baseURL:    strings.TrimRight(cfg.BaseURL, "/"),
		apiKey:     cfg.APIKey,
		tokens:     tokens,
		contentOn:  cfg.ContentCaptureEnabled,
		http:       httpc,
		maxRetries: maxRetries,
		retryBase:  retryBase,
		log:        log,
		now:        now,
	}, nil
}

// ErrDelivery reports that an event could not be delivered after retries. It
// is advisory (see Emit): the caller is never obliged to act on it, but a
// caller holding a durable copy should keep the event and retry rather than
// drop it.
var ErrDelivery = errors.New("client: event delivery failed")

// ErrUnbuildable reports that an event could not be serialized at all, so it
// was never sent and retrying it verbatim cannot help. It is distinct from
// ErrDelivery on purpose.
var ErrUnbuildable = errors.New("client: event could not be built")

// ErrRefused reports that the control plane refused this event ON ITS MERITS: a
// non-retryable 4xx that is neither 401 nor 429. Re-sending it verbatim will be
// refused again, so a durable caller may spend one of its delivery attempts on
// it. It rides ALONGSIDE ErrDelivery, never instead of it, so a caller that only
// knows ErrDelivery keeps working.
//
// It exists because the alternative -- treating every failure as a refusal --
// makes a retry cap into a timer: a transport fault or an outage would spend an
// event's whole allowance without the server ever having judged it, and the
// spool is the only copy.
//
// 401 is deliberately NOT a refusal. Core answers "invalid token or agent
// identity" for a genuinely rejected identity AND for any datastore failure
// while looking the token up, so the two are indistinguishable on the wire.
// Burning attempts on 401 would let a database hiccup delete governance
// evidence; a permanently rejected identity instead costs one cheap request per
// event per sweep, bounded by the retention age.
var ErrRefused = errors.New("client: control plane refused the event")

// isRefusal reports whether err is a proven, event-specific refusal. Anything
// this cannot prove -- a transport error, an exhausted 5xx, a 401, a context
// that expired mid-POST -- is not one.
func isRefusal(err error) bool {
	var he *httpError
	if !errors.As(err, &he) {
		return false // transport, or a budget that died inside the request
	}
	switch {
	case he.status == http.StatusUnauthorized: // see ErrRefused
		return false
	case he.status == http.StatusTooManyRequests: // retryable; exhausted, not refused
		return false
	default:
		return he.status >= 400 && he.status < 500
	}
}

// Emit builds the core payload from a normalized dev event and POSTs it,
// workload-identity-authenticated, to /evaluate. It is fail-open (INV-3): the
// Evaluation it returns on
// any failure is the zero value, which every caller treats as allow, so a
// failure here can never block a tool call.
//   - A caller precondition (empty EventID/SessionID), which is a bug to fix;
//   - ErrDelivery, wrapping a transport failure after retries.
func (c *Client) Emit(ctx context.Context, ev DevEvent) (Evaluation, error) {
	// Both must be surfaced, not fail-open dropped; an empty one would silently
	// corrupt session grouping.
	if ev.EventID == "" {
		return Evaluation{}, errors.New("client: DevEvent.EventID is required (INV-5 idempotency key)")
	}
	if ev.SessionID == "" {
		// Still required even on a continued run: runIDFor selects the minted
		// RunID when there is one and falls back to SessionID otherwise, so an
		// event with no session id has no run id either.
		return Evaluation{}, errors.New("client: DevEvent.SessionID is required (the run id falls back to it)")
	}

	// Copy, so the caller's event is never mutated.
	if !c.contentOn {
		ev = stripContent(ev)
	}

	body, err := buildPayload(ev)
	if err != nil {
		// But it must not read as success: ErrUnbuildable tells a durable caller the
		// event is lost and not worth re-queueing, which returning nil could not.
		c.log.Printf("openbox: dropping event %s (%s): build payload: %v", ev.EventID, ev.EventType, err)
		return Evaluation{}, fmt.Errorf("%w: %v", ErrUnbuildable, err)
	}

	respBody, err := c.post(ctx, evaluatePath, body, ev.EventID)
	if err != nil {
		// Surfacing ErrDelivery lets a durable caller re-spool the event instead of
		// losing it; callers that cannot retry ignore it (see the doc comment).
		c.log.Printf("openbox: delivery failed for event %s (%s): %s", ev.EventID, ev.EventType, describeDrop(err))
		// err itself rides along wrapped (not just its describeDrop text), so
		// FailureClass can recover the *httpError/context deadline a single-attempt
		// caller (the hook spool) needs to pick a discard-ledger/halt-latch reason;
		// safe by the same INV-1 guarantee describeDrop relies on: neither
		// *httpError nor *workloadauth.Error ever carries a secret.
		if isRefusal(err) {
			// Both sentinels: ErrDelivery for every caller that re-spools, ErrRefused
			// for the one that also decides whether this cost an attempt.
			return Evaluation{}, fmt.Errorf("%w: %w: %w", ErrDelivery, ErrRefused, err)
		}
		return Evaluation{}, fmt.Errorf("%w: %w", ErrDelivery, err)
	}
	return parseEvaluation(respBody), nil
}

// retryBudget is what the linear schedule this replaced would have spent:
// sum(i*retryBase) for i in 1..maxRetries. Derived rather than configured, so
// the bound holds for whatever a caller sets and has no knob to grow through.
// It bounds delays only: counting request time would drop a retry against a
// slow control plane, which is when the retry is worth having.
func (c *Client) retryBudget() time.Duration {
	n := time.Duration(c.maxRetries)
	return n * (n + 1) / 2 * c.retryBase
}

// post delivers one signed request, retrying a retryable failure.
//
// Exponential with full jitter, not the arithmetic ramp it replaced: every hook
// on every session used to retry on the same 150/300ms schedule, so a
// recovering control plane met the whole fleet in lockstep.
//
// Retry-After is a stop signal, not a sleep. Asking for longer than the budget
// gets what the server actually wants -- no more requests -- while ErrDelivery
// re-spools the event. Waiting inline buys nothing the spool does not, and
// INV-3 says a hook must not hold the tool call open.
func (c *Client) post(ctx context.Context, path string, body []byte, idemKey string) ([]byte, error) {
	// Each delay is drawn from [0, 2*interval], and the intervals are
	// retryBase/2 then retryBase thereafter, so the worst-case sum is
	// base*(2n-1) against the ramp's base*n(n+1)/2 -- equal at the default two
	// retries, below it for more. The expected sum is half the old schedule.
	b := &budgetedBackOff{remaining: c.retryBudget(), inner: backoff.NewExponentialBackOff()}
	b.inner.InitialInterval = c.retryBase / 2
	b.inner.RandomizationFactor = 1
	b.inner.Multiplier = 2
	b.inner.MaxInterval = c.retryBase

	return backoff.Retry(ctx, func() ([]byte, error) {
		respBody, retryable, err := c.attempt(ctx, path, body, idemKey)
		switch {
		case err == nil:
			return respBody, nil
		case !retryable:
			return nil, backoff.Permanent(err)
		}
		var he *httpError
		if errors.As(err, &he) && he.retryAfter > 0 {
			if he.retryAfter > b.remaining {
				// Longer than what is left of the budget. Giving up is the half of
				// Retry-After that protects the server, and it is the half that
				// matters: ErrDelivery re-spools the event for the next flush.
				return nil, backoff.Permanent(err)
			}
			b.remaining -= he.retryAfter
			return nil, &backoff.RetryAfterError{Duration: he.retryAfter}
		}
		return nil, err
	},
		backoff.WithBackOff(b),
		backoff.WithMaxTries(uint(c.maxRetries)+1),
		// Off explicitly. It bounds total elapsed, request time included, which
		// would drop a retry against a slow control plane -- when the retry is
		// worth having. The budget below bounds the delays and nothing else.
		backoff.WithMaxElapsedTime(0),
	)
}

// budgetedBackOff spends a fixed delay budget and then stops. The clamp has to
// be cumulative: checking each Retry-After against the whole budget lets a
// server spend it once per attempt, and backoff/v5 resets its ramp on every
// RetryAfterError. Retry-After is attacker-influenceable under an impersonated
// control plane, so this is the DoS bound, and a bound that resets is not one.
type budgetedBackOff struct {
	remaining time.Duration
	inner     *backoff.ExponentialBackOff
}

func (b *budgetedBackOff) Reset() { b.inner.Reset() }

// NextBackOff returns backoff.Stop once the budget cannot cover another delay,
// which ends the retry loop with the last error rather than waiting anyway.
func (b *budgetedBackOff) NextBackOff() time.Duration {
	next := b.inner.NextBackOff()
	if next == backoff.Stop || next > b.remaining {
		return backoff.Stop
	}
	b.remaining -= next
	return next
}

// attempt acquires a workload bearer token and POSTs the workload envelope. A
// token-acquisition failure is returned verbatim -- NEVER wrapped in an
// *httpError -- so isRefusal can never mistake it for a proven, event-specific
// refusal (it is not one: the control plane never saw the event). It is
// retryable iff the *workloadauth.Error says Transient.
//
// A 401 is never resent, cached token or not. A cached token's 401
// invalidates the cache (file and memory) so the next attempt goes cold; a
// fresh token's 401 invalidates nothing, since there is nothing stale to
// evict. Either way retryable is false here, which is what stops post's retry
// loop from resending.
func (c *Client) attempt(ctx context.Context, path string, body []byte, idemKey string) (respBody []byte, retryable bool, err error) {
	token, fromCache, terr := c.tokens.Token(ctx)
	if terr != nil {
		var werr *workloadauth.Error
		retryable = errors.As(terr, &werr) && werr.Transient
		return nil, retryable, terr
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Content-Type", "application/json")
	c.setWorkloadHeaders(req.Header, token)
	if idemKey != "" {
		req.Header.Set(headerIdempotencyKey, idemKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, true, err // network/transport error; retryable
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return rb, false, nil
	}
	if resp.StatusCode == http.StatusUnauthorized {
		if fromCache {
			_ = c.tokens.Invalidate()
		}
		return nil, false, &httpError{path: path, status: resp.StatusCode, body: string(rb)}
	}
	retryable = resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests
	ra, _ := parseRetryAfter(resp.Header.Get("Retry-After"), c.now())
	return nil, retryable, &httpError{path: path, status: resp.StatusCode, body: string(rb), retryAfter: ra}
}

// parseRetryAfter reads both forms RFC 9110 allows: delta-seconds, and an
// HTTP-date. The header was read nowhere in this repo, so a 429 saying "come
// back in a minute" was retried 150ms later.
func parseRetryAfter(h string, now time.Time) (time.Duration, bool) {
	h = strings.TrimSpace(h)
	if h == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(h); err == nil {
		if secs <= 0 {
			return 0, false
		}
		return time.Duration(secs) * time.Second, true
	}
	if t, err := http.ParseTime(h); err == nil {
		if d := t.Sub(now); d > 0 {
			return d, true
		}
		return 0, false
	}
	return 0, false
}

// FailureClass classifies why Emit's error means core did not accept an
// event, coarse enough for a discard-ledger line or a halt-latch reason
// without ever naming request/response content (INV-2): "unbuildable" (never
// sent at all), "401", "429", "5xx", "4xx" (a proven refusal that is none of
// the above), "timeout" (the attempt's own deadline, not the caller's), or
// "network" (anything else: DNS, dial, TLS, a token acquisition fault). "" for
// a nil err, which every caller treats as no failure.
//
// Order matters: ErrUnbuildable is checked before *httpError because a build
// failure never reaches the wire at all, and context.DeadlineExceeded is
// checked before the network fallback because a *url.Error wrapping it is
// still "no status code", the same shape a dial failure has.
func FailureClass(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrUnbuildable):
		return "unbuildable"
	}
	var he *httpError
	if errors.As(err, &he) {
		switch {
		case he.status == http.StatusUnauthorized:
			return "401"
		case he.status == http.StatusTooManyRequests:
			return "429"
		case he.status >= 500:
			return "5xx"
		default:
			return "4xx"
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	return "network"
}

// RetryableDelivery reports whether a failed Emit earns its event the one
// delivery retry a drainer allows before halting the run: only a transient
// fault, where core timed out, was unreachable, or answered 5xx. A 401, a
// 429, any other 4xx and an unbuildable event halt on the first failure.
// Only an ErrDelivery qualifies, so a caller's own precondition error is
// never resent.
func RetryableDelivery(err error) bool {
	if !errors.Is(err, ErrDelivery) {
		return false
	}
	switch FailureClass(err) {
	case "timeout", "network", "5xx":
		return true
	}
	return false
}

type httpError struct {
	path   string
	status int
	body   string
	// retryAfter is what the server asked for, zero when it asked for nothing.
	// It is never trusted as a duration to sleep: post clamps it against the
	// retry budget, which is also the bound on what an impersonated control
	// plane could make a hook wait.
	retryAfter time.Duration
}

func (e *httpError) Error() string {
	return e.path + " returned HTTP " + strconv.Itoa(e.status) + ": " + truncate(e.body, 256)
}

func checkBaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("client: invalid BaseURL: %w", err)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if isLoopbackHost(u.Hostname()) {
			return nil
		}
		return fmt.Errorf("client: refusing plaintext http:// to non-loopback host %q; "+
			"the bearer key would be sent in the clear (INV-1); use https", u.Hostname())
	default:
		return fmt.Errorf("client: BaseURL scheme must be https (or http on loopback), got %q", u.Scheme)
	}
}

func isLoopbackHost(host string) bool {
	switch host {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// setWorkloadHeaders writes the request envelope: Accept, the obx_ API key on
// Authorization, the exchanged workload bearer, the SDK identity, and
// (separately, by the caller) an idempotency key when there is one. It sets
// no DID, signature or body-hash header -- pinned by
// TestEmitSendsWorkloadEnvelopeOnV3, since a request carries no attribution
// coordinate at all (the DID is derived in memory and never sent).
func (c *Client) setWorkloadHeaders(h http.Header, workloadToken string) {
	h.Set("Accept", "application/json")
	h.Set(headerAuthorization, "Bearer "+c.apiKey)
	h.Set(headerWorkloadToken, workloadToken)
	h.Set(headerSDKVersion, workloadauth.SDKVersion)
	h.Set(headerUserAgent, "OpenBox-SDK/"+workloadauth.SDKVersion)
}
