package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

const approvalPath = "/api/v1/governance/approval"

// ErrApprovalNotFound reports that core holds no governance event for this
// key; the request was never filed, or has not landed yet.
var ErrApprovalNotFound = errors.New("client: no approval record for this key")

// ApprovalKey addresses one approval record.
type ApprovalKey struct {
	WorkflowID string `json:"workflow_id"`
	RunID      string `json:"run_id"`
	ActivityID string `json:"activity_id"`
}

// Valid reports whether the key can address a record at all.
func (k ApprovalKey) Valid() bool {
	return k.WorkflowID != "" && k.RunID != "" && k.ActivityID != ""
}

// ApprovalKeyFor derives the poll key for a dev event from the same
// derivations buildPayload writes onto the wire; workflow_id, run_id and
// activity_id.
func ApprovalKeyFor(ev DevEvent) ApprovalKey {
	return ApprovalKey{
		WorkflowID: workflowIDFor(ev),
		RunID:      ev.SessionID,
		ActivityID: activityIDFor(ev),
	}
}

// ApprovalStatus is one poll answer: where the approval stands right now.
type ApprovalStatus struct {
	// EventID is core's governance event id; the reference an approver resolves
	// the request by (see Evaluation.ApprovalRef).
	EventID string
	// Verdict is the record's current verdict.
	Verdict Verdict
	// Reason is the policy-authored reason, if core carried one.
	Reason string
	// ExpiresAt is when the approval window closes.
	ExpiresAt time.Time
}

// Pending reports whether the request is still awaiting a decision.
func (s ApprovalStatus) Pending() bool { return s.Verdict == VerdictRequireApproval }

// Decided reports that this record carries a real answer to an approval
// request, and is what a caller must gate on before acting on a poll. The
// approval window is part of the test on purpose.
func (s ApprovalStatus) Decided() bool {
	return !s.ExpiresAt.IsZero() && s.Verdict != VerdictRequireApproval && s.Verdict != VerdictUnknown
}

// PollApproval reads the current state of one approval record. It makes
// exactly ONE attempt, deliberately: callers poll on a cadence, and an
// internal retry loop would spend a hold's whole budget inside a single tick.
func (c *Client) PollApproval(ctx context.Context, k ApprovalKey) (ApprovalStatus, error) {
	if !k.Valid() {
		return ApprovalStatus{}, errors.New("client: ApprovalKey needs workflow_id, run_id and activity_id")
	}
	body, err := json.Marshal(k)
	if err != nil {
		return ApprovalStatus{}, fmt.Errorf("%w: %v", ErrUnbuildable, err)
	}
	respBody, _, err := c.attempt(ctx, approvalPath, body, "")
	if err != nil {
		var he *httpError
		if errors.As(err, &he) && he.status == http.StatusNotFound {
			return ApprovalStatus{}, ErrApprovalNotFound
		}
		return ApprovalStatus{}, fmt.Errorf("%w: %s", ErrDelivery, describeDrop(err))
	}
	return parseApprovalStatus(respBody)
}

type approvalStatusWire struct {
	ID        string     `json:"id"`
	Action    string     `json:"action"`
	Reason    *string    `json:"reason"`
	ExpiresAt *time.Time `json:"approval_expiration_time"`
}

func parseApprovalStatus(body []byte) (ApprovalStatus, error) {
	var r approvalStatusWire
	if err := json.Unmarshal(body, &r); err != nil {
		return ApprovalStatus{}, fmt.Errorf("client: unparseable approval status: %w", err)
	}
	s := ApprovalStatus{
		EventID: r.ID,
		Verdict: resolveApprovalAction(r.Action),
	}
	if r.Reason != nil {
		s.Reason = *r.Reason
	}
	if r.ExpiresAt != nil {
		s.ExpiresAt = *r.ExpiresAt
	}
	return s, nil
}

func resolveApprovalAction(action string) Verdict {
	return resolveVerdict(action, action)
}
