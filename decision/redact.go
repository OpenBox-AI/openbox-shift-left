package decision

import (
	"context"

	"github.com/openbox-ai/openbox-shift-left/client"
)

// This file is what is left of the decision module after ADR-0017. Policy is
// evaluated by OpenBox on every gated tool call, so the local evaluator, the
// bundle it read, and the signature machinery around it are all gone.
//
// What survives is secret redaction, and it survives for a reason that is not
// inertia: it is content PROTECTION, not policy evaluation. It runs before the
// call's content leaves the machine, and it sees the whole body where the server
// sees at most the first 64KB (client.capBody). Deleting it would mean sending
// secrets to the control plane and asking it to judge them.

// Decider is the seam the enforce gate depends on, so it holds a contract
// rather than a concrete type and a test can substitute a fake. It never
// returns an error: every fault yields a decision that proceeds, so a fault in
// this layer can never block the developer (INV-3b).
type Decider interface {
	Decide(ctx context.Context, req DecisionRequest) Decision
}

// Decision is the local step's result.
//
// It no longer carries a verdict. Evaluation stays on the struct because the
// gate's apply cascade is written against it and the SERVER's verdict is folded
// into the same shape a moment later — but nothing local ever populates it now,
// and FailOpen is always true here: the local step governs nothing, it only
// redacts.
type Decision struct {
	Evaluation client.Evaluation
	// FailOpen reports that no verdict was obtained. The failure policy engages
	// on it: fail-open (default) proceeds, fail-closed denies. The local step
	// always sets it, because the local step never decides.
	FailOpen bool
	// Source names where a verdict came from, for the audit.
	Source string
	// RedactedContent carries the local secret-redaction of the tool content.
	// The enforce hook reconstructs tool_input from it (content field only) and
	// applies it via the provider's input rewrite; the evaluation event attaches
	// the same bytes. nil when nothing was redacted.
	//
	// INV-2: content-bearing. It reaches the control plane only through the
	// gated evaluation event, and only after this redaction has run.
	RedactedContent *client.Content
	// RedactionCategories echoes the content-free category names that fired
	// (aws_key, entropy, …), for the durable enforcement audit. Never the
	// secret text.
	RedactionCategories []string
}

// SourceLocalRedaction marks a decision produced by the local step. It is not a
// verdict source — it says only that the redactor ran.
const SourceLocalRedaction = "local:redaction"

// Redactor is the local step: scan the call's content for secrets and return
// the redacted body. It decides nothing.
type Redactor struct{ scanner *secretDetector }

// NewRedactor builds the local redaction step with the default detector set.
func NewRedactor() *Redactor { return &Redactor{scanner: newSecretDetector()} }

var _ Decider = (*Redactor)(nil)

// Decide runs secret detection over the request's content, if any, and reports
// a decision that always proceeds.
//
// Content is present only when the caller passed it, which it does only under
// secret detection or content capture. With neither, nothing is scanned and
// nothing is redacted — byte-identical to the observe baseline.
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
//
// Decide is shaped around a tool call — it reads Content.FileText and returns a
// Decision — which fits nothing but the enforce gate. ADR-0018 added a second
// content class with no tool call anywhere near it: the assistant's turn text,
// redacted before it is attached to a turn event. This is the same scanner
// reached directly, deliberately NOT a second detector: two redaction
// implementations would drift, and the one that drifted would be discovered by
// a secret arriving at the control plane.
//
// A nil Redactor returns the text unchanged. That is the honest degradation for
// `secret_detection:false` — the caller wires nil and the text egresses
// unredacted, which ADR-0018 states rather than hides — and it means no caller
// needs a nil check to stay correct.
func (r *Redactor) RedactText(s string) (redacted string, categories []string, changed bool) {
	if r == nil || r.scanner == nil || s == "" {
		return s, nil, false
	}
	return r.scanner.Redact(s)
}

// DecisionRequest is what the adapters build for the local step.
//
// Only Content is read now. The identity and metadata axes remain because they
// are how each adapter describes a call, and they still reach the control plane
// — as the evaluation event's own fields, which is where the policy that
// consumes them now lives. They are kept here rather than pruned so the
// adapters' mapping code keeps one shape for describing a tool call.
//
// Metadata only (INV-2) except Content, which is gated.
type DecisionRequest struct {
	SessionID    string
	DeveloperDID string
	WorkspaceID  string
	Org          string
	EventType    client.EventType
	Tool         client.Tool
	Attributes   map[string]any
	// Content is gated (INV-2): present only when secret detection or content
	// capture is on. It is the only field this step reads.
	Content *client.Content
}
