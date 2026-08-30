package backend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// rotate.go — re-issue an existing agent's credentials.
//
// This closes a real dead end: the agent exists remotely, its API key and
// signing key were shown exactly once and are gone, and `init` refuses with
// "already exists … but no local credentials are stored." Deleting and
// re-registering loses the agent's history; rotation keeps its id AND its DID.
//
// The DID survives by construction, not by luck: both identity providers return
// `{did: didFor(agentId), privateKey}`, and didFor derives the DID from the agent
// id — so a rotation cannot change it. KMS mode is not an obstacle either; it
// generates locally and imports, so the private key still comes back.

// rotateAPIKeyResponse tolerates either a bare body or a {status,data:{…}}
// envelope, because the control plane uses both shapes across endpoints.
type rotateAPIKeyResponse struct {
	Token string `json:"token"`
	Data  struct {
		Token string `json:"token"`
	} `json:"data"`
}

func (r rotateAPIKeyResponse) token() string {
	if r.Token != "" {
		return r.Token
	}
	return r.Data.Token
}

// rotateIdentityResponse mirrors the identity rotation reply, envelope-tolerant
// for the same reason.
type rotateIdentityResponse struct {
	DID        string `json:"did"`
	PrivateKey string `json:"privateKey"`
	Data       struct {
		DID        string `json:"did"`
		PrivateKey string `json:"privateKey"`
	} `json:"data"`
}

func (r rotateIdentityResponse) did() string {
	if r.DID != "" {
		return r.DID
	}
	return r.Data.DID
}

func (r rotateIdentityResponse) privateKey() string {
	if r.PrivateKey != "" {
		return r.PrivateKey
	}
	return r.Data.PrivateKey
}

// RotateAPIKey issues a new runtime key for an existing agent and invalidates the
// previous one server-side.
//
// A 2xx reply that carries no token FAILS rather than returning an empty string:
// writing an empty credential over a working one would break the install and look
// like a successful rotation.
func (c *Client) RotateAPIKey(ctx context.Context, agentID string) (string, error) {
	if strings.TrimSpace(agentID) == "" {
		return "", errors.New("rotate api key: no agent id")
	}
	var resp rotateAPIKeyResponse
	if err := c.do(ctx, http.MethodPost, "/agent/"+agentID+"/rotate-api-key", nil, &resp); err != nil {
		return "", rotateError("rotate api key", agentID, err)
	}
	if resp.token() == "" {
		return "", fmt.Errorf("rotate api key: the server accepted the request but returned no `token` field. "+
			"The previous key may already be invalid. Check agent %s in the dashboard before retrying; "+
			"if the field was renamed or stripped by a response serializer, that is an upstream change, not a local one", agentID)
	}
	return resp.token(), nil
}

// RotateIdentity re-provisions an agent's signing identity, returning the
// unchanged DID and a fresh private key.
func (c *Client) RotateIdentity(ctx context.Context, agentID string) (did, privateKey string, err error) {
	if strings.TrimSpace(agentID) == "" {
		return "", "", errors.New("rotate identity: no agent id")
	}
	var resp rotateIdentityResponse
	if err := c.do(ctx, http.MethodPost, "/agent/"+agentID+"/identity/rotate", nil, &resp); err != nil {
		return "", "", rotateError("rotate identity", agentID, err)
	}
	// The fail-loud guard exists because of a specific upstream risk:
	// AgentIdentityResponseDto declares did/key_id/key_arn/public_key but NOT
	// privateKey, which is what the service actually returns. No serializer
	// interceptor strips it today, so it arrives — but if one is ever added, every
	// rotation would silently yield nothing usable. Failing here means a broken
	// deploy produces a clear error instead of an empty credential on disk.
	if resp.privateKey() == "" {
		return "", "", fmt.Errorf("rotate identity: the server accepted the request but returned no `privateKey` field. "+
			"Agent %s may now have a rotated identity this machine cannot sign with — re-run to rotate again, "+
			"and report it upstream if it persists (the response DTO does not declare privateKey, so a newly added "+
			"response serializer would strip it)", agentID)
	}
	if resp.did() == "" {
		return "", "", fmt.Errorf("rotate identity: the server returned a private key but no `did` for agent %s; "+
			"refusing to write a credential whose identity is unknown", agentID)
	}
	return resp.did(), resp.privateKey(), nil
}

// rotateError maps the control plane's failures onto the thing the operator has
// to change.
//
// The 404 case is the one worth the effort: rotateAgentIdentity answers 404 with
// "Agent identity has not been provisioned" when signing_required is null, which
// is NOT "no such agent". Reporting them identically sends someone hunting for an
// agent that exists.
func rotateError(op, agentID string, err error) error {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return fmt.Errorf("%s: %w", op, err)
	}
	switch apiErr.StatusCode {
	case http.StatusUnauthorized:
		return fmt.Errorf("%s: the control plane rejected this credential (401). "+
			"Rotation needs an ORGANIZATION key — `obx_key_…`, from dashboard → Organization → API Keys. "+
			"An agent runtime key (`obx_…`, no `key_`) is not a control-plane credential", op)
	case http.StatusForbidden:
		return fmt.Errorf("%s: this organization key lacks the `update:agent` permission (403). "+
			"Grant it in the dashboard, or ask someone who has it to rotate agent %s", op, agentID)
	case http.StatusNotFound:
		if identityNotProvisioned(apiErr.Body) {
			return fmt.Errorf("%s: agent %s exists but has no signing identity provisioned yet (404). "+
				"That is different from an unknown agent: there is nothing to rotate. "+
				"Provision the agent's identity first, then rotate", op, agentID)
		}
		return fmt.Errorf("%s: no agent %s in this organization (404). "+
			"Check the agent id, and that the key belongs to the same organization as the agent", op, agentID)
	default:
		return fmt.Errorf("%s: %w", op, err)
	}
}

// identityNotProvisioned recognizes the not-provisioned 404 from its message,
// since the status code alone cannot distinguish it from an unknown agent.
func identityNotProvisioned(body string) bool {
	if strings.Contains(strings.ToLower(body), "not been provisioned") {
		return true
	}
	// Same check against a decoded message field, for a body shape that nests it.
	var wrapper struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal([]byte(body), &wrapper); err == nil {
		return strings.Contains(strings.ToLower(wrapper.Message), "not been provisioned")
	}
	return false
}
