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

// ValidateDevEvent validates a raw normalized developer-runtime event against
// the dev-event contract.
func ValidateDevEvent(raw []byte, contentCaptureEnabled bool) error {
	schema, err := LoadSchema()
	if err != nil {
		return err
	}

	var inst any
	if err := json.Unmarshal(raw, &inst); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}

	sch, err := compileSchema(schema)
	if err != nil {
		return err
	}

	var errs []string
	if err := sch.Validate(inst); err != nil {
		errs = append(errs, err.Error())
	}

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

	v := &validator{root: schema}
	if !contentCaptureEnabled && v.hasGatedContent(schema, inst) {
		return ErrContentDisabled
	}

	return nil
}
