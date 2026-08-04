package decision

import (
	"github.com/openbox-ai/openbox-shift-left/client"
)

// ProtocolVersion tags the DecisionRequest/DecisionResponse contract
// version. Retained as defense-in-depth (the decide path rejects a version
// it doesn't understand rather than mis-decode); with the in-process
// decider (ADR-0006) the request and the evaluator are always the same
// binary, so this never negotiates across releases.
const ProtocolVersion = 1

// DecisionRequest is what the enforce-mode PreToolUse hook builds and hands
// to the in-process decider. It carries only what a local decision needs —
// the same axes core's rego input is built from (see BuildOPAInput): which
// developer/session, which tool, what action, and the metadata attributes.
//
// INV-2: Content is optional and only populated when the org's content
// posture is on (for local redaction). It never leaves the machine and is
// never logged.
type DecisionRequest struct {
	// Protocol is the wire version the caller speaks; the server rejects a
	// version it doesn't understand rather than mis-decode.
	Protocol int `json:"protocol"`

	// Identity — maps to core's run_id / workflow_id / agent_id
	// (MAPPING.md §1, BuildOPAInput). SessionID is required (it's the
	// decision's subject); an empty one is a malformed request.
	SessionID    string `json:"openbox_session_id"`
	DeveloperDID string `json:"developer_did"`
	WorkspaceID  string `json:"workspace_id,omitempty"`
	Org          string `json:"org,omitempty"`

	// EventType is the dev-runtime lifecycle type this decision is for —
	// almost always ToolCall for enforcement (the pre-execution gate), but
	// the field is general so PromptSubmitted etc. can be gated later.
	EventType client.EventType `json:"event_type"`

	// Tool is the developer tool/action being gated (name + provider-agnostic
	// kind: shell|file|mcp). The dev-runtime analog of core's activity/span.
	Tool client.Tool `json:"tool"`

	// Attributes are the metadata axes a local policy matches on — e.g.
	// {"command":"rm -rf /", "file_path":"/etc/passwd",
	// "file_operation":"write"}. Metadata only (INV-2): never raw
	// prompt/output/file bodies.
	Attributes map[string]any `json:"attributes,omitempty"`

	// Content is gated (INV-2): present only when content-capture is on, for
	// local redaction. Stripped/absent by default. Not used by the verdict
	// decision.
	Content *client.Content `json:"content,omitempty"`
}

// DecisionResponse is the decider's answer: the resolved governance
// Evaluation (the same client.Evaluation the Advisory tier records and the
// apply path acts on — no parallel verdict type), plus how it was reached.
type DecisionResponse struct {
	Protocol int `json:"protocol"`

	// Evaluation is the local verdict + reason + policy id + constraints.
	// When no policy is loaded it is zero-valued (VerdictUnknown) with
	// sourceFailOpenNoBundle, which the enforce hook treats as allow under
	// fail-open (or denies under the opt-in fail-closed policy).
	Evaluation client.Evaluation `json:"evaluation"`

	// Source records how the decision was made, for the async telemetry
	// mirror and for conformance evidence. One of the decisionSource*
	// constants.
	Source string `json:"source"`

	// Stale is true when the decision was served from a bundle older than
	// the configured freshness window (the out-of-band sync fell behind).
	// The decision is still returned (fail-open never denies on staleness);
	// the flag lets telemetry observe drift.
	// Stale reports that the policy this decision used has not been refreshed
	// within the freshness window, measured from the bundle FILE's mtime
	// (OD-RF-4). It was previously measured from evaluator-load time, which made
	// it permanently false: the decider is built per tool call in a short-lived
	// hook process, so the window could never elapse. The session-start pin
	// compare remains the check against the control plane; this is the local
	// "how old is the policy on disk" signal.
	Stale bool `json:"stale,omitempty"`

	// Error is a non-secret diagnostic when the server couldn't decide (e.g.
	// no bundle loaded yet at cold start). Advisory only; the Evaluation is
	// the fail-open zero value. Never carries a secret or content
	// (INV-1/INV-2).
	Error string `json:"error,omitempty"`

	// RedactedContent is the local redaction result: the tool's content
	// field(s) (a file body) with detected secrets replaced by an
	// env-var-ref placeholder. The enforce hook does not emit this object
	// as-is; instead it reconstructs the tool_input, replacing only the
	// recognized content field with RedactedContent.FileText and leaving
	// every structural locator (file_path, …) byte-identical.
	//
	// INV-2: this is the one content-bearing field on the decision
	// contract. It is carried here — not on Evaluation — deliberately:
	// Evaluation flows into the advisory sink, the enforcement audit, and
	// core egress, none of which must ever see content; this field stays
	// confined to the local in-process decider ↔ hook ↔ Claude Code stdout
	// channel (same machine, never egressed, never logged).
	RedactedContent *client.Content `json:"redacted_content,omitempty"`

	// RedactionCategories are the secret-category names that fired (e.g.
	// ["aws_key","entropy"]) — the content-free (INV-2) signal the enforce
	// hook records in its durable audit so an operator can see
	// redact-and-continue happened without ever seeing the secret. Never
	// the secret text.
	RedactionCategories []string `json:"redaction_categories,omitempty"`
}

// decisionSource values — how a DecisionResponse.Evaluation was reached.
const (
	// sourceLocalBundle: decided locally from the synced policy bundle (the
	// normal enforce path).
	sourceLocalBundle = "local-bundle"
	// sourceFailOpenNoBundle: no bundle loaded yet (cold start), or the
	// request was unusable (missing session / bad protocol) → fail-open
	// allow.
	sourceFailOpenNoBundle = "fail-open:no-bundle"
)
