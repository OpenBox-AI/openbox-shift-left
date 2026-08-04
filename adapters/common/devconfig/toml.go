package devconfig

import "strings"

// TopLevelTOMLKeys reports the bare keys a TOML document defines at the TOP
// level — outside every table header — as a set.
//
// This exists because "is this key present in the file" is the wrong question
// for a mandate file, and answering it that way shipped a real hole (E8-S8): a
// Codex `requirements.toml` listed `allow_managed_hooks_only` and the
// approval/sandbox pins BELOW a `[hooks]` header, so TOML bound them as
// `hooks.*` and Codex ignored them — while a substring check still reported the
// machine as managed. A detector that cannot tell a top-level key from a nested
// one will keep asserting mandates that are not in effect.
//
// It is a scanner, not a TOML parser: it tracks table headers and collects bare
// `key = …` assignments seen while no table is open. That is enough to answer
// "did this mandate actually land at the top level", and it keeps the shared
// module dependency-free. Deliberate limitations, all in the safe direction
// (they under-report rather than claim a mandate that is not there):
//
//   - inline tables and arrays spanning multiple lines are not followed, so a
//     key inside a continuation line is not mistaken for a top-level one;
//   - dotted keys (`a.b = 1`) are recorded verbatim, so a caller asking for `a`
//     does not match them;
//   - values are not returned — presence at the right level is the question, and
//     a caller that needs the value should parse the file properly.
func TopLevelTOMLKeys(raw []byte) map[string]bool {
	keys := map[string]bool{}
	inTable := false
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// A table or array-of-tables header opens a scope; everything after it
		// belongs to that table until the next header. There is no way back to
		// the top level in TOML, but keep scanning so the whole file is read.
		if strings.HasPrefix(line, "[") {
			inTable = true
			continue
		}
		if inTable {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		key = strings.Trim(key, `"'`)
		if key != "" {
			keys[key] = true
		}
	}
	return keys
}
