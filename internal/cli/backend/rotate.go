package backend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

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

// RotateAPIKey issues a new runtime key for an existing agent and invalidates
// the previous one server-side.
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
	// No serializer interceptor strips it today, so it arrives; but if one is
	// ever added, every rotation would silently yield nothing usable.
	if resp.privateKey() == "" {
		return "", "", fmt.Errorf("rotate identity: the server accepted the request but returned no `privateKey` field. "+
			"Agent %s may now have a rotated identity this machine cannot sign with; re-run to rotate again, "+
			"and report it upstream if it persists (the response DTO does not declare privateKey, so a newly added "+
			"response serializer would strip it)", agentID)
	}
	if resp.did() == "" {
		return "", "", fmt.Errorf("rotate identity: the server returned a private key but no `did` for agent %s; "+
			"refusing to write a credential whose identity is unknown", agentID)
	}
	return resp.did(), resp.privateKey(), nil
}

func rotateError(op, agentID string, err error) error {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return fmt.Errorf("%s: %w", op, err)
	}
	switch apiErr.StatusCode {
	case http.StatusUnauthorized:
		return fmt.Errorf("%s: the control plane rejected this credential (401). "+
			"Rotation needs an ORGANIZATION key; `obx_key_…`, from dashboard → Organization → API Keys. "+
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

func identityNotProvisioned(body string) bool {
	if strings.Contains(strings.ToLower(body), "not been provisioned") {
		return true
	}
	var wrapper struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal([]byte(body), &wrapper); err == nil {
		return strings.Contains(strings.ToLower(wrapper.Message), "not been provisioned")
	}
	return false
}
