// Package backend is the OpenBox control-plane client used only during
// onboarding (STORY-SL-2). It calls the openbox-backend NestJS API to register
// a developer agent and to look one up for idempotent re-init.
//
// This is NOT the runtime data-plane client — that is the AIP-signed
// /api/v1/governance/evaluate transport owned by STORY-SL-3 (client/). The two
// are deliberately separate:
//
//	backend (here):  POST /agent/create        auth = human/org control-plane
//	                                            credential (Keycloak JWT or org
//	                                            API key). Mints agent identity.
//	client (SL-3):   POST /api/v1/governance/   auth = the AGENT's obx_ key +
//	                 evaluate                    Ed25519 AIP signature minted here.
//
// Verified against openbox-backend (cross-repo explore, 2026-07-08):
//   - Path is POST /agent/create — the backend sets NO global prefix and NO
//     versioning, so there is no /api/v1 here (unlike the core /evaluate path).
//   - Auth is a global JwtAuthGuard accepting either a Keycloak Bearer JWT
//     (which also requires the x-openbox-client header) or an org control-plane
//     key via X-API-Key (obx_key_...). organization_id is derived from the
//     caller identity, never from the body (INV-4).
//   - A minimal valid body is agent_name + icon + full aivss_config; icon is
//     @IsNotEmpty on the DTO.
//   - Success returns {data:{agent, token, identity}} (global TransformInterceptor
//     wraps in `data`). identity.privateKey is the base64 raw 32-byte Ed25519
//     seed, returned exactly once; token is the obx_(live|test)_+48hex runtime
//     key, also once-only.
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

// APIError is a non-2xx response from the backend. The exact status + body are
// preserved so callers can HALT with the precise 4xx (e.g. an agent_type or
// aivss_config rejection — a STORY-SL-2 stop condition).
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
	AgentName   string        `json:"agent_name"`
	AgentType   string        `json:"agent_type"`
	Icon        string        `json:"icon"`
	Description string        `json:"description,omitempty"`
	ModelName   string        `json:"model_name,omitempty"`
	Tags        []string      `json:"tags,omitempty"`
	AivssConfig aivss.Config  `json:"aivss_config"`
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
func (c *Client) FindByName(ctx context.Context, name string) (*AgentSummary, error) {
	var lr listResponse
	if err := c.do(ctx, http.MethodGet, "/agent/list", nil, &lr); err != nil {
		return nil, err
	}
	agents := parseAgentList(lr.Data)
	for i := range agents {
		if strings.EqualFold(agents[i].AgentName, name) {
			return &agents[i], nil
		}
	}
	return nil, nil
}

// parseAgentList tolerates either a bare array or a paginated {items:[...]}.
func parseAgentList(raw json.RawMessage) []AgentSummary {
	if len(raw) == 0 {
		return nil
	}
	var arr []AgentSummary
	if err := json.Unmarshal(raw, &arr); err == nil {
		return arr
	}
	var paged struct {
		Items []AgentSummary `json:"items"`
	}
	if err := json.Unmarshal(raw, &paged); err == nil {
		return paged.Items
	}
	return nil
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
