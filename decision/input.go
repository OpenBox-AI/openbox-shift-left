package decision

import "github.com/openbox-ai/openbox-shift-left/client"

// BuildOPAInput assembles the `input` document a DecisionRequest is
// evaluated against — shaped to match openbox-core's buildOPAInput +
// buildSpanMap exactly, so a builder policy authored to run against core
// (whose field paths resolve directly against this tree — e.g.
// `spans[_].file_path`, `spans[_].semantic_type`,
// `spans[_].attributes.command`) fires identically here (ADR-0005). This is
// the load-bearing correctness crux: a field-name mismatch silently
// no-fires a rule.
//
// It is consumed by the native builderEvaluator (builder.go); the legacy
// bundleEvaluator matches the request directly and does not use it.
//
// Fidelity to core:
//   - Top-level keys align to buildOPAInput: event_type, source, run_id,
//     workflow_id, agent_id, span_count, spans. There is no top-level `org`
//     (core carries org only in the OPA query path, never in the input
//     document).
//   - Span keys align to buildSpanMap: span_id, trace_id, name,
//     semantic_type, start_time, end_time, attributes are always present
//     (matching core, which sets them unconditionally); file_path /
//     file_operation / function are promoted to top-level span keys only
//     when set (core promotes span.FilePath, span.FileOperation,
//     span.FuncName). Every other metadata axis — the shell `command`,
//     permission_mode, mcp_* — lives under the per-span `attributes` map,
//     since core has no top-level span key for them. There is deliberately
//     no top-level `tool_kind` span key (core never emits one).
func BuildOPAInput(req DecisionRequest) map[string]any {
	// run_id / workflow_id / agent_id mirror the dev-event → core mapping
	// (MAPPING.md §1): SessionID → run_id, WorkspaceID (or DID) → workflow_id,
	// DeveloperDID → agent_id. Absent optional fields are omitted, exactly as core
	// omits zero-valued input keys.
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

	in["spans"] = []any{buildSpan(req)}
	in["span_count"] = 1
	return in
}

// promotedSpanKeys are the metadata-attribute keys core's buildSpanMap surfaces
// as TOP-LEVEL span keys (from typed SpanData fields), rather than leaving them
// under `attributes`. A DecisionRequest attribute whose key is in this set is
// promoted to a top-level span key for parity; every other attribute stays under
// `attributes` (where core keeps arbitrary span.Attributes). Sourced from
// buildSpanMap's recognized single-value fields (opa.go:619-719).
var promotedSpanKeys = map[string]struct{}{
	"file_path": {}, "file_mode": {}, "file_operation": {},
	"bytes_read": {}, "bytes_written": {}, "lines_count": {},
	"function": {}, "module": {}, "args": {}, "result": {},
	"http_method": {}, "http_url": {}, "http_status_code": {},
	"db_system": {}, "db_name": {}, "db_operation": {}, "db_statement": {},
	"server_address": {}, "server_port": {}, "rowcount": {},
	"duration_ns": {}, "request_body": {}, "response_body": {},
	"hook_type": {}, "stage": {},
}

// buildSpan produces one span map matching core's buildSpanMap for a dev tool
// call. span_id / trace_id / start_time / end_time are present-but-empty: core
// sets them unconditionally, so emitting the keys (empty) preserves
// exists-parity for a policy that tests their presence, while a local decision
// needs no real span/trace ids or timing.
func buildSpan(req DecisionRequest) map[string]any {
	attrs := map[string]any{}
	span := map[string]any{
		"span_id":       "",
		"trace_id":      "",
		"name":          req.Tool.Name,
		"semantic_type": semanticType(req),
		"start_time":    "",
		"end_time":      "",
	}

	// MCP server is NOT a buildSpanMap key at all — core's buildSpanMap emits no
	// mcp_* field (recon B). We carry `attributes.mcp_server` as a LOCAL-only
	// superset so a local bundle can match on the server if it wants; it has no
	// core counterpart, so it can only ever cause a benign local OVER-match, never
	// an under-match. Real MCP parity with core is via semantic_type=="mcp_tool_call"
	// + the tool name, both of which ARE core-faithful.
	if req.Tool.MCPServer != "" {
		attrs["mcp_server"] = req.Tool.MCPServer
	}
	for k, v := range req.Attributes {
		if _, promoted := promotedSpanKeys[k]; promoted {
			span[k] = v // top-level span key, mirroring core's typed field promotion
		} else {
			attrs[k] = v // arbitrary attribute → per-span attributes (incl. `command`)
		}
	}
	span["attributes"] = attrs
	return span
}

// semanticType derives the span semantic_type core would RECOMPUTE server-side
// (openbox-core ComputeSemanticTypeFromSpan, session.go), so a policy keyed on
// `spans[_].semantic_type` fires identically:
//   - mcp  → "mcp_tool_call"
//   - file → "file_read" / "file_write" / "file_open" / "file_delete" from the
//     file_operation attribute (write & edit → file_write, read → file_read)
//   - shell, unclassified file (Glob/Grep), or anything else → "internal"
//     (core has NO shell semantic type — a shell command is matched on
//     `spans[_].attributes.command`, a LOCAL-only axis, never on semantic_type).
func semanticType(req DecisionRequest) string {
	if req.Tool.Kind == client.ToolMCP {
		return "mcp_tool_call"
	}
	if req.Tool.Kind == client.ToolFile {
		switch attrString(req.Attributes, "file_operation") {
		case "write", "edit":
			return "file_write"
		case "read":
			return "file_read"
		case "open":
			return "file_open"
		case "delete":
			return "file_delete"
		}
	}
	return "internal"
}
