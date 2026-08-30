package backend

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// It is deliberately a client and nothing more: no listener, no socket, no
// daemon.

// Approval is one pending request in the org's queue. With capture off it is
// absent and the queue cannot show what was never sent.
type Approval struct {
	ID string `json:"id"`
	// AgentID owns the request; the decide route is per-agent.
	AgentID string `json:"agent_id"`
	// Agent is the nested agent object the list embeds.
	Agent     *ApprovalAgent `json:"agent"`
	AgentName string         `json:"agent_name"`
	// ActivityType is the tool (e.g.
	ActivityType string `json:"activity_type"`
	// Input is the activity_input the gated event carried: tool kind and
	// identifiers always, and; with content capture on; the command, the MCP
	// arguments or the file body under `command`/`arguments`/`content`.
	Input     map[string]any `json:"input"`
	Reason    *string        `json:"reason"`
	ExpiresAt *time.Time     `json:"approval_expired_at"`
	CreatedAt *time.Time     `json:"created_at"`
}

// ApprovalAgent is the subset of the embedded agent object worth showing.
type ApprovalAgent struct {
	ID   string `json:"id"`
	Name string `json:"agent_name"`
}

// Name is the agent's display name from wherever the response carried it.
func (a Approval) Name() string {
	if a.Agent != nil && a.Agent.Name != "" {
		return a.Agent.Name
	}
	return a.AgentName
}

// Request is what the call is asking to do; the command for a shell tool, the
// arguments for an MCP one; and it is the field an approval is actually
// decided on.
func (a Approval) Request() string {
	if a.Input == nil {
		return ""
	}
	for _, k := range []string{"command", "arguments"} {
		if v, ok := a.Input[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// Context is the structural remainder; the identifiers that locate the call
// without describing it. Rendered separately from Request so the thing being
// judged is never mixed in with the thing merely identifying it.
func (a Approval) Context() map[string]any {
	if a.Input == nil {
		return nil
	}
	out := make(map[string]any, len(a.Input))
	for k, v := range a.Input {
		if k == "command" || k == "arguments" {
			continue
		}
		out[k] = v
	}
	return out
}

// Expired reports whether this request's window has already closed.
func (a Approval) Expired() bool {
	return a.ExpiresAt != nil && !time.Now().Before(*a.ExpiresAt)
}

type approvalsEnvelope struct {
	Data struct {
		Approvals struct {
			Data  []Approval `json:"data"`
			Total int        `json:"total"`
		} `json:"approvals"`
	} `json:"data"`
}

// PendingApprovals lists the organization's undecided requests. OrgID
// addresses the queue, but it does not authorize it: the backend derives the
// organization from the caller's own credential and ignores the path value for
// scoping, so passing another org's id cannot read that org's queue.
func (c *Client) PendingApprovals(ctx context.Context, orgID string) ([]Approval, error) {
	if strings.TrimSpace(orgID) == "" {
		return nil, fmt.Errorf("organization id is required to read the approval queue")
	}
	var env approvalsEnvelope
	path := "/organization/" + url.PathEscape(orgID) + "/approvals?status=pending"
	if err := c.do(ctx, http.MethodGet, path, nil, &env); err != nil {
		return nil, err
	}
	return env.Data.Approvals.Data, nil
}

const (
	ApprovalApprove = "approve"
	ApprovalReject  = "reject"
)

// DecideApproval answers one request.
func (c *Client) DecideApproval(ctx context.Context, agentID, eventID, action string) error {
	if strings.TrimSpace(agentID) == "" || strings.TrimSpace(eventID) == "" {
		return fmt.Errorf("agent id and event id are both required to decide an approval")
	}
	if action != ApprovalApprove && action != ApprovalReject {
		return fmt.Errorf("approval action must be %q or %q, got %q", ApprovalApprove, ApprovalReject, action)
	}
	path := fmt.Sprintf("/agent/%s/approvals/%s/decide?action=%s",
		url.PathEscape(agentID), url.PathEscape(eventID), action)
	return c.do(ctx, http.MethodPut, path, nil, nil)
}
