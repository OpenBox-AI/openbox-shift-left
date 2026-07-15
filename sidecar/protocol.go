package sidecar

import (
	"github.com/openbox-ai/openbox-shift-left/client"
)

// ProtocolVersion is the Unix-socket wire contract version. Bumped on any
// breaking change to DecisionRequest/DecisionResponse. Both the daemon (server)
// and the enforce hook (Client) are shipped in the SAME binary (WIRE-2), so this
// is defense-in-depth against a stale hook talking to a newer daemon, not a
// cross-release negotiation.
const ProtocolVersion = 1

// DecisionRequest is what the enforce-mode PreToolUse hook (E6-S1) sends over the
// Unix socket, one request per connection. It carries only what a LOCAL decision
// needs — the same axes core's rego input is built from (see BuildOPAInput):
// which developer/session, which tool, what action, and the metadata attributes.
//
// INV-2: Content is OPTIONAL and only populated when the org's content posture is
// on (for E6-S4 local redaction). It never leaves the machine and is never logged.
type DecisionRequest struct {
	// Protocol is the wire version the caller speaks; the server rejects a version
	// it does not understand rather than mis-decode.
	Protocol int `json:"protocol"`

	// Identity — maps to core's run_id / workflow_id / agent_id (MAPPING.md §1,
	// BuildOPAInput). SessionID is required (it is the decision's subject); an
	// empty one is a malformed request.
	SessionID    string `json:"openbox_session_id"`
	DeveloperDID string `json:"developer_did"`
	WorkspaceID  string `json:"workspace_id,omitempty"`
	Org          string `json:"org,omitempty"`

	// EventType is the dev-runtime lifecycle type this decision is for — almost
	// always ToolCall for enforcement (the pre-execution gate), but the field is
	// general so PromptSubmitted etc. can be gated later. Uses the SL-1 contract
	// strings (client.EventType).
	EventType client.EventType `json:"event_type"`

	// Tool is the developer tool/action being gated (name + provider-agnostic
	// kind: shell|file|mcp). The dev-runtime analog of core's activity/span.
	Tool client.Tool `json:"tool"`

	// Attributes are the metadata axes a local policy matches on — e.g.
	// {"command":"rm -rf /", "file_path":"/etc/passwd", "file_operation":"write"}.
	// Metadata only (INV-2): never raw prompt/output/file bodies.
	Attributes map[string]any `json:"attributes,omitempty"`

	// Content is GATED (INV-2): present only when content-capture is on, for local
	// redaction (E6-S4). Stripped/absent by default. Not used by the Phase-1
	// verdict decision.
	Content *client.Content `json:"content,omitempty"`
}

// DecisionResponse is the daemon's answer: the resolved governance Evaluation
// (the SAME client.Evaluation the Advisory tier records and E6-S2's apply acts
// on — no parallel verdict type), plus how it was reached.
type DecisionResponse struct {
	Protocol int `json:"protocol"`

	// Evaluation is the local verdict + reason + policy id + constraints. On any
	// server-side fault it is zero-valued (VerdictUnknown), which the enforce hook
	// treats as allow (fail-open) — but the Client also fails open BEFORE this,
	// without a response, when the socket is absent/slow.
	Evaluation client.Evaluation `json:"evaluation"`

	// Source records how the decision was made, for the async telemetry mirror and
	// for conformance evidence (E6-S7). One of the decisionSource* constants.
	Source string `json:"source"`

	// Stale is true when the decision was served from a bundle older than the
	// configured freshness window (the out-of-band sync fell behind). The decision
	// is still returned (fail-open never denies on staleness); the flag lets E6-S3
	// / telemetry observe drift.
	Stale bool `json:"stale,omitempty"`

	// Error is a non-secret diagnostic when the server could not decide (e.g. no
	// bundle loaded yet at cold start). Advisory only; the Evaluation is the
	// fail-open zero value. Never carries a secret or content (INV-1/INV-2).
	Error string `json:"error,omitempty"`

	// RedactedContent is the LOCAL redaction result: the tool's content field(s)
	// (a file body) with detected secrets replaced by an env-var-ref placeholder
	// (STORY-E6-S9 Tier-1 secret detection; a future guardrail-redaction evaluator
	// uses the same carrier). The enforce hook does NOT emit this object as-is;
	// instead it RECONSTRUCTS the tool_input, replacing ONLY the recognized content
	// field with RedactedContent.FileText and leaving every structural locator
	// (file_path, …) byte-identical (E6-S9, closing the E6-S4/S7 "content-only
	// fields, never structural" carry-forward). It replaces the E6-S4
	// `redacted_input` full-object carrier for exactly that reason.
	//
	// INV-2: this is the ONE content-bearing field on the sidecar protocol. It is
	// carried here — NOT on Evaluation — deliberately: Evaluation flows into the
	// advisory sink, the enforcement audit, and core egress, none of which must
	// ever see content; this field stays confined to the LOCAL Unix socket ↔ hook
	// ↔ Claude Code stdout channel (same machine, never egressed, never logged).
	RedactedContent *client.Content `json:"redacted_content,omitempty"`

	// RedactionCategories are the secret-category names that fired (e.g.
	// ["aws_key","entropy"]) — the CONTENT-FREE (INV-2) signal the enforce hook
	// records in its durable audit so an operator can see redact-and-continue
	// happened without ever seeing the secret. Never the secret text.
	RedactionCategories []string `json:"redaction_categories,omitempty"`
}

// decisionSource values — how a DecisionResponse.Evaluation was reached.
const (
	// sourceLocalBundle: decided locally from the synced policy bundle (the normal
	// enforce path).
	sourceLocalBundle = "local-bundle"
	// sourceFailOpenNoBundle: no bundle loaded yet (cold start) → fail-open allow.
	sourceFailOpenNoBundle = "fail-open:no-bundle"
	// sourceFailOpenClient: set by the Client (not the server) when the socket is
	// absent, the dial/decision timed out, or the response was unusable → allow.
	sourceFailOpenClient = "fail-open:client"
)
