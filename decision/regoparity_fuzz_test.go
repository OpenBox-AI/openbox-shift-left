package decision

import (
	"strings"
	"testing"
)

// These parse attacker-adjacent strings: a path comes from a policy bundle, a
// value from a hook payload. A panic here is a crashed hook on the enforce path,
// so the property being fuzzed is simply "never panics, always terminates".
func FuzzTokenizePath(f *testing.F) {
	for _, seed := range []string{
		"", ".", "..", "tool.name", "a.b.c", `a["b"].c`, `a['b']`, "[0]", "a[0][1]",
		`a["b.c"]`, `["`, `a[`, `a]`, strings.Repeat("a.", 200), "\x00", "…",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, path string) {
		_ = tokenizePath(path)
	})
}

func FuzzResolvePath(f *testing.F) {
	for _, seed := range []string{"tool.name", "attributes.command", `a["b"]`, "", "[0]"} {
		f.Add(seed)
	}
	input := map[string]any{
		"tool":       map[string]any{"name": "Bash", "kind": "shell"},
		"attributes": map[string]any{"command": "rm -rf /", "count": 3.0},
		"list":       []any{"a", "b"},
		"nested":     map[string]any{"deep": map[string]any{"deeper": true}},
	}
	f.Fuzz(func(t *testing.T, path string) {
		_ = resolvePath(input, path)
	})
}

// Redact runs over file bodies a developer is about to write, so the same
// no-panic property applies — and additionally it must never return a body
// shorter than its own placeholder logic implies, i.e. it must terminate.
func FuzzRedact(f *testing.F) {
	for _, seed := range []string{
		"", "AWS_SECRET_ACCESS_KEY=abcd1234EXAMPLEabcd1234EXAMPLEabcd1234EX",
		"-----BEGIN RSA PRIVATE KEY-----\nabc\n-----END RSA PRIVATE KEY-----",
		"token = \"\"", "a=b", strings.Repeat("x", 4096), "\x00\xff", "é=ü",
	} {
		f.Add(seed)
	}
	d := newSecretDetector()
	f.Fuzz(func(t *testing.T, body string) {
		out, _, changed := d.Redact(body)
		if !changed && out != body {
			t.Fatalf("Redact reported no change but rewrote the body: %q -> %q", body, out)
		}
	})
}
