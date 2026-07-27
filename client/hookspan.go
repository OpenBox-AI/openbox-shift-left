package client

import (
	"fmt"
	"regexp"
)

// This file is shift-left's hand-maintained mirror of the base SDK's
// hook-span wire contract. shift-left is Go and cannot import openbox_core,
// so it re-expresses the contract here. Per ADR-0004, shift-left does not
// edit the base SDK — openbox-sdk-python is a read-only reference. Keep these
// definitions byte-faithful to:
//   - openbox-sdk-python/openbox_core/contracts/otel_spans.py
//       HookType, _ROOT_FIELDS_BY_HOOK_TYPE, _DEFAULT_KIND_BY_HOOK
//   - openbox-sdk-python/openbox_core/conformance/fake_core.py
//       _COMMON_ROOT_FIELDS, _FAMILY_ROOT_FIELDS, assert_hook_wire_shape
//
// shell/mcp/tool are the developer-runtime additions to the base families.
// The base SDK also defines http_request/db_query/function_call/llm_call
// families; the developer runtime doesn't emit those, so they're
// intentionally not mirrored here (an unmirrored hook_type is simply not
// family-checked, matching the base SDK's
// `_FAMILY_ROOT_FIELDS.get(hook_type, ())` fall-through).

// HookType is the operation category of a hook (tool-call) span — the Core
// root field `hook_type`. Mirrors otel_spans.py HookType (relevant subset).
type HookType string

const (
	HookFileOperation HookType = "file_operation" // file read/write/open/delete
	HookShell         HookType = "shell"          // command execution (dev-runtime add)
	HookMCP           HookType = "mcp"             // MCP tool call (dev-runtime add)
	HookTool          HookType = "tool"           // generic tool call (dev-runtime add)
)

// CommonRootFields are the flat SpanData keys every hook span carries,
// present even when null. Mirrors fake_core._COMMON_ROOT_FIELDS.
var CommonRootFields = []string{
	"span_id",
	"trace_id",
	"parent_span_id",
	"name",
	"kind",
	"stage",
	"start_time",
	"end_time",
	"duration_ns",
	"attributes",
	"status",
	"events",
	"hook_type",
	"error",
}

// FamilyRootFields are the per-family flat SpanData keys that must exist
// (present even when null) for a given hook_type. Mirrors byte-for-byte
// otel_spans._ROOT_FIELDS_BY_HOOK_TYPE / fake_core._FAMILY_ROOT_FIELDS for
// the families the developer runtime emits.
//
//   - shell_command is content (INV-2): carried only under content-capture,
//     size-capped before egress; null by default.
//   - mcp_*/tool_name are structural identifiers (safe metadata).
var FamilyRootFields = map[HookType][]string{
	HookFileOperation: {"file_path", "file_mode", "file_operation", "bytes_read", "bytes_written"},
	HookShell:         {"shell_command", "shell_exit_code"},
	HookMCP:           {"mcp_server", "mcp_tool", "mcp_method"},
	HookTool:          {"tool_name"},
}

// DefaultKind is the OTel span kind used when a span carries none. Mirrors
// otel_spans._DEFAULT_KIND_BY_HOOK: INTERNAL for local ops (file/shell/tool),
// CLIENT for calls that leave the process (mcp). Unmapped → "INTERNAL".
var DefaultKind = map[HookType]string{
	HookFileOperation: "INTERNAL",
	HookShell:         "INTERNAL",
	HookMCP:           "CLIENT",
	HookTool:          "INTERNAL",
}

// KindFor returns the default span kind for a hook type ("INTERNAL" fallback),
// mirroring the base SDK's `_DEFAULT_KIND_BY_HOOK.get(hook_value, "INTERNAL")`.
func KindFor(h HookType) string {
	if k, ok := DefaultKind[h]; ok {
		return k
	}
	return "INTERNAL"
}

var (
	spanIDRe  = regexp.MustCompile(`^[0-9a-f]{16}$`)
	traceIDRe = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

// AssertHookWireShape checks that a decoded evaluate payload matches the base
// SDK's flat hook-span wire contract. It's the Go mirror of
// fake_core.assert_hook_wire_shape. Returns nil when the payload conforms,
// else a descriptive error (never panics).
//
// The payload must be the JSON-decoded map (not the typed struct) so stray
// nested shapes (`otel`/`openbox`/`data`) and an SDK-set `semantic_type` are
// caught exactly as the base assertion catches them.
func AssertHookWireShape(payload map[string]any) error {
	if et, _ := payload["event_type"].(string); et != "ActivityStarted" {
		return fmt.Errorf("event_type: want ActivityStarted, got %v", payload["event_type"])
	}
	if ht, ok := payload["hook_trigger"].(bool); !ok || !ht {
		return fmt.Errorf("hook_trigger: want true, got %v", payload["hook_trigger"])
	}
	spansRaw, ok := payload["spans"].([]any)
	if !ok || len(spansRaw) == 0 {
		return fmt.Errorf("hook payload must carry non-empty spans")
	}
	if sc, _ := toInt(payload["span_count"]); sc != len(spansRaw) {
		return fmt.Errorf("span_count %v != len(spans) %d", payload["span_count"], len(spansRaw))
	}
	for i, sr := range spansRaw {
		span, ok := sr.(map[string]any)
		if !ok {
			return fmt.Errorf("span[%d] is not an object", i)
		}
		if err := assertSpanShape(i, span); err != nil {
			return err
		}
	}
	return nil
}

func assertSpanShape(i int, span map[string]any) error {
	for _, k := range []string{"otel", "openbox"} {
		if _, present := span[k]; present {
			return fmt.Errorf("span[%d]: nested %q envelope leaked to the wire", i, k)
		}
	}
	if _, present := span["data"]; present {
		return fmt.Errorf("span[%d]: flat hook spans must not carry a data blob", i)
	}
	if _, present := span["semantic_type"]; present {
		return fmt.Errorf("span[%d]: semantic_type is computed by Core, not the SDK", i)
	}
	for _, f := range CommonRootFields {
		if _, present := span[f]; !present {
			return fmt.Errorf("span[%d]: missing common root field %q", i, f)
		}
	}
	if id, _ := span["span_id"].(string); !spanIDRe.MatchString(id) {
		return fmt.Errorf("span[%d]: span_id %q not 16 lowercase hex", i, span["span_id"])
	}
	if id, _ := span["trace_id"].(string); !traceIDRe.MatchString(id) {
		return fmt.Errorf("span[%d]: trace_id %q not 32 lowercase hex", i, span["trace_id"])
	}
	if p := span["parent_span_id"]; p != nil {
		if ps, _ := p.(string); !spanIDRe.MatchString(ps) {
			return fmt.Errorf("span[%d]: parent_span_id %q not 16 lowercase hex", i, p)
		}
	}
	stage, _ := span["stage"].(string)
	if stage != "started" && stage != "completed" {
		return fmt.Errorf("span[%d]: stage %q not in {started, completed}", i, span["stage"])
	}
	ht, _ := span["hook_type"].(string)
	if ht == "" {
		return fmt.Errorf("span[%d]: hook spans must carry hook_type at the root", i)
	}
	for _, f := range FamilyRootFields[HookType(ht)] {
		if _, present := span[f]; !present {
			return fmt.Errorf("span[%d]: missing %s root field %q", i, ht, f)
		}
	}
	if stage == "started" {
		if span["end_time"] != nil {
			return fmt.Errorf("span[%d]: started stage must emit end_time:null", i)
		}
		if span["duration_ns"] != nil {
			return fmt.Errorf("span[%d]: started stage must emit duration_ns:null", i)
		}
	}
	return nil
}

// toInt coerces a JSON-decoded number (float64) or an int to int, mirroring the
// Python assertion's numeric comparison of span_count.
func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case int64:
		return int(n), true
	default:
		return 0, false
	}
}
