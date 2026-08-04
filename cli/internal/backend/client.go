// Package backend is the OpenBox control-plane client used only during
// onboarding. It calls the openbox-backend NestJS API to register a
// developer agent and to look one up for idempotent re-init.
//
// This is not the runtime data-plane client — that is the AIP-signed
// /api/v1/governance/evaluate transport owned by client/. The two are
// deliberately separate:
//
//	backend (here):  POST /agent/create        auth = human/org control-plane
//	                                            credential (Keycloak JWT or org
//	                                            API key). Mints agent identity.
//	client:          POST /api/v1/governance/   auth = the agent's obx_ key +
//	                 evaluate                    Ed25519 AIP signature minted here.
//
// Verified against openbox-backend:
//   - Path is POST /agent/create — the backend sets no global prefix and no
//     versioning, so there is no /api/v1 here (unlike the core /evaluate
//     path).
//   - Auth is a global JwtAuthGuard accepting either a Keycloak Bearer JWT
//     (which also requires the x-openbox-client header) or an org
//     control-plane key via X-API-Key (obx_key_...). organization_id is
//     derived from the caller identity, never from the body (INV-4).
//   - A minimal valid body is agent_name + icon + full aivss_config; icon
//     is @IsNotEmpty on the DTO.
//   - Success returns {data:{agent, token, identity}} (global
//     TransformInterceptor wraps in `data`). identity.privateKey is the
//     base64 raw 32-byte Ed25519 seed, returned exactly once; token is the
//     obx_(live|test)_+48hex runtime key, also once-only.
package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/openbox-ai/openbox-shift-left/cli/internal/aivss"
)

// APIError is a non-2xx response from the backend. The exact status + body
// are preserved so callers can HALT with the precise 4xx (e.g. an
// agent_type or aivss_config rejection).
type APIError struct {
	StatusCode int
	Body       string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("backend returned HTTP %d: %s", e.StatusCode, strings.TrimSpace(e.Body))
}

// Client talks to the openbox-backend control plane.
type Client struct {
	BaseURL    string
	HTTP       *http.Client
	authHeader string // "Authorization" or "X-API-Key"
	authValue  string
	clientID   string // value for the x-openbox-client header (JWT path only)
}

// New builds a control-plane client. The credential is auto-classified: an
// org control-plane key (obx_key_...) is sent as X-API-Key; anything else is
// treated as a Keycloak Bearer JWT (and paired with the required
// x-openbox-client header). The credential is never logged (INV-1).
func New(baseURL, credential, clientID string) *Client {
	c := &Client{
		BaseURL:  strings.TrimRight(baseURL, "/"),
		HTTP:     &http.Client{Timeout: 30 * time.Second},
		clientID: clientID,
	}
	if strings.HasPrefix(credential, "obx_key_") {
		c.authHeader, c.authValue = "X-API-Key", credential
	} else {
		c.authHeader, c.authValue = "Authorization", "Bearer "+credential
	}
	return c
}

// CreateAgentRequest is the subset of CreateAgentDto the CLI populates. Fields
// the backend defaults (team_ids, attestation_mode=kms) are omitted.
type CreateAgentRequest struct {
	AgentName   string         `json:"agent_name"`
	AgentType   string         `json:"agent_type"`
	Icon        string         `json:"icon"`
	Description string         `json:"description,omitempty"`
	ModelName   string         `json:"model_name,omitempty"`
	Tags        []string       `json:"tags,omitempty"`
	AivssConfig aivss.Config   `json:"aivss_config"`
	Config      map[string]any `json:"config,omitempty"`
}

// createResponse mirrors {data:{agent, token, identity}}.
type createResponse struct {
	Data struct {
		Agent    agentBody `json:"agent"`
		Token    string    `json:"token"`
		Identity struct {
			DID        string `json:"did"`
			PrivateKey string `json:"privateKey"`
		} `json:"identity"`
	} `json:"data"`
}

type agentBody struct {
	ID             string `json:"id"`
	AgentName      string `json:"agent_name"`
	AgentType      string `json:"agent_type"`
	DID            string `json:"did"`
	OrganizationID string `json:"organization_id"`
	Tier           string `json:"tier"`
	TrustScore     any    `json:"trust_score"`
}

// Registration is the credential material captured from a successful create.
// APIKey and PrivateKey are secrets (INV-1) and must go straight to the secret
// store — never logged, never written to a config file.
type Registration struct {
	AgentID    string
	AgentName  string
	DID        string
	APIKey     string // obx_(live|test)_+48hex — shown once
	PrivateKey string // base64 raw 32-byte Ed25519 seed — shown once
	Tier       string
	TrustScore string
}

// Create registers a developer agent. It returns *APIError on a non-2xx so the
// caller can inspect the exact status/body for HALT handling.
func (c *Client) Create(ctx context.Context, req CreateAgentRequest) (*Registration, error) {
	var out createResponse
	if err := c.do(ctx, http.MethodPost, "/agent/create", req, &out); err != nil {
		return nil, err
	}
	did := out.Data.Identity.DID
	if did == "" {
		did = out.Data.Agent.DID // fall back to the agent body's did
	}
	reg := &Registration{
		AgentID:    out.Data.Agent.ID,
		AgentName:  out.Data.Agent.AgentName,
		DID:        did,
		APIKey:     out.Data.Token,
		PrivateKey: out.Data.Identity.PrivateKey,
		Tier:       out.Data.Agent.Tier,
		TrustScore: fmt.Sprint(out.Data.Agent.TrustScore),
	}
	return reg, nil
}

// AgentSummary is the identifying subset of a listed agent (no secrets).
type AgentSummary struct {
	ID             string `json:"id"`
	AgentName      string `json:"agent_name"`
	AgentType      string `json:"agent_type"`
	DID            string `json:"did"`
	OrganizationID string `json:"organization_id"`
}

type listResponse struct {
	Data json.RawMessage `json:"data"`
}

// FindByName looks up an existing agent by exact agent_name via GET /agent/list
// for idempotent re-init. Returns (nil, nil) when none matches. The list shape
// varies (array, or {items:[...]}), so it is parsed defensively.
//
// all=true asks the control plane for the unpaginated list. Without it the
// endpoint returns the first page only, so in an organization with more agents
// than fit on one page the duplicate you are looking for may simply not be in
// the response — and "not on page one" would read as "does not exist".
func (c *Client) FindByName(ctx context.Context, name string) (*AgentSummary, error) {
	var lr listResponse
	if err := c.do(ctx, http.MethodGet, "/agent/list?all=true", nil, &lr); err != nil {
		return nil, err
	}
	agents, err := parseAgentList(lr.Data)
	if err != nil {
		return nil, err
	}
	for i := range agents {
		if strings.EqualFold(agents[i].AgentName, name) {
			return &agents[i], nil
		}
	}
	return nil, nil
}

// FirstAgentID returns any agent in the caller's organization, or "" when the
// org has none.
//
// It exists for the approver's install-time permission probe: the decide route
// is per-agent and refuses an agent outside the caller's org with the same 403
// it uses for a missing permission, so probing needs a real agent to tell the
// two apart. Which agent does not matter — the probe never names a real
// approval.
func (c *Client) FirstAgentID(ctx context.Context) (string, error) {
	var lr listResponse
	if err := c.do(ctx, http.MethodGet, "/agent/list?all=true", nil, &lr); err != nil {
		return "", err
	}
	agents, err := parseAgentList(lr.Data)
	if err != nil {
		return "", err
	}
	if len(agents) == 0 {
		return "", nil
	}
	return agents[0].ID, nil
}

// parseAgentList tolerates either a bare array or a paginated {items:[...]}.
//
// It reports an error rather than returning nil on a shape it cannot read.
// Swallowing the failure turned "the list could not be parsed" into "no such
// agent exists", which is precisely the confusion devinit's lookup guard exists
// to prevent: a duplicate would go unnoticed, non-force init would proceed into
// an opaque 400, and --force would pick an already-taken name.
func parseAgentList(raw json.RawMessage) ([]AgentSummary, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var arr []AgentSummary
	if err := json.Unmarshal(raw, &arr); err == nil {
		return arr, nil
	}
	// *[]T, not []T: decoding an object into a struct ignores fields it does
	// not know about, so a plain slice would make *any* JSON object look like
	// a successful parse of an empty page. The pointer distinguishes "items
	// was present" from "this is some other shape entirely".
	var paged struct {
		Items *[]AgentSummary `json:"items"`
		// openbox-backend@develop paginates as {data:{data:[…],…}} — the
		// envelope's data field holds another data array.
		Data *[]AgentSummary `json:"data"`
	}
	if err := json.Unmarshal(raw, &paged); err == nil {
		if paged.Items != nil {
			return *paged.Items, nil
		}
		if paged.Data != nil {
			return *paged.Data, nil
		}
	}
	return nil, fmt.Errorf("agent list: unrecognized response shape (neither an array, {items:[…]}, nor {data:[…]})")
}

// Policy is the current per-agent policy read from the control plane
// (ADR-0005). It is the subset of openbox-backend's PolicyEntity the CLI
// needs to translate into a local policy bundle: the pin (ID + UpdatedAt),
// the structured config.policy_builder (when the policy was authored in the
// builder UI), and whether raw rego is present (the fidelity-residual
// case). RegoCode is intentionally not exposed as a field beyond a presence
// signal — it is never printed/logged (INV-1) and cannot be localized.
type Policy struct {
	ID            string
	UpdatedAt     string
	PolicyBuilder json.RawMessage // config.policy_builder, or nil when absent
	HasRawRego    bool            // rego_code present but no policy_builder → unlocalized
	// Signed is the backend's signature over the authoritative policy, when it
	// serves one (E8-S6 / ADR-0008). nil from a backend that does not sign yet,
	// which `dev sync` treats as the compatibility path rather than an error.
	Signed *SignedPolicy
}

// SignedPolicy mirrors the response's signature block. It is carried verbatim to
// the decision module for verification — the CLI does not interpret it, so the
// bytes that get verified are the bytes the backend signed.
type SignedPolicy struct {
	KeyID        string `json:"key_id"`
	Algorithm    string `json:"algorithm"`
	CanonicalB64 string `json:"canonical_b64"`
	SigB64       string `json:"sig_b64"`
}

// policyEnvelope decodes {status, data: PolicyEntity|null}. The global
// TransformInterceptor wraps every response as {status, data}; a
// no-current-policy read is HTTP 200 with data:null (not 404).
type policyEnvelope struct {
	Data *policyEntity `json:"data"`
}

type policyEntity struct {
	ID        string          `json:"id"`
	RegoCode  string          `json:"rego_code"`
	Config    json.RawMessage `json:"config"`
	UpdatedAt string          `json:"updated_at"`
	Signed    *SignedPolicy   `json:"signed"`
}

// GetCurrentPolicy fetches GET /agent/<agentID>/policies/current with the
// org control-plane credential (read:agent_policy is org-scoped — the org
// key, not the agent runtime obx_ key). It returns (nil, nil) when the
// agent has no current policy (data:null → an allow / no-policy state), so
// the caller writes a no-policy (allow) bundle. On a non-2xx it returns
// *APIError so the caller can map an auth/permission failure to a hint. The
// org key and the fetched rego are never logged (INV-1).
func (c *Client) GetCurrentPolicy(ctx context.Context, agentID string) (*Policy, error) {
	if strings.TrimSpace(agentID) == "" {
		return nil, fmt.Errorf("agent id is required to read the current policy")
	}
	var env policyEnvelope
	if err := c.do(ctx, http.MethodGet, "/agent/"+agentID+"/policies/current", nil, &env); err != nil {
		return nil, err
	}
	if env.Data == nil {
		return nil, nil // no current policy → allow / no-policy bundle
	}
	p := &Policy{ID: env.Data.ID, UpdatedAt: env.Data.UpdatedAt, Signed: env.Data.Signed}
	if pb := extractPolicyBuilder(env.Data.Config); pb != nil {
		p.PolicyBuilder = pb
	} else if strings.TrimSpace(env.Data.RegoCode) != "" {
		p.HasRawRego = true // raw rego with no builder config → unlocalized residual
	}
	return p, nil
}

// extractPolicyBuilder pulls config.policy_builder out of the PolicyEntity.config
// jsonb, returning nil when absent/empty. It parses only the ONE key it needs;
// the rest of config (including config.path) is ignored.
func extractPolicyBuilder(config json.RawMessage) json.RawMessage {
	if len(config) == 0 {
		return nil
	}
	var wrapper struct {
		PolicyBuilder json.RawMessage `json:"policy_builder"`
	}
	if err := json.Unmarshal(config, &wrapper); err != nil {
		return nil
	}
	if len(wrapper.PolicyBuilder) == 0 || string(wrapper.PolicyBuilder) == "null" {
		return nil
	}
	return wrapper.PolicyBuilder
}

// do performs one request, applying auth headers and decoding the JSON body.
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal request: %w", err)
		}
		rdr = bytes.NewReader(b)
	}
	httpReq, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, rdr)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set(c.authHeader, c.authValue)
	if c.authHeader == "Authorization" && c.clientID != "" {
		httpReq.Header.Set("x-openbox-client", c.clientID)
	}
	if body != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	httpReq.Header.Set("Accept", "application/json")

	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &APIError{StatusCode: resp.StatusCode, Body: string(respBody)}
	}
	if out != nil {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("decode %s response: %w", path, err)
		}
	}
	return nil
}
