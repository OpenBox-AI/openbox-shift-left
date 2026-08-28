package conformance

import (
	"strings"
)

// validator holds the root schema so the content-gate walk can resolve local
// $defs references.
//
// It used to also carry a hand-rolled draft-2020-12 structural validator. That
// half is gone — santhosh-tekuri/jsonschema/v6 does it now (see structural.go),
// and it does the whole draft rather than the fourteen keywords this one
// covered. What remains is the part no library performs: the x-content-gated
// walk that enforces INV-2.
type validator struct {
	root map[string]any
}

// resolve resolves a local "#/$defs/name" reference against the root schema.
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

// hasGatedContent walks the instance against the schema's property tree
// (resolving $refs) and reports true if any x-content-gated node has a present,
// non-null instance value — i.e. the event carries content.
//
// It does not descend through oneOf. That is true of the contract as it stands —
// every gated field (top-level `content`, span bodies) is reachable via
// `properties` — and TestGatedFieldsAreReachableWithoutOneOf holds it true,
// because a gated field placed inside a oneOf branch would be invisible here and
// would egress with content-capture OFF.
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
