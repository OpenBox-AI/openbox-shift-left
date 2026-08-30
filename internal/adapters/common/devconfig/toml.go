package devconfig

import (
	"github.com/pelletier/go-toml/v2"
)

// TopLevelTOMLKeys reports the bare keys a TOML document defines at the TOP
// level; outside every table; as a set. A detector that cannot tell a top-
// level key from a nested one will keep asserting mandates that are not in
// effect.
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
