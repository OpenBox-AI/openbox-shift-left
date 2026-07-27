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

// apiVerifier is the real OwnershipVerifier: it asks openbox-backend (the
// control plane) which sessions the pusher's agent owns, and promotes a
// claim only when the trailer value names one of them.
//
// # The read
//
// There is no DID-keyed session-ownership endpoint, and a DID cannot be
// reversed to an agent id (did:aip:<uuidv5(agentID, namespace)> is
// one-way). So the deploy agent's own agent UUID is supplied directly to CI
// (OPENBOX_AGENT_ID — known at agent-create time), and the verifier reads
// the existing, org-scoped endpoint:
//
//	GET <backend>/agent/<agentID>/sessions?search=<sessionID>
//	    X-API-Key: <obx_key_… org key with read:agent_session>
//	200 → { "data": [ { "run_id": "<session id>", "agent_id": "<agentID>", … } ] }
//
// The endpoint double-scopes by agentID and the key's organization_id
// server-side, so a 200 row genuinely belongs to that agent in that org.
// The trailer value is owned iff a row's run_id equals it.
//
// # Matching on run_id, not id
//
// The `OpenBox-Session:` trailer value is the tool's session UUID, which
// the shift-left client writes to core's run_id (client/payload.go: RunID =
// ev.SessionID), not the backend SessionEntity `id` PK. So ownership
// matches on run_id. Matching `id` would silently never attribute — the
// load-bearing detail.
//
// # INV-4, discharged for real
//
//   - agentID↔DID binding: at construction the verifier recomputes
//     did:aip:<uuidv5(agentID, namespace)> and requires it to equal the
//     deploy agent's DID (OPENBOX_DID). A misconfigured agent id that
//     names a different principal than the deploy DID is rejected — the
//     sessions it reads always belong to the same identity the deploy is
//     attributed to.
//   - per-row agent_id check: a row is accepted only when its agent_id
//     equals the queried agentID, so a stray/other-agent row can never
//     enter the owned set.
//   - a forged trailer naming a victim's session id (visible in the
//     victim's pushed commits) is not returned for the pusher's agent →
//     stays Inferred.
//
// # Fail-closed (INV-6)
//
// Every fault path — transport error, non-2xx, malformed/ambiguous body,
// timeout, no matching row — resolves to (false, err) or (false, nil): the
// claim is not promoted. A lookup failure never over-attributes; the worst
// case is honest under-attribution (Inferred), never a silent wrong
// Attributed.
//
// # Rollout
//
// Off by default (NoopVerifier). Enabled by OPENBOX_OWNERSHIP_VERIFY=1 plus
// the backend URL, agent id, and org key. A misconfigured/unreachable
// verifier degrades to Noop — it never breaks CI and never over-attributes.

// aipNamespace mirrors openbox-backend's OPENBOX_AIP_NAMESPACE; the backend
// derives an agent's DID as did:aip:<uuidv5(agentID, aipNamespace)>. It is
// used only to bind a supplied agent id to the deploy DID (see
// NewAPIVerifier). If the backend ever changes this derivation the bind
// fails → the verifier degrades to Noop (fail-safe: it never
// over-attributes on a mismatch).
const aipNamespace = "b6e4a1d3-7c02-4e8a-9d1f-5a3b7c2d8e0f"

// defaultOwnershipTimeout bounds the ownership read so a slow/absent API degrades
// to Inferred (NFR reliability) instead of hanging the deploy. It never changes
// the outcome's safety — a timeout is a fault, and a fault fails closed.
const defaultOwnershipTimeout = 5 * time.Second

// apiVerifier is the real, flag-gated OwnershipVerifier. It answers each claim by
// searching the pusher agent's sessions for a matching run_id, and caches the
// definitive per-session result for the life of one deploy resolution.
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
	// https://backend.openbox.ai). It MUST be a bare origin — no path prefix — and
	// https (or http on loopback for tests): the org key rides in a header (INV-1).
	BaseURL string
	// AgentID is the deploy agent's UUID (the /agent/<id>/sessions path key). It is
	// bound to PusherDID via uuidv5 at construction.
	AgentID string
	// PusherDID is the deploy agent's did:aip:<uuid> (the signing/attribution
	// identity). AgentID must derive to exactly this DID.
	PusherDID string
	// OrgAPIKey is an org X-API-Key (obx_key_…) holding read:agent_session (INV-1:
	// never logged; sent only in the X-API-Key header).
	OrgAPIKey string

	Timeout time.Duration // optional; default 5s
	Logger  Logger        // optional; INV-1/INV-2 ids/types/errors only
}

// NewAPIVerifier builds the real OwnershipVerifier. It fails (so the caller can
// fall back to NoopVerifier) only on a structurally unusable config: a missing/
// non-https base URL, a path-unsafe or DID-mismatched agent id, or a missing org
// key. A runtime lookup fault is NOT a construction error; it surfaces per-call
// as fail-closed (false, err).
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
	// The agent id is interpolated raw into the request path; a UUID is hex+hyphen,
	// so validate positively against that charset (rejects /, ?, #, %, whitespace,
	// CRLF — no path break, desync, or header injection).
	if !isUUIDShaped(cfg.AgentID) {
		return nil, fmt.Errorf("ownership verify: agent id must be a UUID, got %q", cfg.AgentID)
	}
	// INV-4 binding: the supplied agent id MUST derive to the deploy DID, so the
	// sessions read always belong to the same principal the deploy is attributed
	// to. A mismatch means a misconfigured pair (wrong agent id or wrong DID).
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
		// Never follow a redirect: this is a plain JSON GET with no legitimate
		// 3xx, and Go forwards custom headers (our X-API-Key) across a
		// cross-host redirect (it only strips Authorization/Cookie). A 302
		// → http://evil would otherwise leak the broad org key past
		// checkOwnershipBaseURL's origin/scheme guard. ErrUseLastResponse
		// surfaces the 3xx as a non-2xx → fail-closed.
		http: &http.Client{
			Timeout:       timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		baseURL: base,
		agentID:   cfg.AgentID,
		orgAPIKey: cfg.OrgAPIKey,
		timeout:   timeout,
		log:       cfg.Logger,
		cache:     map[string]bool{},
	}, nil
}

// OwnsSession reports whether the pusher's agent owns the session named by
// a trailer claim. It returns (true, nil) only on positively-established
// ownership; (false, nil) when provably not owned; and (false, err) on any
// lookup fault — the resolver treats the latter two identically (never
// promoted), so a fault can never over-attribute.
func (v *apiVerifier) OwnsSession(ctx context.Context, sessionID string) (bool, error) {
	v.mu.Lock()
	if owned, ok := v.cache[sessionID]; ok {
		v.mu.Unlock()
		return owned, nil
	}
	v.mu.Unlock()

	owned, err := v.query(ctx, sessionID)
	if err != nil {
		// Fail closed and do NOT cache: a transient fault on one claim must not
		// poison a later claim that might read cleanly.
		return false, err
	}
	v.mu.Lock()
	v.cache[sessionID] = owned
	v.mu.Unlock()
	return owned, nil
}

// query performs one ownership read for a single session id.
func (v *apiVerifier) query(ctx context.Context, sessionID string) (bool, error) {
	// Bound the read (NFR reliability): a slow/absent API becomes a fault, not a
	// hang. The http.Client timeout also bounds it; this covers ctx cancellation.
	ctx, cancel := context.WithTimeout(ctx, v.timeout)
	defer cancel()

	// agentID is UUID-validated at construction (path-safe). Escape the untrusted
	// trailer value in the query so it can never break the URL; an ILIKE wildcard
	// in it can only broaden the server match — we still require EXACT run_id
	// equality below, so a broadened match cannot promote a different id.
	u := v.baseURL + "/agent/" + v.agentID + "/sessions?search=" + url.QueryEscape(sessionID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return false, fmt.Errorf("ownership read: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-API-Key", v.orgAPIKey) // INV-1: secret only in this header

	resp, err := v.http.Do(req)
	if err != nil {
		// Transport fault → fail closed. The error carries no secret (the key lives
		// only in the X-API-Key header, never a URL or error).
		return false, fmt.Errorf("ownership read failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// A non-2xx (auth reject, 404, 5xx) is a lookup fault, not proof of
		// non-ownership — but either way the claim is not promoted (fail-closed).
		return false, fmt.Errorf("ownership read: HTTP %d", resp.StatusCode)
	}

	// openbox-backend wraps every 2xx in a global envelope
	// {status, data:<payload>}; the sessions payload is itself
	// {data:[…], …pagination} (SessionListResponseDto). So the session rows
	// live at .data.data[]. Parse run_id + agent_id only (INV-2: no content
	// read; unknown fields ignored).
	var env struct {
		Data struct {
			Data []struct {
				RunID   string `json:"run_id"`
				AgentID string `json:"agent_id"`
			} `json:"data"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		// An ambiguous/malformed body must never be read as "owned" — fail closed.
		return false, fmt.Errorf("ownership read: malformed response body: %w", err)
	}
	for _, s := range env.Data.Data {
		// Exact run_id match (the search is a substring ILIKE server-side, so a
		// broadened match cannot promote a different id), AND the row must belong to
		// the agent we queried (INV-4 defense-in-depth over the server scoping). The
		// agent_id check is unconditional: a row missing it is not proof of ownership.
		if s.RunID == sessionID && s.AgentID == v.agentID {
			return true, nil
		}
	}
	return false, nil
}

// checkOwnershipBaseURL rejects a plaintext http:// backend on a non-loopback
// host: the org key rides in a header and would travel in the clear (INV-1).
// Loopback http is allowed for local development and tests.
func checkOwnershipBaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid backend URL: %w", err)
	}
	// Must be a bare origin: the request path is built as /agent/<id>/sessions, so a
	// base path prefix would produce a wrong (fail-safe 404) URL — reject it loudly.
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

// isUUIDShaped reports whether s is a canonical 8-4-4-4-12 hex UUID (the shape of
// an AgentEntity id). A positive charset+shape check keeps the id safe to
// interpolate raw into a URL path.
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
// did:aip:<uuidv5(agentID, aipNamespace)> (RFC 4122 v5, SHA-1). It lets the
// verifier bind a supplied agent id to the deploy DID (INV-4) without a network
// lookup — the DID is a pure function of the id. Exported so the CI entrypoint
// can derive/verify the deploy agent's DID from OPENBOX_AGENT_ID.
func DIDForAgent(agentID string) (string, error) {
	id, err := uuidV5(aipNamespace, agentID)
	if err != nil {
		return "", err
	}
	return "did:aip:" + id, nil
}

// uuidV5 computes the RFC 4122 v5 (SHA-1, name-based) UUID of name within the
// given namespace UUID, byte-for-byte as the JS `uuid` library's v5(name,
// namespace) does: SHA-1 over namespaceBytes(16) ++ utf8(name), first 16 bytes,
// with the version (5) and variant (RFC 4122) bits set. Rendered lowercase
// canonical.
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

// parseUUIDBytes decodes a canonical hyphenated UUID string to its 16 bytes.
func parseUUIDBytes(s string) ([]byte, error) {
	hexStr := strings.ReplaceAll(s, "-", "")
	if len(hexStr) != 32 {
		return nil, fmt.Errorf("not a UUID: %q", s)
	}
	return hex.DecodeString(hexStr)
}

// formatUUID renders 16 bytes as a canonical lowercase 8-4-4-4-12 UUID.
func formatUUID(u [16]byte) string {
	h := hex.EncodeToString(u[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}
