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
