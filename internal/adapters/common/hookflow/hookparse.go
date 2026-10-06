package hookflow

import (
	"encoding/json"
	"fmt"
	"io"
)

// MaxHookPayload bounds what a hook reads from its stdin: 32 MiB.
const MaxHookPayload = 32 << 20

// ParseHookEvent decodes one hook's stdin JSON into an adapter's HookEvent,
// through a size-bounded reader (MaxHookPayload).
func ParseHookEvent[E any](r io.Reader) (*E, error) {
	dec := json.NewDecoder(io.LimitReader(r, MaxHookPayload))
	var ev E
	if err := dec.Decode(&ev); err != nil {
		if err == io.EOF {
			return nil, fmt.Errorf("empty hook payload")
		}
		return nil, fmt.Errorf("parse hook payload: %w", err)
	}
	return &ev, nil
}

// ParseHookName validates a hook name against an adapter's known set; tool is
// the provider's display name ("Claude Code", "Codex", "Muse").
func ParseHookName[H ~string](s string, known map[H]bool, tool string) (H, error) {
	h := H(s)
	if !known[h] {
		return "", fmt.Errorf("unknown %s hook %q", tool, s)
	}
	return h, nil
}
