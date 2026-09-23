package gitaction

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

//   - Server witness: at construction the verifier calls Witness, which
//     authenticates the deploy's own credential against core, and requires the
//     agent id core names to equal the configured OPENBOX_AGENT_ID. A derived
//     DID can no longer prove this on its own -- it is a pure function of the
//     agent id, so it agrees with itself by construction and proves nothing
//     about which credential is actually authenticating.
//   - Per-row agent_id check: a row is accepted only when its agent_id equals
//     the queried agentID, so a stray/other-agent row can never enter the
//     owned set.

// defaultOwnershipTimeout bounds the ownership read, and the witness call, so
// a slow/absent API degrades to Inferred (NFR reliability) instead of hanging
// the deploy.
const defaultOwnershipTimeout = 5 * time.Second

type apiVerifier struct {
	http      *http.Client
	baseURL   string // backend control-plane origin (no path prefix)
	agentID   string // pusher agent UUID (path key)
	orgAPIKey string // obx_key_ org key (INV-1: X-API-Key header only)
	timeout   time.Duration
	log       Logger

	mu    sync.Mutex
	cache map[string]bool // sessionID -> owned; only definitive answers cached (never errors)
}

// APIVerifierConfig configures the real OwnershipVerifier.
type APIVerifierConfig struct {
	// BaseURL is the openbox-backend control-plane origin (e.g.
	// Https://backend.openbox.ai). It must be a bare origin; no path prefix; and
	// https (or http on loopback for tests): the org key rides in a header
	// (INV-1).
	BaseURL string
	// AgentID is the deploy agent's UUID (the /agent/<id>/sessions path key),
	// normally OPENBOX_AGENT_ID.
	AgentID string
	// Witness authenticates the deploy's own runtime credential against core
	// and reports the agent id core itself attributes that credential to
	// (core's GET /api/v3/auth/validate, via the runtime client's
	// ValidateDetailed). NewAPIVerifier calls it exactly once, before any
	// session read, bounded by Timeout (or defaultOwnershipTimeout), and
	// requires the agent id it returns to name the same principal as AgentID.
	//
	// This replaces binding AgentID to a caller-supplied PusherDID: a DID is a
	// pure function of AgentID, so it agrees with itself by construction and
	// proves nothing about which credential is actually authenticating. The
	// server witness is what makes the check real.
	Witness func(ctx context.Context) (agentID string, err error)
	// OrgAPIKey is an org X-API-Key (obx_key_…) holding read:agent_session
	// (INV-1: never logged; sent only in the X-API-Key header).
	OrgAPIKey string

	Timeout time.Duration // optional; default 5s
	Logger  Logger        // optional; INV-1/INV-2 ids/types/errors only
}

// NewAPIVerifier builds the real OwnershipVerifier. It calls cfg.Witness
// before ever reading a session and requires the agent id it reports to name
// the same principal as cfg.AgentID (case-insensitive UUID compare). A
// mismatch, a witness error (a 401 included), or an unset Witness all refuse
// construction; the caller degrades to NoopVerifier exactly as it does for
// any other misconfiguration (fail-safe: this never over-attributes).
func NewAPIVerifier(cfg APIVerifierConfig) (OwnershipVerifier, error) {
	base := strings.TrimRight(cfg.BaseURL, "/")
	if base == "" {
		return nil, errors.New("ownership verify: backend base URL is required (OPENBOX_OWNERSHIP_API_URL)")
	}
	if err := checkOwnershipBaseURL(base); err != nil {
		return nil, fmt.Errorf("ownership verify: %w", err)
	}
	if cfg.OrgAPIKey == "" {
		return nil, errors.New("ownership verify: org API key is required (OPENBOX_ORG_API_KEY)")
	}
	configuredID, err := uuid.Parse(cfg.AgentID)
	if err != nil {
		return nil, fmt.Errorf("ownership verify: agent id must be a UUID, got %q", cfg.AgentID)
	}
	if cfg.Witness == nil {
		return nil, errors.New("ownership verify: a witness is required to authenticate the configured agent id")
	}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultOwnershipTimeout
	}

	// Bounded, and run before any session read: an ownership check must never
	// trust a caller-asserted identity, only one core itself just confirmed.
	wctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	witnessed, werr := cfg.Witness(wctx)
	if werr != nil {
		return nil, fmt.Errorf("ownership verify: witness: %w", werr)
	}
	witnessedID, err := uuid.Parse(witnessed)
	if err != nil {
		return nil, fmt.Errorf("ownership verify: witness returned a non-UUID agent id %q: %w", witnessed, err)
	}
	if !strings.EqualFold(witnessedID.String(), configuredID.String()) {
		return nil, fmt.Errorf("ownership verify: the witnessed agent %s does not match the configured agent %s "+
			"(refusing to read another principal's sessions)", witnessed, cfg.AgentID)
	}
	return &apiVerifier{
		http: &http.Client{
			Timeout:       timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		baseURL:   base,
		agentID:   cfg.AgentID,
		orgAPIKey: cfg.OrgAPIKey,
		timeout:   timeout,
		cache:     map[string]bool{},
	}, nil
}

// OwnsSession reports whether the pusher's agent owns the session named by a
// trailer claim.
func (v *apiVerifier) OwnsSession(ctx context.Context, sessionID string) (bool, error) {
	v.mu.Lock()
	if owned, ok := v.cache[sessionID]; ok {
		v.mu.Unlock()
		return owned, nil
	}
	v.mu.Unlock()

	owned, err := v.query(ctx, sessionID)
	if err != nil {
		return false, err
	}
	v.mu.Lock()
	v.cache[sessionID] = owned
	v.mu.Unlock()
	return owned, nil
}

func (v *apiVerifier) query(ctx context.Context, sessionID string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, v.timeout)
	defer cancel()

	u := v.baseURL + "/agent/" + v.agentID + "/sessions?search=" + url.QueryEscape(sessionID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return false, fmt.Errorf("ownership read: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-API-Key", v.orgAPIKey) // INV-1: secret only in this header

	resp, err := v.http.Do(req)
	if err != nil {
		// The error carries no secret (the key lives only in the X-API-Key header,
		// never a URL or error).
		return false, fmt.Errorf("ownership read failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false, fmt.Errorf("ownership read: HTTP %d", resp.StatusCode)
	}

	var env struct {
		Data struct {
			Data []struct {
				RunID   string `json:"run_id"`
				AgentID string `json:"agent_id"`
			} `json:"data"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return false, fmt.Errorf("ownership read: malformed response body: %w", err)
	}
	for _, s := range env.Data.Data {
		if s.RunID == sessionID && s.AgentID == v.agentID {
			return true, nil
		}
	}
	return false, nil
}

func checkOwnershipBaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid backend URL: %w", err)
	}
	if u.Path != "" && u.Path != "/" {
		return fmt.Errorf("backend URL must be a bare origin (no path), got path %q", u.Path)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		switch u.Hostname() {
		case "localhost", "127.0.0.1", "::1":
			return nil
		}
		return fmt.Errorf("refusing plaintext http:// to non-loopback host %q; the org key "+
			"would be sent in the clear (INV-1); use https", u.Hostname())
	default:
		return fmt.Errorf("backend URL scheme must be https (or http on loopback), got %q", u.Scheme)
	}
}
