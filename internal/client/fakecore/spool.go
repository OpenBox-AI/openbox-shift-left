package fakecore

import (
	"os"
	"path/filepath"
	"strings"
)

// SpoolLines reads the spooled DevEvent JSONL, pre-flush. This is the only
// place the client's own 33-type vocabulary is observable: Spool.Append writes
// client.DevEvent, while the wire carries the four-value projection.
//
// Returns raw lines rather than validating them here, so the caller owns the
// schema dependency and fakecore keeps its import wall.
func SpoolLines(dir string) [][]byte {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out [][]byte
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			out = append(out, []byte(line))
		}
	}
	return out
}

// SpooledEventTypes reports which DevEvent types are present in the spool.
//
// Needed because what reaches the spool depends on posture: under enforce a
// gated PreToolUse egresses synchronously at the gate and its observe copy is
// discarded, so ToolCall and PromptSubmitted never spool at all. A suite that
// only ever ran enforced would validate ToolResult and believe it had covered
// tool events.
func SpooledEventTypes(dir string) map[string]int {
	types := map[string]int{}
	for _, line := range SpoolLines(dir) {
		s := string(line)
		const key = `"event_type":"`
		i := strings.Index(s, key)
		if i < 0 {
			continue
		}
		rest := s[i+len(key):]
		j := strings.Index(rest, `"`)
		if j < 0 {
			continue
		}
		types[rest[:j]]++
	}
	return types
}
