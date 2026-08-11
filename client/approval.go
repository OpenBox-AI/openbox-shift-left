package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// approvalPath is the openbox-core approval-status route, and the signed
// canonical-string PATH component.
//
// It is agent-authenticated exactly as /evaluate is — same Bearer obx_ key,
// same AIP signature — and is keyed on the three ids a hook payload already
// carries. That is what makes a bounded hold possible without a local socket or
// a second install (E9 §2.2): the hook polls the same control plane it just
// escalated to.
const approvalPath = "/api/v1/governance/approval"

// ErrApprovalNotFound reports that core holds no governance event for this key
// — the request was never filed, or has not landed yet. It is distinct from a
// transport failure because the two mean opposite things to a caller: not-found
// is an answer worth retrying at the poll cadence, a transport error is an
// outage the failure policy governs.
var ErrApprovalNotFound = errors.New("client: no approval record for this key")

// ApprovalKey addresses one approval record. All three ids are required by
// core, and all three are already derived by the payload builder — see
// ApprovalKeyFor, which is what keeps a poll pointed at the row the escalation
// created.
type ApprovalKey struct {
	WorkflowID string `json:"workflow_id"`
	RunID      string `json:"run_id"`
	ActivityID string `json:"activity_id"`
}

// Valid reports whether the key can address a record at all. Core rejects a
// partial key with a 400, so a caller checks first rather than spending a
// round-trip on it.
func (k ApprovalKey) Valid() bool {
	return k.WorkflowID != "" && k.RunID != "" && k.ActivityID != ""
}

// ApprovalKeyFor derives the poll key for a dev event from the SAME
// derivations buildPayload writes onto the wire — workflow_id, run_id and
// activity_id. Sharing the derivation rather than re-deriving it is the point:
// a poll built from independently-computed ids would silently address a
// different row (or none), and the hold would report "never decided" for an
// approval that was in fact granted.
//
// client/approval_key_pin_test.go pins the output. These three ids are the only
// identity in the package a refactor must not move.
func ApprovalKeyFor(ev DevEvent) ApprovalKey {
	return ApprovalKey{
		WorkflowID: workflowIDFor(ev),
		RunID:      ev.SessionID,
		ActivityID: activityIDFor(ev),
	}
}

// ApprovalStatus is one poll answer: where the approval stands right now.
type ApprovalStatus struct {
	// EventID is core's governance event id — the reference an approver
	// resolves the request by (see Evaluation.ApprovalRef).
	EventID string
	// Verdict is the record's current verdict. REQUIRE_APPROVAL means still
	// pending; anything else is a decision.
	Verdict Verdict
	// Reason is the policy-authored reason, if core carried one.
	Reason string
	// ExpiresAt is when the approval window closes. Zero when core sent none,
	// which also means this record is not in an approval flow at all — the
	// same condition core itself uses to recognize an approval grant.
	ExpiresAt time.Time
}

// Pending reports whether the request is still awaiting a decision.
func (s ApprovalStatus) Pending() bool { return s.Verdict == VerdictRequireApproval }

// Decided reports that this record carries a real answer to an approval
// request, and is what a caller must gate on before acting on a poll.
//
// The approval window is part of the test on purpose. Core renders a record
// with no verdict as action="allow" (VerdictToAction's default branch), so
// "allow" ALONE cannot distinguish "an approver said yes" from "this row was
// never governed" — and a hold that could not tell those apart would release a
// pending call. The window is exactly the condition core itself uses to
// recognize an approval grant, and it survives the decision, so requiring it
// rejects the ambiguous reading without rejecting any real answer.
func (s ApprovalStatus) Decided() bool {
	return !s.ExpiresAt.IsZero() && s.Verdict != VerdictRequireApproval && s.Verdict != VerdictUnknown
}

// PollApproval reads the current state of one approval record.
//
// It makes exactly ONE attempt, deliberately: callers poll on a cadence, and an
// internal retry loop would spend a hold's whole budget inside a single tick.
// The caller's own interval is the retry.
func (c *Client) PollApproval(ctx context.Context, k ApprovalKey) (ApprovalStatus, error) {
	if !k.Valid() {
		return ApprovalStatus{}, errors.New("client: ApprovalKey needs workflow_id, run_id and activity_id")
	}
	body, err := json.Marshal(k)
	if err != nil {
		return ApprovalStatus{}, fmt.Errorf("%w: %v", ErrUnbuildable, err)
	}
	// No Idempotency-Key: this is a read, and core mints nothing from it.
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

// approvalStatusWire is core's ApprovalStatusResponse. Note it carries `action`
// and no `verdict` field.
type approvalStatusWire struct {
	ID        string     `json:"id"`
	Action    string     `json:"action"`
	Reason    *string    `json:"reason"`
	ExpiresAt *time.Time `json:"approval_expiration_time"`
}

func parseApprovalStatus(body []byte) (ApprovalStatus, error) {
	var r approvalStatusWire
	if err := json.Unmarshal(body, &r); err != nil {
		// Unlike /evaluate there is no useful degraded reading here: a status we
		// cannot parse is not "allow", it is "unknown", and a hold that guessed
		// would either block a granted call or release a pending one.
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

// resolveApprovalAction maps the poll response's `action` to a canonical
// verdict. Core renders it with VerdictToAction, which emits the same
// lowercase vocabulary the /evaluate `verdict` field uses — not the hyphenated
// legacy set — so it resolves as a verdict string first, with the legacy
// mapping still available for an older core.
func resolveApprovalAction(action string) Verdict {
	return resolveVerdict(action, action)
}
