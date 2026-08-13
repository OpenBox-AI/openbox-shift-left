package decision

import (
	"strings"
	"testing"
)

// The redactor reads attacker-adjacent strings: a file body arriving from a hook
// payload. A panic here is a crashed hook on the enforce path, so the property
// fuzzed is simply "never panics, always terminates" — plus the one internal
// consistency claim its callers rely on.
//
// It lived in regoparity_fuzz_test.go alongside two rego-path fuzzers. ADR-0017
// deleted the rego parser and would have taken this with it, silently ending
// fuzz coverage of code that still runs on every gated call with a body.
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
		// The callers branch on `changed` and skip the rewrite when it is false,
		// so a false negative here would silently drop a redaction.
		if !changed && out != body {
			t.Fatalf("Redact reported no change but rewrote the body: %q -> %q", body, out)
		}
	})
}
