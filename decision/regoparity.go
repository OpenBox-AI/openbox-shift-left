package decision

import (
	"fmt"
	"strings"
)

// This file is the rego-parity surface: the primitives that must agree with how
// the backend's OPA policy would evaluate the same input. They are separated
// from the evaluator so the "must match OPA" obligation is one reviewable file
// rather than scattered through it, and so they can be fuzzed — they parse
// attacker-adjacent strings (a policy path from a bundle, a value from a hook
// payload) and a panic here is a crashed hook.
//
// Known deviations from OPA, deliberate and bounded:
//   - regoCompare falls back to formatting composites with %v, so arrays and
//     objects order by their Go rendering rather than by OPA's own composite
//     ordering. Deterministic, but not OPA-identical; policies compare scalars.

// resolvePath resolves a builder field path against the input document, returning
// the SET of leaf values it addresses (rego existential semantics). It mirrors
// normalizeFieldPath + the generated path expression:
//   - a leading "input." is stripped; the bare "input" addresses the whole doc;
//   - a `.name` segment descends into a map key;
//   - a `[_]` segment iterates every element of an array (existential).
//
// A segment that does not apply to a branch (missing key, non-array `[_]`) drops
// that branch, so an undefined path yields an empty set — the rego "undefined"
// that fails a rule body.
func resolvePath(root map[string]any, field string) []any {
	field = strings.TrimSpace(field)
	if field == "input" {
		return []any{map[string]any(root)}
	}
	field = strings.TrimPrefix(field, "input.")
	if field == "" {
		return nil
	}
	segs := tokenizePath(field)
	current := []any{map[string]any(root)}
	for _, seg := range segs {
		next := make([]any, 0, len(current))
		for _, v := range current {
			if seg == "[_]" {
				if arr, ok := v.([]any); ok {
					next = append(next, arr...)
				}
				continue
			}
			if m, ok := v.(map[string]any); ok {
				if child, present := m[seg]; present {
					next = append(next, child)
				}
			}
		}
		current = next
		if len(current) == 0 {
			return nil
		}
	}
	return current
}

// tokenizePath splits a normalized field path (no "input." prefix) into an
// ordered list of segments — dotted names and `[_]` markers. It matches
// POLICY_BUILDER_FIELD_PATTERN (identifier, then repeated `.identifier` | `[_]`),
// e.g. "spans[_].attributes.command" → ["spans","[_]","attributes","command"].
func tokenizePath(field string) []string {
	var segs []string
	for _, part := range strings.Split(field, ".") {
		// A part may be "name", "name[_]", or "name[_][_]".
		for {
			idx := strings.Index(part, "[_]")
			if idx < 0 {
				if part != "" {
					segs = append(segs, part)
				}
				break
			}
			if idx > 0 {
				segs = append(segs, part[:idx])
			}
			segs = append(segs, "[_]")
			part = part[idx+len("[_]"):]
		}
	}
	return segs
}

// regoCompare returns -1/0/1 for a<b / a==b / a>b under rego's total value order:
// null < boolean < number < string < array < object. Within a type it compares by
// value (numbers numerically, strings lexically, booleans false<true); composite
// types fall back to a deterministic canonical-string comparison. This makes
// `==` type-sensitive (1 == "1" is false) and ordering cross-type-total, exactly
// as OPA evaluates the generated expressions.
func regoCompare(a, b any) int {
	ra, rb := typeRank(a), typeRank(b)
	if ra != rb {
		if ra < rb {
			return -1
		}
		return 1
	}
	switch ra {
	case rankNull:
		return 0
	case rankBool:
		ab, bb := a.(bool), b.(bool)
		if ab == bb {
			return 0
		}
		if !ab {
			return -1
		}
		return 1
	case rankNumber:
		af, bf := toFloat(a), toFloat(b)
		switch {
		case af < bf:
			return -1
		case af > bf:
			return 1
		default:
			return 0
		}
	case rankString:
		return strings.Compare(a.(string), b.(string))
	default:
		return strings.Compare(fmt.Sprintf("%v", a), fmt.Sprintf("%v", b))
	}
}

// regoCount replicates rego count(): length of an array/set, number of keys of an
// object, number of RUNES of a string. A scalar (number/bool/null) is not
// countable → (0,false).
func regoCount(v any) (int, bool) {
	switch t := v.(type) {
	case []any:
		return len(t), true
	case map[string]any:
		return len(t), true
	case string:
		return len([]rune(t)), true
	default:
		return 0, false
	}
}

func typeRank(v any) int {
	switch v.(type) {
	case nil:
		return rankNull
	case bool:
		return rankBool
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
		return rankNumber
	case string:
		return rankString
	default:
		return rankComposite
	}
}

func toFloat(v any) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case float32:
		return float64(t)
	case int:
		return float64(t)
	case int64:
		return float64(t)
	case int32:
		return float64(t)
	case uint:
		return float64(t)
	case uint64:
		return float64(t)
	default:
		return 0
	}
}
