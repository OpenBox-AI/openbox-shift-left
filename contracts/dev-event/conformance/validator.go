package conformance

import (
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"
)

// validator holds the root schema (for $ref resolution) and the content-capture
// posture used to enforce INV-2 (content gated / absent by default).
type validator struct {
	root           map[string]any
	contentEnabled bool
}

// resolveRef resolves a local "#/$defs/name" reference against the root schema.
// Only local $defs refs are used by this contract.
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

// validate structurally validates instance against schema, appending human
// readable errors to errs. It deliberately ignores x-content-gated (handled
// separately by the content-gate pass, so oneOf branch trials never conflate
// a content-posture violation with a structural mismatch).
func (v *validator) validate(schema map[string]any, inst any, path string, errs *[]string) {
	schema = v.resolve(schema)

	if c, ok := schema["const"]; ok {
		if !jsonEqual(inst, c) {
			*errs = append(*errs, fmt.Sprintf("%s: must equal %v", path, c))
		}
	}

	if enum, ok := schema["enum"].([]any); ok {
		matched := false
		for _, e := range enum {
			if jsonEqual(inst, e) {
				matched = true
				break
			}
		}
		if !matched {
			*errs = append(*errs, fmt.Sprintf("%s: %v is not one of the allowed values", path, inst))
		}
	}

	if t, ok := schema["type"].(string); ok {
		if !typeMatches(t, inst) {
			*errs = append(*errs, fmt.Sprintf("%s: expected type %s", path, t))
			return // further keyword checks assume the type held
		}
	}

	switch val := inst.(type) {
	case string:
		if ml, ok := numeric(schema["minLength"]); ok && float64(len(val)) < ml {
			*errs = append(*errs, fmt.Sprintf("%s: shorter than minLength", path))
		}
		if pat, ok := schema["pattern"].(string); ok {
			if re, err := regexp.Compile(pat); err == nil && !re.MatchString(val) {
				*errs = append(*errs, fmt.Sprintf("%s: does not match pattern %q", path, pat))
			}
		}
		// Only date-time is implemented — the sole format the contract uses. An
		// unimplemented format would silently validate nothing, so the schema is
		// guarded against gaining one (TestSchemaUsesOnlySupportedKeywords).
		if f, ok := schema["format"].(string); ok && f == "date-time" {
			if _, err := time.Parse(time.RFC3339, val); err != nil {
				*errs = append(*errs, fmt.Sprintf("%s: not an RFC3339 date-time: %q", path, val))
			}
		}
	case float64:
		if m, ok := numeric(schema["minimum"]); ok && val < m {
			*errs = append(*errs, fmt.Sprintf("%s: less than minimum %v", path, m))
		}
		if schema["type"] == "integer" && val != math.Trunc(val) {
			*errs = append(*errs, fmt.Sprintf("%s: expected integer", path))
		}
	case map[string]any:
		v.validateObject(schema, val, path, errs)
	}

	if oneOf, ok := schema["oneOf"].([]any); ok {
		valid := 0
		for _, sub := range oneOf {
			if subSchema, ok := sub.(map[string]any); ok {
				var subErrs []string
				v.validate(subSchema, inst, path, &subErrs)
				if len(subErrs) == 0 {
					valid++
				}
			}
		}
		if valid != 1 {
			*errs = append(*errs, fmt.Sprintf("%s: matched %d oneOf branches (want exactly 1)", path, valid))
		}
	}
}

func (v *validator) validateObject(schema map[string]any, obj map[string]any, path string, errs *[]string) {
	if req, ok := schema["required"].([]any); ok {
		for _, r := range req {
			name, _ := r.(string)
			if _, present := obj[name]; !present {
				*errs = append(*errs, fmt.Sprintf("%s: missing required property %q", path, name))
			}
		}
	}

	props, _ := schema["properties"].(map[string]any)
	if props != nil {
		for name, sub := range props {
			if child, present := obj[name]; present {
				if subSchema, ok := sub.(map[string]any); ok {
					v.validate(subSchema, child, path+"."+name, errs)
				}
			}
		}
	}

	if add, ok := schema["additionalProperties"].(bool); ok && !add {
		for name := range obj {
			if _, declared := props[name]; !declared {
				*errs = append(*errs, fmt.Sprintf("%s: unknown property %q", path, name))
			}
		}
	}
}

// hasGatedContent walks the instance against the schema's property tree
// (resolving $refs) and reports true if any x-content-gated node has a present,
// non-null instance value — i.e. the event carries content. It does not descend
// through oneOf; the contract's gated fields (top-level `content`, span bodies)
// are all reachable via `properties`.
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

// --- helpers -------------------------------------------------------------

func typeMatches(t string, inst any) bool {
	switch t {
	case "object":
		_, ok := inst.(map[string]any)
		return ok
	case "array":
		_, ok := inst.([]any)
		return ok
	case "string":
		_, ok := inst.(string)
		return ok
	case "boolean":
		_, ok := inst.(bool)
		return ok
	case "number":
		_, ok := inst.(float64)
		return ok
	case "integer":
		f, ok := inst.(float64)
		return ok && f == math.Trunc(f)
	}
	return true
}

func numeric(v any) (float64, bool) {
	f, ok := v.(float64)
	return f, ok
}

// jsonEqual compares two JSON-decoded scalar values.
func jsonEqual(a, b any) bool {
	return fmt.Sprintf("%v", a) == fmt.Sprintf("%v", b)
}
