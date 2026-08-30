// Package approver is the autonomous half of the approval tier: a bounded,
// policy-first decider that works the same queue a human works (`openbox
// approve`), under the approver's own credential.
//
//	decided automatically at all. Anything it does not cover is left for a
//	human; never approved because nothing objected.
//	consulted only for the classes the envelope marks consultable, and it may
//	only NARROW: its deny is always honoured, its approve is honoured only
package approver

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Outcome is what the envelope says may happen to a request.
type Outcome string

const (
	// AutoApprove / AutoDeny; the envelope decides, no model involved.
	AutoApprove Outcome = "auto_approve"
	AutoDeny    Outcome = "auto_deny"
	// Consult; inside the envelope's judgeable set: ask the host, and honour only
	// an answer that narrows.
	Consult Outcome = "consult"
	// Escalate; outside the envelope entirely.
	Escalate Outcome = "escalate"
)

// Envelope is the org's boundary on autonomous decisions. It is deliberately
// NOT the enforcement bundle (decision.Bundle).
type Envelope struct {
	Version string `json:"version,omitempty"`

	// AutoDeny matched in this order, first match wins: deny beats approve beats
	// consult, so an overlapping envelope cannot accidentally widen.
	AutoDeny    []Rule `json:"auto_deny,omitempty"`
	AutoApprove []Rule `json:"auto_approve,omitempty"`
	Consult     []Rule `json:"consult,omitempty"`
}

// Rule matches a request structurally. Every set field must match; unset
// fields are wildcards.
type Rule struct {
	// Tool is the tool name, exact or with a trailing "*" (e.g.
	Tool string `json:"tool,omitempty"`
	// RequestContains matches the request text; the command for a shell call, the
	// arguments for an MCP one.
	RequestContains string `json:"request_contains,omitempty"`
	// Note explains the rule in the audit line.
	Note string `json:"note,omitempty"`
}

func (r Rule) matches(tool, request string) bool {
	if r.Tool != "" {
		if strings.HasSuffix(r.Tool, "*") {
			if !strings.HasPrefix(tool, strings.TrimSuffix(r.Tool, "*")) {
				return false
			}
		} else if tool != r.Tool {
			return false
		}
	}
	if r.RequestContains != "" && !strings.Contains(request, r.RequestContains) {
		return false
	}
	return true
}

// Classify reports what may happen to a request, and which rule said so.
func (e Envelope) Classify(tool, request string) (Outcome, Rule) {
	for _, set := range []struct {
		out   Outcome
		rules []Rule
	}{
		{AutoDeny, e.AutoDeny},
		{AutoApprove, e.AutoApprove},
		{Consult, e.Consult},
	} {
		for _, r := range set.rules {
			if r.matches(tool, request) {
				return set.out, r
			}
		}
	}
	return Escalate, Rule{Note: "not covered by the envelope"}
}

// LoadEnvelope reads an envelope file.
func LoadEnvelope(path string) (Envelope, error) {
	if strings.TrimSpace(path) == "" {
		return Envelope{}, fmt.Errorf("no envelope configured — an autonomous approver without one decides nothing; " +
			"set `envelope` in approver.json or pass --envelope")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return Envelope{}, fmt.Errorf("read envelope %s: %w", filepath.Clean(path), err)
	}
	var e Envelope
	if err := json.Unmarshal(raw, &e); err != nil {
		return Envelope{}, fmt.Errorf("parse envelope %s: %w", filepath.Clean(path), err)
	}
	if len(e.AutoApprove)+len(e.AutoDeny)+len(e.Consult) == 0 {
		return Envelope{}, fmt.Errorf("envelope %s has no rules — it would escalate everything", filepath.Clean(path))
	}
	return e, nil
}
