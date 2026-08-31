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
)

const evaluatePath = "/api/v1/governance/evaluate"

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
	BaseURL       string // openbox-core base, e.g. https://core.openbox.ai
	APIKey        string // obx_(live|test)_… runtime key (INV-1)
	DID           string // did:aip:…; the developer agent's DID (INV-7)
	PrivateKeyB64 string // base64 raw 32-byte Ed25519 seed (INV-1)

	// ContentCaptureEnabled is the org's content posture; default false strips
	// content before egress (INV-2).
	ContentCaptureEnabled bool

	HTTP *http.Client // optional; a 30s-timeout client is built if nil

	// MaxRetries and RetryBase are *T so zero is expressible.
	MaxRetries *int
	RetryBase  *time.Duration

	Logger Logger // optional; default discards
	now    func() time.Time
}

// Client is the shared AIP-signed /evaluate transport.
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
	sg, err := newSigner(cfg.DID, cfg.PrivateKeyB64)
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

// ErrDelivery reports that an event could not be delivered after retries. It
// is advisory (see Emit): the caller is never obliged to act on it, but a
// caller holding a durable copy should keep the event and retry rather than
// drop it.
var ErrDelivery = errors.New("client: event delivery failed")

// ErrUnbuildable reports that an event could not be serialized at all, so it
// was never sent and retrying it verbatim cannot help. It is distinct from
// ErrDelivery on purpose.
var ErrUnbuildable = errors.New("client: event could not be built")

// Emit builds the core payload from a normalized dev event, signs it, and
// POSTs it to /evaluate. It is fail-open (INV-3): the Evaluation it returns on
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
		return Evaluation{}, errors.New("client: DevEvent.SessionID is required (maps to core run_id)")
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
		return Evaluation{}, fmt.Errorf("%w: %s", ErrDelivery, describeDrop(err))
	}
	return parseEvaluation(respBody), nil
}

// retryBudget is the delay budget the linear schedule this replaced would have
// spent: sum(i*retryBase) for i in 1..maxRetries. Derived rather than
// configured, so "no longer than before" holds for whatever MaxRetries and
// RetryBase a caller sets and there is no knob to grow it through by accident.
//
// It bounds the delays only, never the attempts. Counting request time toward
// it would silently drop a retry whenever the control plane was slow, which is
// exactly when the retry is worth having.
func (c *Client) retryBudget() time.Duration {
	n := time.Duration(c.maxRetries)
	return n * (n + 1) / 2 * c.retryBase
}

// post delivers one signed request, retrying a retryable failure.
//
// The delays are exponential with full jitter rather than the arithmetic ramp
// they replaced. Every hook process on every concurrent session used to retry
// on the same deterministic 150/300ms schedule, so a control plane coming back
// from an outage met the whole fleet in lockstep; jitter is what spreads it.
//
// Retry-After is honoured as a stop signal, not as a sleep. A server asking
// for longer than the budget gets what it actually wants -- no more requests
// -- while the event returns ErrDelivery and the caller re-spools it for the
// next flush. Waiting it out inline would buy nothing the spool does not
// already provide, and INV-3 says a hook must not hold the tool call open.
func (c *Client) post(ctx context.Context, path string, body []byte, idemKey string) ([]byte, error) {
	// Each delay is drawn from [0, 2*interval], and the intervals are
	// retryBase/2 then retryBase thereafter, so the worst-case sum is
	// base*(2n-1) against the ramp's base*n(n+1)/2 -- equal at the default two
	// retries, below it for more. The expected sum is half the old schedule.
	b := backoff.NewExponentialBackOff()
	b.InitialInterval = c.retryBase / 2
	b.RandomizationFactor = 1
	b.Multiplier = 2
	b.MaxInterval = c.retryBase

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
			if he.retryAfter > c.retryBudget() {
				// Longer than this client will ever pause. Giving up is the half of
				// Retry-After that protects the server, and it is the half that
				// matters: ErrDelivery re-spools the event for the next flush.
				return nil, backoff.Permanent(err)
			}
			return nil, &backoff.RetryAfterError{Duration: he.retryAfter}
		}
		return nil, err
	},
		backoff.WithBackOff(b),
		backoff.WithMaxTries(uint(c.maxRetries)+1),
	)
}

// attempt path is both the URL suffix and the signed canonical-string PATH
// component, so the two can never disagree; signing one route and sending to
// another would fail authentication in a way that reads as an outage.
func (c *Client) attempt(ctx context.Context, path string, body []byte, idemKey string) (respBody []byte, retryable bool, err error) {
	sig, err := c.signer.sign(http.MethodPost, path, body, c.now())
	if err != nil {
		return nil, false, err // signing failure is not retryable
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, false, err
	}
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
