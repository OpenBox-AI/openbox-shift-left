package conformance

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ErrContentDisabled is returned when an event carries content (a populated
// x-content-gated field) while content-capture is disabled (INV-2).
var ErrContentDisabled = errors.New("event carries content while content-capture is disabled (INV-2)")

// ValidateDevEvent validates a raw normalized developer-runtime event
// against the dev-event contract.
//
// contentCaptureEnabled reflects the org's content posture. When false (the
// default), any event carrying content is rejected with ErrContentDisabled.
//
// It returns nil for a conformant event, or an error describing every problem.
func ValidateDevEvent(raw []byte, contentCaptureEnabled bool) error {
	schema, err := LoadSchema()
	if err != nil {
		return err
	}

	var inst any
	if err := json.Unmarshal(raw, &inst); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}

	v := &validator{root: schema, contentEnabled: contentCaptureEnabled}

	var errs []string
	v.validate(schema, inst, "$", &errs)

	// Extra contract rule not expressible in the schema subset:
	// tool.kind == "mcp" requires tool.mcp_server.
	if obj, ok := inst.(map[string]any); ok {
		if tool, ok := obj["tool"].(map[string]any); ok {
			if tool["kind"] == "mcp" {
				if s, ok := tool["mcp_server"].(string); !ok || s == "" {
					errs = append(errs, "$.tool: mcp_server is required when kind=mcp")
				}
			}
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("event is not conformant:\n  - %s", strings.Join(errs, "\n  - "))
	}

	// Content-gate pass (INV-2): independent of structural validity so a
	// posture violation is never masked by a oneOf branch trial.
	if !contentCaptureEnabled && v.hasGatedContent(schema, inst) {
		return ErrContentDisabled
	}

	return nil
}
