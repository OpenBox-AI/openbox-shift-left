// Package backend is the OpenBox control-plane client used only during
// onboarding.
//   - Path is POST /agent/create; the backend sets no global prefix and no
//     versioning, so there is no /api/v1 here (unlike the core /evaluate
//     path).
//   - Auth is a global JwtAuthGuard accepting either a Keycloak Bearer JWT
//     (which also requires the x-openbox-client header) or an org control-
//     plane key via X-API-Key (obx_key_...). Organization_id is derived from
//     the caller identity, never from the body (INV-4).
//   - A minimal valid body is agent_name + icon + full aivss_config; icon is
//     @IsNotEmpty on the DTO.
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

	"github.com/openbox-ai/openbox-shift-left/internal/cli/aivss"
)

// APIError is a non-2xx response from the backend.
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

// New builds a control-plane client. The credential is never logged (INV-1).
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

// CreateAgentRequest is the subset of CreateAgentDto the CLI populates.
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
// store; never logged, never written to a config file.
type Registration struct {
	AgentID    string
	AgentName  string
	DID        string
	APIKey     string // obx_(live|test)_+48hex — shown once
	PrivateKey string // base64 raw 32-byte Ed25519 seed — shown once
	Tier       string
	TrustScore string
}

// Create registers a developer agent.
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

// FindByName looks up an existing agent by exact agent_name via GET
// /agent/list for idempotent re-init.
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
// org has none. Which agent does not matter; the probe never names a real
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
// It reports an error rather than returning nil on a shape it cannot read.
func parseAgentList(raw json.RawMessage) ([]AgentSummary, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var arr []AgentSummary
	if err := json.Unmarshal(raw, &arr); err == nil {
		return arr, nil
	}
	var paged struct {
		Items *[]AgentSummary `json:"items"`
		Data  *[]AgentSummary `json:"data"`
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

// Policy is the current per-agent policy read from the control plane. RegoCode
// is intentionally not exposed as a field beyond a presence signal; it is
// never printed/logged (INV-1) and cannot be localized.
type Policy struct {
	ID            string
	UpdatedAt     string
	PolicyBuilder json.RawMessage // config.policy_builder, or nil when absent
	HasRawRego    bool            // rego_code present but no policy_builder → unlocalized
	// Signed is the backend's signature over the authoritative policy, when it
	// serves one (E8-S6 /). Nil from a backend that does not sign yet, which `dev
	// sync` treats as the compatibility path rather than an error.
	Signed *SignedPolicy
}

// SignedPolicy mirrors the response's signature block.
type SignedPolicy struct {
	KeyID        string `json:"key_id"`
	Algorithm    string `json:"algorithm"`
	CanonicalB64 string `json:"canonical_b64"`
	SigB64       string `json:"sig_b64"`
}

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

// GetCurrentPolicy fetches GET /agent/<agentID>/policies/current with the org
// control-plane credential (read:agent_policy is org-scoped; the org key, not
// the agent runtime obx_ key). The org key and the fetched rego are never
// logged (INV-1).
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
