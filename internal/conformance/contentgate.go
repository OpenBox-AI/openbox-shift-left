package conformance

import (
	"strings"
)

type validator struct {
	root map[string]any
}

func (v *validator) resolve(schema map[string]any) map[string]any {
	ref, ok := schema["$ref"].(string)
	if !ok {
		return schema
	}
	const prefix = "#/$defs/"
	if !strings.HasPrefix(ref, prefix) {
		return schema
	}
	name := strings.TrimPrefix(ref, prefix)
	defs, _ := v.root["$defs"].(map[string]any)
	if target, ok := defs[name].(map[string]any); ok {
		return target
	}
	return schema
}

func (v *validator) hasGatedContent(schema map[string]any, inst any) bool {
	schema = v.resolve(schema)

	if gated, _ := schema["x-content-gated"].(bool); gated && inst != nil {
		return true
	}

	obj, ok := inst.(map[string]any)
	if !ok {
		return false
	}
	props, _ := schema["properties"].(map[string]any)
	for name, sub := range props {
		child, present := obj[name]
		if !present {
			continue
		}
		if subSchema, ok := sub.(map[string]any); ok {
			if v.hasGatedContent(subSchema, child) {
				return true
			}
		}
	}
	return false
}
