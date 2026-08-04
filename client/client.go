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
	"time"
)

// evaluatePath is the openbox-core /evaluate route, and the signed
// canonical-string PATH component.
const evaluatePath = "/api/v1/governance/evaluate"

// Transport defaults. Fail-open (INV-3): these bound delay, never whether
// Emit proceeds.
const (
	defaultTimeout    = 30 * time.Second
	defaultMaxRetries = 2
	defaultRetryBase  = 150 * time.Millisecond
)

// Logger sinks fail-open diagnostics: event ids, types, transport errors
// only — never secrets or content (INV-1/INV-2). Nil discards.
type Logger interface {
	Printf(format string, args ...any)
}

type nopLogger struct{}

func (nopLogger) Printf(string, ...any) {}

// Config configures a Client. BaseURL, APIKey, DID, and SeedB64 are required;
// the rest default.
type Config struct {
	BaseURL string // openbox-core base, e.g. https://core.openbox.ai
	APIKey  string // obx_(live|test)_… runtime key (INV-1)
	DID     string // did:aip:… — the developer agent's DID (INV-7)
	SeedB64 string // base64 raw 32-byte Ed25519 seed (INV-1)

	// ContentCaptureEnabled is the org's content posture; default false
	// strips content before egress (INV-2).
	ContentCaptureEnabled bool

	HTTP *http.Client // optional; a 30s-timeout client is built if nil

	// MaxRetries and RetryBase are *T so zero is expressible. As plain values
	// they used zero to mean "unset, use the default", which made
	// "0 retries" impossible to ask for; a negative value then skipped the
	// send loop entirely and returned a nil body with no error, which
	// parsed as VerdictUnknown with nothing logged. Nil ⇒ the default.
	MaxRetries *int
	RetryBase  *time.Duration

	Logger Logger // optional; default discards
	now    func() time.Time
}

// Client is the shared AIP-signed /evaluate transport. It is safe for
// concurrent use.
type Client struct {
	baseURL    string
	apiKey     string
	signer     *signer
	contentOn  bool
	http       *http.Client
	maxRetries int
	retryBase  time.Duration
	log        Logger
	now        func() time.Time
}

// New builds a Client. It fails only on a structurally unusable identity (bad
// seed / missing key) — a construction-time error, distinct from the runtime
// fail-open path. Callers construct once and reuse.
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
	sg, err := newSigner(cfg.DID, cfg.SeedB64)
	if err != nil {
		return nil, err
	}
	httpc := cfg.HTTP
	if httpc == nil {
		httpc = &http.Client{Timeout: defaultTimeout}
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
		baseURL:    trimTrailingSlash(cfg.BaseURL),
		apiKey:     cfg.APIKey,
		signer:     sg,
		contentOn:  cfg.ContentCaptureEnabled,
		http:       httpc,
		maxRetries: maxRetries,
		retryBase:  retryBase,
		log:        log,
		now:        now,
	}, nil
}

// ErrDelivery reports that an event could not be delivered after retries. It is
// advisory (see Emit): the caller is never obliged to act on it, but a caller
// holding a durable copy should keep the event and retry rather than drop it.
var ErrDelivery = errors.New("client: event delivery failed")

// ErrUnbuildable reports that an event could not be serialized at all, so it was
// never sent and retrying it verbatim cannot help.
//
// It is distinct from ErrDelivery on purpose. Emit used to log a build failure
// and return a nil error, which a durable caller reads as "delivered" — so a
// single non-marshalable metadata value (a NaN lifted from a provider payload,
// say) silently dropped the event out of the spool as though it had been
// accepted. Callers should record the drop rather than re-queue.
var ErrUnbuildable = errors.New("client: event could not be built")

// Emit builds the core payload from a normalized dev event, signs it, and POSTs
// it to /evaluate. It is FAIL-OPEN (INV-3): the Evaluation it returns on any
// failure is the zero value, which every caller treats as allow, so a failure
// here can never block a tool call.
//
// The returned error is ADVISORY — it reports what happened, it does not ask
// the caller to stop. Two kinds:
//
//   - a caller precondition (empty EventID/SessionID), which is a bug to fix;
//   - ErrDelivery, wrapping a transport failure after retries.
//
// ErrDelivery exists so a durable caller can retry. Emit used to log the drop
// and return nil, which meant the spool could not tell "delivered" from "lost"
// and had to treat every event as delivered — at-most-once as a data-loss
// guarantee rather than a safety one. Retrying is safe because the payload
// carries a stable Idempotency-Key: the server returns the original verdict for
// a key it has already seen instead of counting the event twice (E8-S7).
//
// Callers that cannot retry (the git action, the Tier-2 escalation) must keep
// ignoring the error and proceed fail-open.
func (c *Client) Emit(ctx context.Context, ev DevEvent) (Evaluation, error) {
	// EventID is the idempotency key (INV-5); SessionID becomes core's run_id.
	// Both must be surfaced, not fail-open dropped — an empty one would
	// silently corrupt session grouping.
	if ev.EventID == "" {
		return Evaluation{}, errors.New("client: DevEvent.EventID is required (INV-5 idempotency key)")
	}
	if ev.SessionID == "" {
		return Evaluation{}, errors.New("client: DevEvent.SessionID is required (maps to core run_id)")
	}

	// INV-2: strip content unless the org opted in. Copy, so the caller's
	// event is never mutated.
	if !c.contentOn {
		ev = stripContent(ev)
	}

	body, err := buildPayload(ev)
	if err != nil {
		// Advisory like every other Emit error — the zero Evaluation below is
		// allow, so this still cannot block a tool call. But it must not read
		// as success: ErrUnbuildable tells a durable caller the event is lost
		// and not worth re-queueing, which returning nil could not.
		c.log.Printf("openbox: dropping event %s (%s): build payload: %v", ev.EventID, ev.EventType, err)
		return Evaluation{}, fmt.Errorf("%w: %v", ErrUnbuildable, err)
	}

	respBody, err := c.post(ctx, evaluatePath, body, ev.EventID)
	if err != nil {
		// Advisory, not a block: the zero Evaluation below is allow. Surfacing
		// ErrDelivery lets a durable caller re-spool the event instead of
		// losing it; callers that cannot retry ignore it (see the doc comment).
		c.log.Printf("openbox: delivery failed for event %s (%s): %s", ev.EventID, ev.EventType, describeDrop(err))
		return Evaluation{}, fmt.Errorf("%w: %s", ErrDelivery, describeDrop(err))
	}
	return parseEvaluation(respBody), nil
}

// post signs and sends the body to path with bounded retries, returning the
// response body on a 2xx. idemKey stays constant across retries (INV-5), so a
// retry after a lost 200 re-sends the same key.
func (c *Client) post(ctx context.Context, path string, body []byte, idemKey string) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if attempt > 0 {
			// Linear backoff: base, 2*base, … Cancellation ends waiting early.
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt) * c.retryBase):
			}
		}
		respBody, retryable, err := c.attempt(ctx, path, body, idemKey)
		if err == nil {
			return respBody, nil
		}
		lastErr = err
		if !retryable {
			return nil, err
		}
	}
	return nil, lastErr
}

// attempt performs one signed POST to path. retryable reports whether a retry
// could plausibly succeed (network error or 5xx / 429); a 4xx is terminal.
//
// path is both the URL suffix and the signed canonical-string PATH component,
// so the two can never disagree — signing one route and sending to another
// would fail authentication in a way that reads as an outage.
func (c *Client) attempt(ctx context.Context, path string, body []byte, idemKey string) (respBody []byte, retryable bool, err error) {
	sig, err := c.signer.sign(http.MethodPost, path, body, c.now())
	if err != nil {
		return nil, false, err // signing failure is not retryable
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, false, err
	}
	// Send the exact signed bytes; Content-Length is set from the reader.
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set(headerAuthorization, "Bearer "+c.apiKey)
	req.Header.Set(headerSDKVersion, sdkVersion)
	req.Header.Set(headerUserAgent, "OpenBox-SDK/"+sdkVersion)
	req.Header.Set(headerAgentDID, c.signer.did)
	req.Header.Set(headerAgentTS, sig.timestamp)
	req.Header.Set(headerAgentNonce, sig.nonce)
	req.Header.Set(headerAgentSig, sig.sig)
	req.Header.Set(headerBodySHA256, sig.bodySHA)
	// Idempotency key (INV-5), not part of the signed canonical string. Empty
	// only if a caller bypassed Emit's guard.
	if idemKey != "" {
		req.Header.Set(headerIdempotencyKey, idemKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, true, err // network/transport error — retryable
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return rb, false, nil
	}
	retryable = resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests
	return nil, retryable, &httpError{path: path, status: resp.StatusCode, body: string(rb)}
}

type httpError struct {
	path   string
	status int
	body   string
}

func (e *httpError) Error() string {
	// The body may echo request context but never our secret (it is only in the
	// Authorization header, never the payload).
	return e.path + " returned HTTP " + strconv.Itoa(e.status) + ": " + truncate(e.body, 256)
}

// checkBaseURL rejects a plaintext http:// core on a non-loopback host: the
// obx_ runtime key rides in the Authorization header and would travel in the
// clear (INV-1). Loopback http is allowed for local development and tests.
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
		return fmt.Errorf("client: refusing plaintext http:// to non-loopback host %q — "+
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

func trimTrailingSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
