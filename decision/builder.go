package decision

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/openbox-ai/openbox-shift-left/client"
)

// builderEvaluator (ADR-0005) is the pure-Go native evaluator for a backend
// policy_builder config. It replicates — without a rego engine, no cgo, no
// OPA dependency — the verdict the backend's builder→rego compilation would
// yield when run by core's external OPA against the same input document
// (BuildOPAInput, shaped to core's buildSpanMap in input.go).
//
// It is distinct from bundleEvaluator on purpose:
//   - bundleEvaluator = max-severity across the legacy hand-authored local
//     Rules format (a BLOCK rule wins over a CONSTRAIN rule regardless of
//     order).
//   - builderEvaluator = first-match by rule order (generatePolicyBuilderRego
//     emits `not rule_0_match … not rule_{i-1}_match` guard clauses so an
//     earlier rule always wins), with the default `ALLOW`.
//
// Reusing Rules for a builder policy would silently change its precedence,
// so SetBundle selects builderEvaluator when Bundle.PolicyBuilder != nil and
// keeps bundleEvaluator for the legacy Rules format.
//
// Pure, no I/O, concurrency-safe (matches the Evaluator contract): it reads
// only the immutable parsed config + the per-request input.
type builderEvaluator struct {
	cfg *PolicyBuilderConfig
	// policyID stamps the resolved Evaluation for audit/telemetry parity
	// with core (the generated rego's `result` object carries only
	// {decision, reason}; the policy identity is the enclosing
	// PolicyEntity.id, carried on the bundle pin).
	policyID string
}

func newBuilderEvaluator(cfg *PolicyBuilderConfig, policyID string) *builderEvaluator {
	return &builderEvaluator{cfg: cfg, policyID: policyID}
}

// PolicyBuilderConfig mirrors openbox-backend's PolicyBuilderConfig
// (policy-builder.util.ts): {version:1, rules:[…]}. It is the pre-compilation
// structured form of a builder-authored policy — the form `dev sync` fetches
// from config.policy_builder and this evaluator interprets directly.
type PolicyBuilderConfig struct {
	Version int                 `json:"version"`
	Rules   []PolicyBuilderRule `json:"rules"`
}

// PolicyBuilderRule mirrors PolicyBuilderRule. Precedence is FIRST-MATCH by slice
// order; there is NO priority field. Decision is UPPERCASE
// ALLOW|REQUIRE_APPROVAL|BLOCK|HALT (decisionToVerdict is case-insensitive).
type PolicyBuilderRule struct {
	ID         string                   `json:"id,omitempty"`
	Name       string                   `json:"name,omitempty"`
	Decision   string                   `json:"decision"`
	Reason     string                   `json:"reason,omitempty"`
	MatchMode  string                   `json:"matchMode"` // "all" (AND) | "any" (OR)
	Conditions []PolicyBuilderCondition `json:"conditions"`
}

// PolicyBuilderCondition mirrors PolicyBuilderCondition. field is a dotted path
// (optionally `input.`-prefixed) with `[_]` existential array segments; operator
// is one of the 9 recognized operators; transform is value|count; value is always
// a STRING (coerced per valueType).
type PolicyBuilderCondition struct {
	ID        string `json:"id,omitempty"`
	Field     string `json:"field"`
	Operator  string `json:"operator"`
	Transform string `json:"transform"` // "value" | "count"
	Value     string `json:"value"`
	ValueType string `json:"valueType"` // "string" | "number" | "boolean"
}

// Evaluate resolves the FIRST rule (by order) whose predicate holds against the
// request's BuildOPAInput document and returns its decision→verdict + reason +
// the policy id. No rule matches → the default ALLOW (mirrors the generated
// rego's `default result = {"decision":"ALLOW","reason":null}`). Never
// max-severity.
func (e *builderEvaluator) Evaluate(req DecisionRequest) client.Evaluation {
	if e == nil || e.cfg == nil {
		return client.Evaluation{Verdict: client.VerdictAllow}
	}
	input := BuildOPAInput(req)
	for i := range e.cfg.Rules {
		r := &e.cfg.Rules[i]
		if ruleMatches(r, input) {
			return client.Evaluation{
				Verdict:  decisionToVerdict(r.Decision),
				Reason:   r.Reason,
				PolicyID: e.policyID,
			}
		}
	}
	return client.Evaluation{Verdict: client.VerdictAllow}
}

// ruleMatches evaluates one rule's conditions against the input, combining them
// per matchMode: "any" = OR (the generated rego emits one rule block per
// condition, sharing a name — a set of OR alternatives), anything else ("all",
// the default) = AND (a single block ANDing every expression). An empty
// condition set never matches (the backend rejects it at author time; we treat
// it as no-match to stay fail-open — an unmatchable rule can never over-block).
func ruleMatches(r *PolicyBuilderRule, input map[string]any) bool {
	if len(r.Conditions) == 0 {
		return false
	}
	if strings.EqualFold(r.MatchMode, "any") {
		for i := range r.Conditions {
			if conditionHolds(&r.Conditions[i], input) {
				return true
			}
		}
		return false
	}
	for i := range r.Conditions {
		if !conditionHolds(&r.Conditions[i], input) {
			return false
		}
	}
	return true
}

// conditionHolds evaluates ONE condition, exactly mirroring the rego
// buildConditionExpression emits (policy-builder.util.ts):
//
//   - exists / not_exists → count of resolved path values > 0 / == 0.
//   - contains → case-insensitive substring: contains(lower(sprintf("%v",[path])),
//     lower(value)); existential over a `[_]` path (true if ANY element matches);
//     the value is ALWAYS treated as a string (ignores valueType).
//   - equals/not_equals/ordering → the target expression (the resolved path value,
//     or its `count` when transform=count) compared to the typed value literal
//     (buildValueLiteral). Existential: a `[_]` path holds if ANY element satisfies.
func conditionHolds(c *PolicyBuilderCondition, input map[string]any) bool {
	switch c.Operator {
	case "exists":
		return len(resolvePath(input, c.Field)) > 0
	case "not_exists":
		return len(resolvePath(input, c.Field)) == 0
	case "contains":
		// contains(lower(sprintf("%v", [PATH])), lower(value)): the value literal is
		// the raw string regardless of valueType. Existential across resolved values.
		needle := strings.ToLower(c.Value)
		for _, v := range resolvePath(input, c.Field) {
			if strings.Contains(strings.ToLower(fmt.Sprintf("%v", v)), needle) {
				return true
			}
		}
		return false
	}

	// equals / not_equals / greater_than[_or_equal] / less_than[_or_equal].
	literal, ok := valueLiteral(c)
	if !ok {
		return false // unknown operator
	}

	targets := targetValues(c, input)
	// An undefined target makes the generated rego rule body undefined → the
	// condition does not hold. For not_equals, note the rego is `PATH != literal`,
	// which is ALSO undefined (false) when PATH is undefined — NOT vacuously true.
	if len(targets) == 0 {
		return false
	}
	for _, t := range targets {
		if compareOp(c.Operator, t, literal) {
			return true // existential: ANY element satisfies
		}
	}
	return false
}

// targetValues yields the operand values the operator compares against the
// literal. For transform=value it is the resolved path value(s). For
// transform=count (only honored when the operator supports it, i.e. not
// contains/exists/not_exists) it is the COLLECTION LENGTH as a number:
//   - a `[_]` path → the count of matched elements (len of resolved values);
//   - otherwise → count() of the single resolved value (array/object/string/set
//     length), undefined (→ no target) when the value is not countable.
func targetValues(c *PolicyBuilderCondition, input map[string]any) []any {
	if c.Transform == "count" && countTransformSupported(c.Operator) {
		if strings.Contains(c.Field, "[_]") {
			return []any{float64(len(resolvePath(input, c.Field)))}
		}
		vals := resolvePath(input, c.Field)
		if len(vals) == 0 {
			return nil // count(undefined) → undefined
		}
		n, ok := regoCount(vals[0])
		if !ok {
			return nil // count(scalar) → type error → undefined
		}
		return []any{float64(n)}
	}
	return resolvePath(input, c.Field)
}

func countTransformSupported(op string) bool {
	return op != "contains" && op != "exists" && op != "not_exists"
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

// valueLiteral coerces the condition's string value per valueType, mirroring
// buildValueLiteral: number → float64 (NaN→0); boolean → true iff exactly "true";
// string (and any other) → the raw string. The bool reports a recognized operator
// (always true here; unknown operators are filtered by compareOp).
//
// count→number coercion: the backend parser forces valueType="number"
// whenever transform=="count". We mirror that defensively so a count
// condition whose stored valueType is (wrongly) "string" still compares its
// numeric count target against a numeric literal — never a float vs a
// quoted string (which regoCompare would rank cross-type and silently never
// match).
func valueLiteral(c *PolicyBuilderCondition) (any, bool) {
	valueType := c.ValueType
	if c.Transform == "count" && countTransformSupported(c.Operator) {
		valueType = "number"
	}
	switch valueType {
	case "number":
		f, err := strconv.ParseFloat(strings.TrimSpace(c.Value), 64)
		if err != nil {
			f = 0 // Number(value) NaN → 0
		}
		return f, true
	case "boolean":
		return c.Value == "true", true
	default:
		return c.Value, true
	}
}

// compareOp applies a comparison operator between a resolved target and the typed
// literal, using regoCompare's total ordering so cross-type comparisons match
// rego semantics (equals across differing types is never true; numbers order
// before strings, etc.).
func compareOp(op string, target, literal any) bool {
	switch op {
	case "equals":
		return regoCompare(target, literal) == 0
	case "not_equals":
		return regoCompare(target, literal) != 0
	case "greater_than":
		return regoCompare(target, literal) > 0
	case "greater_than_or_equal":
		return regoCompare(target, literal) >= 0
	case "less_than":
		return regoCompare(target, literal) < 0
	case "less_than_or_equal":
		return regoCompare(target, literal) <= 0
	default:
		return false
	}
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

const (
	rankNull = iota
	rankBool
	rankNumber
	rankString
	rankComposite
)

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
