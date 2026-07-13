package sidecar

import "github.com/openbox-ai/openbox-shift-left/client"

// BuildOPAInput assembles the rego `input` document a DecisionRequest would be
// evaluated against — shaped to match openbox-core's buildOPAInput
// (openbox-core internal/services/opa.go:458-477) so a future embedded-OPA
// evaluator (the one that loads real per-agent rego and queries
// data.org.<org>.policy_<id>, reading result.result.decision + reason) can be
// dropped in behind the Evaluator seam with ZERO change to the socket protocol
// or the server.
//
// It is exported and pure (no I/O) precisely so it is the documented contract
// point between "what the hook sends" (DecisionRequest) and "what a policy
// engine consumes" (core's rego input). The Phase-1 default bundleEvaluator does
// NOT need the full document — it matches on the request directly — but building
// it here keeps the fidelity to core's contract explicit and testable, and marks
// exactly where the real OPA evaluator plugs in.
//
// Fidelity note (cross-repo recon, 2026-07-13): core does NOT embed OPA; it
// POSTs this document to an external OPA server (OPA_URL, default
// localhost:8181), and the per-agent rego lives in core's Postgres as RegoCode —
// there is no bundle openbox-core distributes today. So a live "load core's
// bundle and evaluate" path is an EXTERNAL dependency (the OPA-bundle
// distribution), tracked as a follow-up, not built here. This function encodes
// the input half of that contract now so the seam is real, not hand-wavy.
func BuildOPAInput(req DecisionRequest) map[string]any {
	// run_id / workflow_id / agent_id mirror the dev-event → core mapping
	// (MAPPING.md §1): SessionID → run_id, WorkspaceID (or DID) → workflow_id,
	// DeveloperDID → agent_id. Absent optional fields are simply omitted, exactly
	// as core omits zero-valued input keys.
	workflowID := req.WorkspaceID
	if workflowID == "" {
		workflowID = req.DeveloperDID
	}

	in := map[string]any{
		"event_type": string(req.EventType),
		"source":     "developer-runtime", // dev-runtime analog of core's `source`
		"run_id":     req.SessionID,
	}
	if workflowID != "" {
		in["workflow_id"] = workflowID
	}
	if req.DeveloperDID != "" {
		in["agent_id"] = req.DeveloperDID
	}
	if req.Org != "" {
		in["org"] = req.Org
	}

	// One span for the tool call — core's rego already matches ToolCall onto the
	// activity/span input fields (recon note; opa.go addSpansToInput/buildSpanMap).
	// We surface the tool name/kind + the metadata attributes the local policy
	// matches on (command, file_path, file_operation, mcp_server, …).
	span := map[string]any{
		"name":          req.Tool.Name,
		"tool_kind":     string(req.Tool.Kind),
		"semantic_type": semanticTypeFor(req.Tool.Kind),
	}
	if req.Tool.MCPServer != "" {
		span["mcp_server"] = req.Tool.MCPServer
	}
	for k, v := range req.Attributes {
		// Attributes are metadata only (INV-2); they flow into the span map the way
		// core carries per-span attributes.
		span[k] = v
	}
	in["spans"] = []any{span}
	in["span_count"] = 1

	return in
}

// semanticTypeFor maps a provider-agnostic tool kind to the span semantic_type
// hint (aligns with the SL-1 contract / client.Span.SemanticType). Kept tiny and
// local — core recomputes semantic_type server-side; this is only the input hint.
func semanticTypeFor(kind client.ToolKind) string {
	switch kind {
	case client.ToolShell:
		return "command_execution"
	case client.ToolFile:
		return "file_operation"
	case client.ToolMCP:
		return "mcp_tool_call"
	default:
		return "tool_use"
	}
}
