package decision

import (
	"strings"
	"sync"
	"testing"
)

// TestRedact_NamedPatterns asserts each high-confidence format is detected,
// redacted to an env-var-ref placeholder, and reports its category — and that the
// original secret NEVER survives in the redacted output.
func TestRedact_NamedPatterns(t *testing.T) {
	cases := []struct {
		name   string
		in     string
		secret string // must NOT appear in the output
		cat    string
	}{
		{"aws_key", "aws = AKIAIOSFODNN7EXAMPLE done", "AKIAIOSFODNN7EXAMPLE", "aws_key"},
		{"github_pat", "tok ghp_" + strings.Repeat("a", 36) + " end", "ghp_" + strings.Repeat("a", 36), "github_token"},
		{"github_fine", "github_pat_" + strings.Repeat("A", 22) + " x", "github_pat_" + strings.Repeat("A", 22), "github_token"},
		{"slack", "xoxb-123456789012-abcdefghijkl here", "xoxb-123456789012-abcdefghijkl", "slack_token"},
		{"google", "AIza" + strings.Repeat("B", 35) + " z", "AIza" + strings.Repeat("B", 35), "google_api_key"},
		{"stripe", "sk_live_" + strings.Repeat("c", 24), "sk_live_" + strings.Repeat("c", 24), "stripe_key"},
		{"ai_key", "sk-ant-" + strings.Repeat("d", 40), "sk-ant-" + strings.Repeat("d", 40), "ai_api_key"},
		{"jwt", "eyJhbGciOi.eyJzdWIiOi.abcdefghij tail", "eyJhbGciOi.eyJzdWIiOi.abcdefghij", "jwt"},
	}
	d := newSecretDetector()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, cats, changed := d.Redact(c.in)
			if !changed {
				t.Fatalf("%s: expected a redaction, got none (out=%q)", c.name, out)
			}
			if strings.Contains(out, c.secret) {
				t.Errorf("%s: secret survived in output: %q", c.name, out)
			}
			if !strings.Contains(out, "${"+redactedPrefix) {
				t.Errorf("%s: no env-var placeholder in %q", c.name, out)
			}
			if !containsStr(cats, c.cat) {
				t.Errorf("%s: categories = %v, want to include %q", c.name, cats, c.cat)
			}
		})
	}
}

// TestRedact_PEMBlock redacts a whole multi-line private-key block.
func TestRedact_PEMBlock(t *testing.T) {
	in := "before\n-----BEGIN RSA PRIVATE KEY-----\nMIIEpAIBAAKCAQEA...\n-----END RSA PRIVATE KEY-----\nafter"
	out, cats, changed := newSecretDetector().Redact(in)
	if !changed || strings.Contains(out, "MIIEpAIBAAKCAQEA") {
		t.Fatalf("PEM not redacted: %q", out)
	}
	if !strings.HasPrefix(out, "before\n") || !strings.HasSuffix(out, "\nafter") {
		t.Errorf("surrounding text not preserved: %q", out)
	}
	if !containsStr(cats, "private_key") {
		t.Errorf("categories = %v, want private_key", cats)
	}
}

// TestRedact_AssignmentValueOnly redacts the VALUE of a KEY=VALUE assignment while
// preserving the key and quoting (a partial patch, not a whole-line blank).
func TestRedact_AssignmentValueOnly(t *testing.T) {
	in := `password = "hunter2secret9999"`
	out, cats, changed := newSecretDetector().Redact(in)
	if !changed {
		t.Fatalf("assignment not redacted")
	}
	if strings.Contains(out, "hunter2secret9999") {
		t.Errorf("value survived: %q", out)
	}
	if !strings.Contains(out, "password") || !strings.Contains(out, `"`) {
		t.Errorf("key/quote not preserved: %q", out)
	}
	if !containsStr(cats, "secret_assignment") {
		t.Errorf("categories = %v", cats)
	}
}

// TestRedact_Entropy catches a long high-entropy base64 token in a VALUE position.
func TestRedact_Entropy(t *testing.T) {
	// 40-char base64-class token with high symbol diversity.
	tok := "aB3xK9pLmQ7rT2vW8yZ1cD4fG6hJ0nS5uE7iO3q"
	// Keys chosen to NOT collide with a named-pattern key (api_key/token/…), so the
	// generic entropy fallback (not secret_assignment) is what fires.
	for _, in := range []string{"value: " + tok, `blobval=` + tok, `nonce = "` + tok + `"`} {
		out, cats, changed := newSecretDetector().Redact(in)
		if !changed || strings.Contains(out, tok) {
			t.Fatalf("assigned high-entropy token not redacted: %q → %q", in, out)
		}
		if !containsStr(cats, "entropy") {
			t.Errorf("categories = %v, want entropy", cats)
		}
	}
}

// TestRedact_EntropyBlobsNotCorrupted covers G3 Finding 2: on-by-default entropy
// redaction must NOT corrupt free-floating base64 blobs that are not in a value
// position — data: URIs, PEM certificate lines, minified/embedded assets.
func TestRedact_EntropyBlobsNotCorrupted(t *testing.T) {
	blobs := []string{
		// PEM certificate body lines (64-char base64 runs at line start).
		"-----BEGIN CERTIFICATE-----\nMIIDdzCCAl+gAwIBAgIEAgICADANBgkqhkiG9w0BAQsFADBaMQswCQYDVQQGEwJV\nUzAeFw0xNjA4MTcyMDM2NTVaFw0xNzA4MTcyMDM2NTVaMFoxCzAJBgNVBAYTAlVT\n-----END CERTIFICATE-----",
		// data: URI base64 payload (preceded by a comma, not an assignment).
		"const img = load('data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAAC0lEQVR42mNk')",
	}
	d := newSecretDetector()
	for _, s := range blobs {
		out, cats, changed := d.Redact(s)
		if changed {
			t.Errorf("blob corrupted by entropy pass: %q → %q (cats=%v)", s, out, cats)
		}
	}
}

// TestRedact_NoFalsePositives asserts ordinary prose/code is NOT redacted — the
// low-false-positive posture (AC-6). Includes a git SHA and a UUID (hex ≤4.0 bits,
// below the 4.5 entropy floor) that must survive.
func TestRedact_NoFalsePositives(t *testing.T) {
	safe := []string{
		"package main\n\nimport \"fmt\"\n\nfunc main() { fmt.Println(\"hello world\") }",
		"the quick brown fox jumps over the lazy dog several times in a row here",
		"commit 5abcf11e3fa73ac6ebb5f4095aa00d430ab6b282 fixed the bug", // git SHA (hex)
		"id = 550e8400-e29b-41d4-a716-446655440000 // a uuid",           // UUID (hex+dashes)
		"const maxLen = 512 // a short constant, not a secret",
	}
	d := newSecretDetector()
	for _, s := range safe {
		out, cats, changed := d.Redact(s)
		if changed {
			t.Errorf("false positive: %q redacted → %q (cats=%v)", s, out, cats)
		}
	}
}

// TestRedact_NoOpAndEmpty: empty input and no-secret input return unchanged, nil cats.
func TestRedact_NoOpAndEmpty(t *testing.T) {
	d := newSecretDetector()
	for _, s := range []string{"", "just some ordinary text"} {
		out, cats, changed := d.Redact(s)
		if changed || out != s || cats != nil {
			t.Errorf("Redact(%q) = (%q,%v,%v), want unchanged", s, out, cats, changed)
		}
	}
}

// TestRedact_Idempotentish: re-running on an already-redacted body does not
// re-redact the inserted placeholders (no runaway rewriting).
func TestRedact_PlaceholderNotReRedacted(t *testing.T) {
	d := newSecretDetector()
	once, _, _ := d.Redact("token = AKIAIOSFODNN7EXAMPLE")
	twice, _, changed := d.Redact(once)
	if changed || twice != once {
		t.Errorf("placeholder re-redacted: %q → %q", once, twice)
	}
}

// TestRedact_ConcurrentSafe runs the shared detector from many goroutines (-race).
func TestRedact_ConcurrentSafe(t *testing.T) {
	in := "key AKIAIOSFODNN7EXAMPLE and password=\"supersecretvalue123\""
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, _, changed := defaultSecretDetector.Redact(in)
			if !changed || strings.Contains(out, "AKIAIOSFODNN7EXAMPLE") {
				t.Errorf("concurrent redaction wrong: %q", out)
			}
		}()
	}
	wg.Wait()
}

func containsStr(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// JSON shape. Until ADR-0019 P1 this scanner only ever saw natural language and
// flat KEY=VALUE lines, so nothing exercised what happens when a secret is nested
// inside JSON. Tool output made that the common case: a tool's response IS JSON,
// so a nested value arrives ESCAPED — `{\"key\":\"<tok>\"}` — and an MCP result or
// a `cat config.json` is exactly that shape.
//
// Both generic mechanisms used to miss it, for the same underlying reason and in
// two different places:
//
//   - secretAssignment required the keyword ADJACENT to the `:`/`=`, and a JSON
//     key's closing quote (plus its escaping backslash) sits in between.
//   - precededByAssignment walked back over spaces and quotes to decide whether a
//     high-entropy token sits in a value position, but stopped at a backslash.
//
// The named formats were never affected — they match on the secret's own shape,
// so surrounding syntax is irrelevant. That asymmetry is what made this easy to
// miss: an AWS key in JSON was caught, and a database password was not.
func TestRedact_JSONShapedSecrets(t *testing.T) {
	d := newSecretDetector()
	const entropyTok = "aB3xQ9vK2mZ7pL4wR8tY6nH1jF5sD0gC"

	for _, c := range []struct {
		name, in, secret, cat string
	}{
		{"unescaped json, keyword", `{"password":"hunter2-prod-db-2026"}`, "hunter2-prod-db-2026", "secret_assignment"},
		{"unescaped json, spaced", `{"api_key": "abcdefghijklmnop"}`, "abcdefghijklmnop", "secret_assignment"},
		{"escaped json, keyword", `{"stdout":"{\"password\":\"hunter2-prod-db-2026\"}"}`, "hunter2-prod-db-2026", "secret_assignment"},
		{"escaped json, entropy", `{"stdout":"{\"key\":\"` + entropyTok + `\"}"}`, entropyTok, "entropy"},
	} {
		t.Run(c.name, func(t *testing.T) {
			out, cats, changed := d.Redact(c.in)
			if !changed {
				t.Fatalf("not redacted at all: %s", c.in)
			}
			if strings.Contains(out, c.secret) {
				t.Errorf("secret survived: %s", out)
			}
			if !containsCat(cats, c.cat) {
				t.Errorf("categories = %v, want %q", cats, c.cat)
			}
		})
	}

	// The escaping must survive redaction. The value group must not swallow the
	// backslash that terminates a JSON string, or the redacted output stops being
	// parseable — and this text is carried INSIDE a JSON body on the wire.
	t.Run("escaping survives", func(t *testing.T) {
		in := `{"stdout":"{\"password\":\"hunter2-prod-db-2026\"}"}`
		out, _, _ := d.Redact(in)
		if !strings.Contains(out, `\"}`) {
			t.Errorf("the closing escaped quote was consumed by the redaction: %s", out)
		}
	})

	// A value that legitimately contains backslashes must still be redacted whole.
	// Excluding `\` from the value charset outright would silently stop redacting
	// this, which is why the pattern only refuses a backslash as the LAST character.
	t.Run("windows path value still redacted", func(t *testing.T) {
		in := `password=C:\Users\dev\secret.key`
		out, cats, changed := d.Redact(in)
		if !changed || strings.Contains(out, `C:\Users\dev\secret.key`) {
			t.Errorf("a backslash-bearing value must still be redacted: out=%q cats=%v", out, cats)
		}
	})
}

// A value whose LAST character is a raw backslash must still be redacted.
//
// This is the case the JSON-escape fix cost, and it cost it silently: the value
// group was narrowed to "8+ permitted characters whose last is not a backslash",
// so for a value of exactly 8 characters ending in a backslash no split satisfies
// both halves — the 8-char floor and the non-backslash tail — and the pattern
// matched NOTHING. Not partially: nothing. The entropy pass is no backstop
// either, since a short low-entropy value cannot clear the 4.5-bit floor.
//
// Two properties have to hold at once, which is why the boundary moved out of the
// regex and into the replacement step:
//
//	the secret is redacted            (this test)
//	the JSON escape is not swallowed  (TestRedact_JSONShapedSecrets/escaping_survives)
//
// A change that satisfies either one alone has been shipped before.
func TestRedact_ValueEndingInBackslash(t *testing.T) {
	d := newSecretDetector()

	for _, c := range []struct {
		name, in, secret string
	}{
		// Exactly at the 8-char floor: the total-miss case.
		{"eight chars ending in a backslash", `password=abcdefg\`, "abcdefg"},
		// Above the floor: was redacted before, must stay redacted.
		{"nine chars ending in a backslash", `password=abcdefgh\`, "abcdefgh"},
		// A Windows directory as a secret value — trailing separator included.
		{"windows directory value", `client_secret=C:\Users\dev\`, `C:\Users\dev`},
	} {
		t.Run(c.name, func(t *testing.T) {
			out, cats, changed := d.Redact(c.in)
			if !changed {
				t.Fatalf("not redacted at all: %s", c.in)
			}
			if strings.Contains(out, c.secret) {
				t.Errorf("secret survived: %s", out)
			}
			if !containsCat(cats, "secret_assignment") {
				t.Errorf("categories = %v, want secret_assignment", cats)
			}
		})
	}

	// A value that is ONLY backslashes carries no secret material, and stripping
	// the trailing ones would leave the placeholder covering nothing. Redacting it
	// would report a redaction that did not happen.
	t.Run("all-backslash value is left alone", func(t *testing.T) {
		in := `password=` + strings.Repeat(`\`, 10)
		out, _, changed := d.Redact(in)
		if changed || out != in {
			t.Errorf("a value of only backslashes must not report a redaction: out=%q changed=%v", out, changed)
		}
	})
}

// containsCat reports whether cats contains want.
func containsCat(cats []string, want string) bool {
	for _, c := range cats {
		if c == want {
			return true
		}
	}
	return false
}
