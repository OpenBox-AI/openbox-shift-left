package devconfig

import (
	"github.com/pelletier/go-toml/v2"
)

// TopLevelTOMLKeys reports the bare keys a TOML document defines at the TOP
// level — outside every table — as a set.
//
// This exists because "is this key present in the file" is the wrong question
// for a mandate file, and answering it that way shipped a real hole (E8-S8): a
// Codex `requirements.toml` listed `allow_managed_hooks_only` and the
// approval/sandbox pins BELOW a `[hooks]` header, so TOML bound them as
// `hooks.*` and Codex ignored them — while a substring check still reported the
// machine as managed. A detector that cannot tell a top-level key from a nested
// one will keep asserting mandates that are not in effect.
//
// It was a line scanner and is now a real TOML parse, because the scanner had a
// bug in the one direction that matters. It treated ANY line whose first
// character was `[` as a table header and skipped the rest of the file, so a
// continuation line beginning with `[` — inside a multi-line basic string, or an
// element of a wrapped array-of-arrays — hid every later top-level key. The
// consumer is `codexMandated`, so the failure was a mandated machine reading as
// UNMANDATED: enforcement reported absent while it was in force.
// TestTopLevelTOMLKeys_BracketLeadingContinuationDoesNotHideLaterKeys pins both
// shapes.
//
// Deliberate limitations, unchanged in spirit and still all in the safe
// direction — they under-report rather than claim a mandate that is not there:
//
//   - a key whose value is a table is not top-level, which is the whole point.
//     An INLINE table (`key = {…}`) is indistinguishable from a table header
//     once parsed into a map, so it is excluded too. None of the mandate keys is
//     plausibly an inline table, and excluding is the safe direction;
//   - an array-of-tables (`[[servers]]`) is likewise not a top-level key, while a
//     wrapped array of plain arrays is a value and IS reported;
//   - dotted keys (`a.b = 1`) bind as nesting, so `a` holds a table and neither
//     `a` nor `b` is reported. A caller asking for `b` must not match — that is
//     the E8-S8 property. (The scanner used to also report the literal string
//     "a.b"; nothing consumed it, and a real parser cannot reconstruct it.);
//   - values are not returned — presence at the right level is the question, and
//     a caller that needs the value should parse the file properly;
//   - a document that does not parse yields no keys, which reads as "no mandate".
//     Safe direction again, and the same answer the scanner gave for a file it
//     could not make sense of.
func TopLevelTOMLKeys(raw []byte) map[string]bool {
	keys := map[string]bool{}

	var doc map[string]any
	if err := toml.Unmarshal(raw, &doc); err != nil {
		return keys
	}

	for k, v := range doc {
		if isTableValue(v) {
			continue
		}
		keys[k] = true
	}
	return keys
}

// isTableValue reports whether a decoded value is a table or an array of tables,
// i.e. a structure that opens a scope rather than a value assigned to a key.
func isTableValue(v any) bool {
	switch val := v.(type) {
	case map[string]any:
		return true
	case []any:
		// An array of tables opens scopes; an array of anything else is a value.
		// An empty array is a value.
		for _, e := range val {
			if _, ok := e.(map[string]any); !ok {
				return false
			}
		}
		return len(val) > 0
	}
	return false
}
