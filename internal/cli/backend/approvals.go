package backend

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// The approval queue (E9 §2.3).
//
// These are the control-plane routes an approver acts through, and they already
// exist — the dashboard's Approvals page is built on them. `openbox approve`
// adds no new server surface; it is a second CLIENT of the same queue, for
// people who would rather stay in a terminal.
//
// It is deliberately a client and nothing more: no listener, no socket, no
// daemon. And it runs on the APPROVER's machine, which is a different person's
// machine than the one that made the request — the property a local approver
// app could never have (E9 §3.7).

// Approval is one pending request in the org's queue.
//
// The fields are taken from the LIVE response, not from the backend's
// ApprovalItemResponseDto — the two disagree, and the wire is richer: it carries
// `activity_type` and the structural `input`, which the DTO does not declare. E9
// §3.5 flagged the shape as something to verify against a real envelope before
// depending on it; this is that verification.
//
// What the queue gives an approver is the agent, the tool, its structural
// identifiers, and the policy's own reason. Whether it can also show the command
// depends on the org's posture, not on this DTO: a gated call has carried
// Content.ToolInput since that decision, so with content capture ON the command
// IS on the envelope this reads. With capture off it is absent and the queue
// cannot show what was never sent.
//
// This comment used to say the runtime "never egresses tool commands or file
// bodies on an observe event" — which was SL3-SEC-3, and was in any case the
// wrong invariant to cite here: the queue is fed by GATED events, not observe
// ones.
type Approval struct {
	ID string `json:"id"`
	// AgentID owns the request; the decide route is per-agent.
	AgentID string `json:"agent_id"`
	// Agent is the nested agent object the list embeds. AgentName is the flat
	// field the DTO declares; the live response nests it instead, so both are
	// parsed and Name() prefers whichever arrived.
	Agent     *ApprovalAgent `json:"agent"`
	AgentName string         `json:"agent_name"`
	// ActivityType is the tool (e.g. "Bash", "mcp__github__create_issue").
	ActivityType string `json:"activity_type"`
	// Input is the activity_input the gated event carried: tool kind and
	// identifiers always, and — with content capture on — the command, the MCP
	// arguments or the file body under `command`/`arguments`/`content`.
	//
	// This said "never the command or a file body (INV-2)", contradicting the type
	// comment four lines above it, which records the opposite: a gated call has
	// carried Content.ToolInput since. Request() reads Input["command"] precisely
	// because it is the field an approval is decided on, so the old wording
	// described the one thing this struct exists to show.
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

// Request is WHAT the call is asking to do — the command for a shell tool, the
// arguments for an MCP one — and it is the field an approval is actually
// decided on. Empty when the runtime sent none, which is the content-capture-off
// posture: the approver then has the tool's name and nothing else, and the only
// honest answer is to say so rather than present a decidable-looking request.
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

// Context is the structural remainder — the identifiers that locate the call
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

// Expired reports whether this request's window has already closed. The queue
// can return one briefly before the sweep reclassifies it, and deciding it
// would fail server-side, so a caller shows it rather than offering it.
func (a Approval) Expired() bool {
	return a.ExpiresAt != nil && !time.Now().Before(*a.ExpiresAt)
}

// approvalsEnvelope is the live response shape: the global TransformInterceptor's
// `data` wrapper, then `approvals`, then that object's OWN paginated `data`
// array. The double nesting is easy to get wrong from the DTO alone — the DTO
// declares `approvals` as a flat array, and the wire paginates it.
type approvalsEnvelope struct {
	Data struct {
		Approvals struct {
			Data  []Approval `json:"data"`
			Total int        `json:"total"`
		} `json:"approvals"`
	} `json:"data"`
}

// PendingApprovals lists the organization's undecided requests.
//
// orgID addresses the queue, but it does not authorize it: the backend derives
// the organization from the caller's own credential and ignores the path value
// for scoping, so passing another org's id cannot read that org's queue.
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

// Approval decisions the backend accepts (DecideApprovalDto.action).
const (
	ApprovalApprove = "approve"
	ApprovalReject  = "reject"
)

// DecideApproval answers one request. The backend applies the decision under a
// conditional update guarded on the request still being undecided and unexpired,
// so two approvers racing — or an autonomous approver racing a human — resolve
// to one answer rather than overwriting each other.
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
