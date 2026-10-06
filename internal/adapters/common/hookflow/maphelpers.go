package hookflow

import "strings"

// Helpers every adapter's mapper uses to shape hook fields into event
// metadata. They lived as identical copies in each adapter.

// IsFileSemantic reports whether a tool's semantic type touches a file.
func IsFileSemantic(sem string) bool {
	switch sem {
	case "file_read", "file_write", "file_open", "file_delete":
		return true
	}
	return false
}

// SplitMCPName splits "mcp__<server>__<function>" into its server and
// function. A name with no function part returns it as the server alone.
func SplitMCPName(name string) (server, function string) {
	rest := strings.TrimPrefix(name, "mcp__")
	parts := strings.SplitN(rest, "__", 2)
	if len(parts) == 2 {
		return parts[0], parts[1]
	}
	return parts[0], ""
}

// EnumOr returns v when the closed enum allows it, else "", so a value the
// schema does not declare never egresses.
func EnumOr(v string, allowed map[string]bool) string {
	if allowed[v] {
		return v
	}
	return ""
}

// Compact deletes the empty-string values from m in place and returns it.
// Only strings are dropped: a zero number or false is a real value.
func Compact(m map[string]any) map[string]any {
	for k, v := range m {
		if s, ok := v.(string); ok && s == "" {
			delete(m, k)
		}
	}
	return m
}
