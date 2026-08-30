package devconfig

import (
	"github.com/pelletier/go-toml/v2"
)

// TopLevelTOMLKeys reports the bare keys a TOML document defines at the TOP
// level; outside every table; as a set.
//   - A key whose value is a table is not top-level, which is the whole point.
//   - An array-of-tables (`[[servers]]`) is likewise not a top-level key,
//     while a wrapped array of plain arrays is a value and IS reported;
//   - Dotted keys (`a.b = 1`) bind as nesting, so `a` holds a table and
//     neither `a` nor `b` is reported.
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

func isTableValue(v any) bool {
	switch val := v.(type) {
	case map[string]any:
		return true
	case []any:
		for _, e := range val {
			if _, ok := e.(map[string]any); !ok {
				return false
			}
		}
		return len(val) > 0
	}
	return false
}
