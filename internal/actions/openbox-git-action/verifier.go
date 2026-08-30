package gitaction

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// There is no DID-keyed session-ownership endpoint, and a DID cannot be
// reversed to an agent id (did:aip:<uuidv5(agentID, namespace)> is one-way).

// aipNamespace mirrors openbox-backend's OPENBOX_AIP_NAMESPACE; the backend
// derives an agent's DID as did:aip:<uuidv5(agentID, aipNamespace)>.
const aipNamespace = "b6e4a1d3-7c02-4e8a-9d1f-5a3b7c2d8e0f"

// defaultOwnershipTimeout bounds the ownership read so a slow/absent API
// degrades to Inferred (NFR reliability) instead of hanging the deploy.
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
	// AgentID is the deploy agent's UUID (the /agent/<id>/sessions path key).
	AgentID string
	// PusherDID is the deploy agent's did:aip:<uuid> (the signing/attribution
	// identity). AgentID must derive to exactly this DID.
	PusherDID string
	// OrgAPIKey is an org X-API-Key (obx_key_…) holding read:agent_session
	// (INV-1: never logged; sent only in the X-API-Key header).
	OrgAPIKey string

	Timeout time.Duration // optional; default 5s
	Logger  Logger        // optional; INV-1/INV-2 ids/types/errors only
}

// NewAPIVerifier builds the real OwnershipVerifier.
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
	if !isUUIDShaped(cfg.AgentID) {
		return nil, fmt.Errorf("ownership verify: agent id must be a UUID, got %q", cfg.AgentID)
	}
	derived, err := DIDForAgent(cfg.AgentID)
	if err != nil {
		return nil, fmt.Errorf("ownership verify: %w", err)
	}
	if derived != cfg.PusherDID {
		return nil, fmt.Errorf("ownership verify: agent id %q derives to %s, not the deploy DID %q "+
			"(refusing to read another principal's sessions)", cfg.AgentID, derived, cfg.PusherDID)
	}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultOwnershipTimeout
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
		return fmt.Errorf("refusing plaintext http:// to non-loopback host %q — the org key "+
			"would be sent in the clear (INV-1); use https", u.Hostname())
	default:
		return fmt.Errorf("backend URL scheme must be https (or http on loopback), got %q", u.Scheme)
	}
}

func isUUIDShaped(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			isHex := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
			if !isHex {
				return false
			}
		}
	}
	return true
}

// DIDForAgent recomputes the agent's DID as openbox-backend does:
// did:aip:<uuidv5(agentID, aipNamespace)> (RFC 4122 v5, SHA-1).
func DIDForAgent(agentID string) (string, error) {
	id, err := uuidV5(aipNamespace, agentID)
	if err != nil {
		return "", err
	}
	return "did:aip:" + id, nil
}

func uuidV5(namespace, name string) (string, error) {
	ns, err := parseUUIDBytes(namespace)
	if err != nil {
		return "", fmt.Errorf("parse namespace UUID: %w", err)
	}
	h := sha1.New()
	h.Write(ns)
	h.Write([]byte(name)) // name hashed as its utf8 bytes (the agent id STRING)
	sum := h.Sum(nil)
	var u [16]byte
	copy(u[:], sum[:16])
	u[6] = (u[6] & 0x0f) | 0x50 // version 5
	u[8] = (u[8] & 0x3f) | 0x80 // RFC 4122 variant
	return formatUUID(u), nil
}

func parseUUIDBytes(s string) ([]byte, error) {
	hexStr := strings.ReplaceAll(s, "-", "")
	if len(hexStr) != 32 {
		return nil, fmt.Errorf("not a UUID: %q", s)
	}
	return hex.DecodeString(hexStr)
}

func formatUUID(u [16]byte) string {
	h := hex.EncodeToString(u[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}
