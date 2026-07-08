package client

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// evaluatePath is the openbox-core data-plane route (verified: core registers
// it under the /api/v1 prefix, unlike the backend control plane). This exact
// path is also the signed canonical-string PATH component.
const evaluatePath = "/api/v1/governance/evaluate"

// Defaults for the transport. Fail-open (INV-3) means these bound how long a
// caller can be delayed, never whether it proceeds — Emit never blocks the
// caller regardless of outcome.
const (
	defaultTimeout    = 30 * time.Second
	defaultMaxRetries = 2
	defaultRetryBase  = 150 * time.Millisecond
)

// Logger is the minimal sink for fail-open diagnostics. It MUST NOT be used to
// emit secrets or content — the client only ever logs event ids, types, and
// transport errors (INV-1/INV-2). A nil Logger discards.
type Logger interface {
	Printf(format string, args ...any)
}

type nopLogger struct{}

func (nopLogger) Printf(string, ...any) {}

// Config configures a Client. BaseURL, the agent APIKey (obx_), the agent DID,
// and the base64 Ed25519 seed are required; the rest default.
type Config struct {
	BaseURL string // openbox-core base, e.g. https://core.openbox.ai
	APIKey  string // obx_(live|test)_… runtime key (INV-1)
	DID     string // did:aip:… — the developer agent's DID (INV-7)
	SeedB64 string // base64 raw 32-byte Ed25519 seed (INV-1)

	// ContentCaptureEnabled reflects the org's content posture. Default false
	// (metadata-only, INV-2/OD4): the client strips all content before egress.
	ContentCaptureEnabled bool

	HTTP       *http.Client  // optional; a 30s-timeout client is built if nil
	MaxRetries int           // optional; default 2
	RetryBase  time.Duration // optional; default 150ms
	Logger     Logger        // optional; default discards
	now        func() time.Time
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
	maxRetries := cfg.MaxRetries
	if maxRetries == 0 {
		maxRetries = defaultMaxRetries
	}
	retryBase := cfg.RetryBase
	if retryBase == 0 {
		retryBase = defaultRetryBase
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

// Emit builds the core payload from a normalized dev event, signs it, and POSTs
// it to /evaluate. It is FAIL-OPEN (INV-3): on any error (marshal, signing,
// network, non-2xx, exhausted retries) it logs and returns
// (VerdictUnknown, nil) — it NEVER returns a transport error to the caller and
// NEVER blocks a tool call. Phase-1 observe callers ignore the returned verdict.
//
// The returned error is reserved for a programming/precondition fault the
// caller MUST fix (an event that cannot even be built — e.g. empty EventID),
// never a transport failure.
func (c *Client) Emit(ctx context.Context, ev DevEvent) (Verdict, error) {
	// Preconditions the caller MUST fix — not transport failures, so they are
	// surfaced rather than fail-open dropped. EventID is the idempotency key
	// (INV-5); SessionID becomes core's run_id, half of the NOT-NULL session key
	// (workflow_id, run_id) — an empty one would silently corrupt session
	// grouping rather than fail cleanly.
	if ev.EventID == "" {
		return VerdictUnknown, errors.New("client: DevEvent.EventID is required (INV-5 idempotency key)")
	}
	if ev.SessionID == "" {
		return VerdictUnknown, errors.New("client: DevEvent.SessionID is required (maps to core run_id)")
	}

	// INV-2: strip content unless the org opted in. Done on a copy so the
	// caller's event is never mutated.
	if !c.contentOn {
		ev = stripContent(ev)
	}

	body, err := buildPayload(ev)
	if err != nil {
		// A build failure is a data problem, not a transport one; fail-open —
		// log and drop, do not block the caller (INV-3).
		c.log.Printf("openbox: dropping event %s (%s): build payload: %v", ev.EventID, ev.EventType, err)
		return VerdictUnknown, nil
	}

	respBody, err := c.post(ctx, body)
	if err != nil {
		c.log.Printf("openbox: dropping event %s (%s): %v", ev.EventID, ev.EventType, err)
		return VerdictUnknown, nil
	}
	return parseVerdict(respBody), nil
}

// post signs and sends the body with bounded retries. It returns the response
// body on a 2xx, or an error (which Emit converts to a fail-open drop).
func (c *Client) post(ctx context.Context, body []byte) ([]byte, error) {
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
		respBody, retryable, err := c.attempt(ctx, body)
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

// attempt performs one signed POST. retryable reports whether a retry could
// plausibly succeed (network error or 5xx / 429); a 4xx is terminal.
func (c *Client) attempt(ctx context.Context, body []byte) (respBody []byte, retryable bool, err error) {
	sig, err := c.signer.sign(http.MethodPost, evaluatePath, body, c.now())
	if err != nil {
		return nil, false, err // signing failure is not retryable
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+evaluatePath, bytes.NewReader(body))
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
	return nil, retryable, &httpError{status: resp.StatusCode, body: string(rb)}
}

type httpError struct {
	status int
	body   string
}

func (e *httpError) Error() string {
	// The body may echo request context but never our secret (it is only in the
	// Authorization header, never the payload).
	return "evaluate returned HTTP " + itoa(e.status) + ": " + truncate(e.body, 256)
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

// itoa avoids importing strconv for a single small positive int.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
