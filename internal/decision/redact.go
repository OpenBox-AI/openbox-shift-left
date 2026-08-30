package decision

import (
	"context"

	"github.com/openbox-ai/openbox-shift-left/internal/client"
)

// Decider is the seam the enforce gate depends on, so it holds a contract
// rather than a concrete type and a test can substitute a fake. It never
// returns an error: every fault yields a decision that proceeds, so a fault in
// this layer can never block the developer (INV-3b).
type Decider interface {
	Decide(ctx context.Context, req DecisionRequest) Decision
}

// Decision is the local step's result.
type Decision struct {
	Evaluation client.Evaluation
	// FailOpen reports that no verdict was obtained. The local step always sets
	// it, because the local step never decides.
	FailOpen bool
	// SessionHalt marks a HALT that terminates the whole session, not just this
	// call. A synthesized HALT; fail-closed outage, undecided approval, approver
	// reject; must never carry it: those deny one call.
	SessionHalt bool
	// Source names where a verdict came from, for the audit.
	Source string
	// RedactedContent carries the local secret-redaction of the tool content.
	RedactedContent *client.Content
	// RedactionCategories echoes the content-free category names that fired
	// (aws_key, entropy, …), for the durable enforcement audit. Never the secret
	// text.
	RedactionCategories []string
}

// SourceLocalRedaction marks a decision produced by the local step.
const SourceLocalRedaction = "local:redaction"

// Redactor is the local step: scan the call's content for secrets and return
// the redacted body.
type Redactor struct{ scanner *secretDetector }

// NewRedactor builds the local redaction step with the default detector set.
func NewRedactor() *Redactor { return &Redactor{scanner: newSecretDetector()} }

var _ Decider = (*Redactor)(nil)

// Decide runs secret detection over the request's content, if any, and reports
// a decision that always proceeds.
func (r *Redactor) Decide(_ context.Context, req DecisionRequest) Decision {
	dec := Decision{FailOpen: true, Source: SourceLocalRedaction}
	if r == nil || r.scanner == nil || req.Content == nil || req.Content.FileText == "" {
		return dec
	}
	red, cats, changed := r.scanner.Redact(req.Content.FileText)
	if !changed {
		return dec
	}
	dec.RedactedContent = &client.Content{FileText: red}
	dec.RedactionCategories = cats
	return dec
}

// RedactText scans an arbitrary content body for secrets and returns the
// redacted text, the content-free category names that fired, and whether
// anything changed.
func (r *Redactor) RedactText(s string) (redacted string, categories []string, changed bool) {
	if r == nil || r.scanner == nil || s == "" {
		return s, nil, false
	}
	return r.scanner.Redact(s)
}

// DecisionRequest is what the adapters build for the local step.
type DecisionRequest struct {
	SessionID    string
	DeveloperDID string
	WorkspaceID  string
	Org          string
	EventType    client.EventType
	Tool         client.Tool
	Attributes   map[string]any
	// Content is gated (INV-2): present only when secret detection or content
	// capture is on.
	Content *client.Content
}
