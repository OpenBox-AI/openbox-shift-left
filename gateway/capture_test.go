package gateway

import (
	"net/http"
	"strings"
	"testing"
)

// Fixtures are ASSEMBLED AT RUNTIME from fragments, never written as literals.
//
// This is not style. A literal secret-shaped string written into a file here is
// rewritten to an ${OPENBOX_REDACTED_*} placeholder on save, which leaves the
// test compiling, passing, and asserting nothing — the fixture it was supposed to
// catch is already gone before the code under test runs. Two assertions below
// were vacuous for exactly that reason until a mutation drill exposed them.
// Fragments are individually low-entropy, so they survive intact.
func bearerFixture() string {
	return "Bearer " + "sk-" + "ant-" + "api03-" + strings.Repeat("A", 8) + strings.Repeat("B", 8) + strings.Repeat("C", 8)
}

func apiKeyFixture() string {
	return "sk-" + "ant-" + "api03-" + strings.Repeat("D", 12) + strings.Repeat("E", 12)
}

func awsKeyFixture() string { return "AKIA" + "IOSFODNN7" + "EXAMPLE" }

func awsSecretFixture() string {
	return "wJalr" + "XUtnFEMI" + "/K7MDENG/" + "bPxRfiCY" + "EXAMPLEKEY"
}

func basicAuthFixture() string { return "Basic " + "dXNlcjpw" + "YXNz" }

// TestCredentialHeadersRedactedByKeyName is the header half of the capture
// contract. Redaction is by KEY NAME, not by pattern-matching the value: a
// provider credential need not look like anything the secret detector knows, so
// leaving it to content heuristics would be leaving it to luck.
func TestCredentialHeadersRedactedByKeyName(t *testing.T) {
	h := http.Header{}
	h.Set("Authorization", bearerFixture())
	h.Set("X-Api-Key", apiKeyFixture())
	h.Set("Proxy-Authorization", basicAuthFixture())
	h.Set("Cookie", "session=abc123")
	h.Set("Set-Cookie", "session=abc123")
	h.Set("Api-Key", "another")
	h.Set("X-Auth-Token", "yet-another")
	h.Set("X-Amz-Security-Token", "aws-one")
	// Not credentials: these have to survive, or the capture is useless.
	h.Set("Anthropic-Version", "2023-06-01")
	h.Set("Anthropic-Beta", "some-beta")
	h.Set("Content-Type", "application/json")

	got := redactHeaders(h)

	for _, name := range []string{
		"Authorization", "X-Api-Key", "Proxy-Authorization", "Cookie",
		"Set-Cookie", "Api-Key", "X-Auth-Token", "X-Amz-Security-Token",
	} {
		v, present := got[name]
		if !present {
			t.Errorf("%s: dropped entirely; the KEY should remain so a reader can see it was sent", name)
			continue
		}
		if v != redactedHeaderValue {
			t.Errorf("%s: got %q want %q", name, v, redactedHeaderValue)
		}
	}
	for name, want := range map[string]string{
		"Anthropic-Version": "2023-06-01",
		"Anthropic-Beta":    "some-beta",
		"Content-Type":      "application/json",
	} {
		if got[name] != want {
			t.Errorf("%s: got %q want %q — a non-credential header was redacted", name, got[name], want)
		}
	}

	// The whole map, flattened, must not contain any credential substring.
	var flat strings.Builder
	for k, v := range got {
		flat.WriteString(k)
		flat.WriteString(v)
	}
	for _, secret := range []string{bearerFixture(), apiKeyFixture(), basicAuthFixture(), "abc123", "aws-one"} {
		if strings.Contains(flat.String(), secret) {
			t.Errorf("captured headers still contain %q", secret)
		}
	}
}

// TestFingerprintIsComputedBeforeRedaction is the ordering control. The
// fingerprint derives FROM the credential, so it has to be taken while the value
// is still there. Computed after redaction it would fingerprint the placeholder —
// identical on every developer, and silently useless for account binding.
func TestFingerprintIsComputedBeforeRedaction(t *testing.T) {
	h := http.Header{}
	h.Set("Authorization", bearerFixture())

	fp := credentialFingerprint(h)
	if fp == "" {
		t.Fatal("no fingerprint computed from a request carrying a credential")
	}
	if fp == credentialFingerprint(redactedOnly()) {
		t.Error("fingerprint matches the one derived from a redacted placeholder: it was computed too late")
	}

	// One-way and stable.
	if strings.Contains(fp, "sk-") || strings.Contains(bearerFixture(), fp) {
		t.Errorf("fingerprint %q leaks part of the credential", fp)
	}
	if again := credentialFingerprint(h); again != fp {
		t.Errorf("fingerprint is not stable: %q then %q", fp, again)
	}

	// A different credential must fingerprint differently, or it cannot identify.
	other := http.Header{}
	other.Set("Authorization", "Bearer "+strings.Repeat("Z", 24))
	if credentialFingerprint(other) == fp {
		t.Error("two different credentials produced the same fingerprint")
	}
}

func redactedOnly() http.Header {
	h := http.Header{}
	h.Set("Authorization", redactedHeaderValue)
	return h
}

// TestFingerprintPrefersAuthorizationThenAPIKey pins which header is fingerprinted
// when both modes are somehow present, so the value is deterministic rather than
// map-iteration-dependent.
func TestFingerprintPrefersAuthorizationThenAPIKey(t *testing.T) {
	both := http.Header{}
	both.Set("Authorization", bearerFixture())
	both.Set("X-Api-Key", apiKeyFixture())

	onlyAuth := http.Header{}
	onlyAuth.Set("Authorization", bearerFixture())

	if credentialFingerprint(both) != credentialFingerprint(onlyAuth) {
		t.Error("fingerprint depends on which other headers are present; it must key on Authorization first")
	}

	onlyKey := http.Header{}
	onlyKey.Set("X-Api-Key", apiKeyFixture())
	if credentialFingerprint(onlyKey) == "" {
		t.Error("an api-key-mode request produced no fingerprint")
	}

	if credentialFingerprint(http.Header{}) != "" {
		t.Error("a request with no credential produced a fingerprint")
	}
}

// TestCaptureBodyIsRedactedThenCapped pins the order and both mechanisms. A cap
// applied before redaction could slice a secret in half and let the halves
// through; a redaction skipped entirely puts the raw value on the wire.
func TestCaptureBodyIsRedactedThenCapped(t *testing.T) {
	body := `{"messages":[{"role":"user","content":"deploy with ` + awsKeyFixture() +
		` and aws_secret_access_key=` + awsSecretFixture() + `"}]}`
	got := captureBody(body)

	if strings.Contains(got, awsSecretFixture()) {
		t.Error("captured body still contains the AWS secret")
	}
	if strings.Contains(got, awsKeyFixture()) {
		t.Error("captured body still contains the AWS key id")
	}
	if !strings.Contains(got, "deploy with") {
		t.Errorf("redaction destroyed the surrounding text: %q", got)
	}

	// Over-cap input is truncated, counted in runes to match the wire cap.
	long := strings.Repeat("é", captureBodyRunes+500)
	capped := captureBody(long)
	if n := len([]rune(capped)); n != captureBodyRunes {
		t.Errorf("captured body is %d runes, want the cap %d", n, captureBodyRunes)
	}
}

// TestCaptureBodyEmptyStaysEmpty keeps a bodyless call from acquiring a body.
func TestCaptureBodyEmptyStaysEmpty(t *testing.T) {
	if got := captureBody(""); got != "" {
		t.Errorf("empty body became %q", got)
	}
}

// TestCaptureOrderingFingerprintThenRedact is the ordering control, and it has to
// run against the PIPELINE rather than the two functions separately: testing
// credentialFingerprint and redactHeaders in isolation cannot observe the order
// they are called in, which is the thing that can actually be got wrong.
func TestCaptureOrderingFingerprintThenRedact(t *testing.T) {
	reqHeaders := http.Header{}
	reqHeaders.Set("Authorization", bearerFixture())
	reqHeaders.Set("Anthropic-Version", "2023-06-01")
	respHeaders := http.Header{}
	respHeaders.Set("Request-Id", "req_abc")
	respHeaders.Set("Set-Cookie", "s=abc123")

	got := Capture(http.MethodPost, "https://api.anthropic.com/v1/messages?beta=true",
		reqHeaders, `{"model":"claude-opus-4"}`, 200, respHeaders, `{"type":"message"}`)

	// Both halves, from one call: the value is gone AND the fingerprint is real.
	if got.RequestHeaders["Authorization"] != redactedHeaderValue {
		t.Errorf("Authorization was not redacted: %q", got.RequestHeaders["Authorization"])
	}
	if got.CredentialFingerprint == "" {
		t.Fatal("no fingerprint: it was computed after redaction, from the placeholder")
	}
	// The specific failure a reversed order produces: every developer's span
	// carrying the fingerprint OF THE PLACEHOLDER.
	placeholderFP := credentialFingerprint(redactedOnly())
	if got.CredentialFingerprint == placeholderFP {
		t.Error("fingerprint equals the placeholder's; the order is reversed")
	}
	if got.CredentialFingerprint != credentialFingerprint(reqHeaders) {
		t.Error("fingerprint does not match the live credential's")
	}

	// Response credentials are redacted too — Set-Cookie is a credential.
	if got.ResponseHeaders["Set-Cookie"] != redactedHeaderValue {
		t.Errorf("Set-Cookie was not redacted: %q", got.ResponseHeaders["Set-Cookie"])
	}
	if got.ResponseHeaders["Request-Id"] != "req_abc" {
		t.Errorf("a non-credential response header was lost: %q", got.ResponseHeaders["Request-Id"])
	}

	// Classification keys, and no query string on the captured URL.
	if got.HTTPMethod != http.MethodPost || got.HTTPStatus != 200 {
		t.Errorf("classification keys wrong: method=%q status=%d", got.HTTPMethod, got.HTTPStatus)
	}
	if got.HTTPURL != "https://api.anthropic.com/v1/messages" {
		t.Errorf("http_url = %q; want host+path with the query dropped", got.HTTPURL)
	}
}
