package decision

import (
	"strings"
	"testing"
)

// FuzzRedact the redactor reads attacker-adjacent strings: a file body
// arriving from a hook payload. A panic here is a crashed hook on the enforce
// path, so the property fuzzed is simply "never panics, always terminates";
// plus the one internal consistency claim its callers rely on.
func FuzzRedact(f *testing.F) {
	for _, seed := range []string{
		"", "AWS_SECRET_ACCESS_KEY=abcd1234EXAMPLEabcd1234EXAMPLEabcd1234EX",
		"-----BEGIN RSA PRIVATE KEY-----\nabc\n-----END RSA PRIVATE KEY-----",
		"token = \"\"", "a=b", strings.Repeat("x", 4096), "\x00\xff", "é=ü",
	} {
		f.Add(seed)
	}
	// The Meta key shape, built at runtime: pipes plus a URL-encoded variant.
	f.Add("LLM|" + strings.Repeat("6", 15) + "|" + strings.Repeat("Ab3_x-", 5))
	f.Add("LLM%7C" + strings.Repeat("6", 15) + "%7C" + strings.Repeat("Ab3_x-", 5))
	d := newSecretDetector()
	f.Fuzz(func(t *testing.T, body string) {
		out, _, changed := d.Redact(body)
		if !changed && out != body {
			t.Fatalf("Redact reported no change but rewrote the body: %q -> %q", body, out)
		}
	})
}
