package sessionkey

import "testing"

// TestAttributeProxy: dispatch from candidate providers to exactly one by its
// carrier. Nothing is guessed: no carrier and more than one carrier both skip.
func TestAttributeProxy(t *testing.T) {
	cc := map[string]string{"X-Claude-Code-Session-Id": "cc-1"}
	codex := map[string]string{"X-Client-Request-Id": "thread-1"}
	codexOriginator := map[string]string{"X-Client-Request-Id": "thread-1", "Originator": "codex_cli_rs"}
	both := map[string]string{"X-Claude-Code-Session-Id": "cc-1", "X-Client-Request-Id": "thread-1", "Originator": "codex_cli_rs"}
	shared := []string{"claude-code", "muse", "codex"}

	for _, tc := range []struct {
		name       string
		candidates []string
		headers    map[string]string
		want       Provider
		wantID     string
		wantSkip   string
	}{
		{"cc alone on its own host", []string{"claude-code"}, cc, ClaudeCode, "cc-1", ""},
		{"codex alone on its own host needs no originator", []string{"codex"}, codex, Codex, "thread-1", ""},
		{"cc header on the shared host", shared, cc, ClaudeCode, "cc-1", ""},
		{"codex with originator on the shared host", shared, codexOriginator, Codex, "thread-1", ""},
		{"codex request id alone on the shared host is not codex-only", shared, codex, "", "", SkipNoProviderCarrier},
		{"neither carrier on the shared host", shared, map[string]string{}, "", "", SkipNoProviderCarrier},
		{"two carriers on the shared host", shared, both, "", "", SkipAmbiguousCarrier},
		{"a foreign originator does not make it codex", shared,
			map[string]string{"X-Client-Request-Id": "t", "Originator": "some_other_tool"}, "", "", SkipNoProviderCarrier},
		{"no candidates", nil, cc, "", "", SkipNoProviderCarrier},
		{"muse has no carrier", []string{"muse"}, cc, "", "", SkipNoProviderCarrier},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, id, skip := AttributeProxy(tc.candidates, tc.headers)
			if p != tc.want || id != tc.wantID || skip != tc.wantSkip {
				t.Errorf("AttributeProxy = (%q, %q, %q), want (%q, %q, %q)", p, id, skip, tc.want, tc.wantID, tc.wantSkip)
			}
		})
	}
}

// TestCarrierHeaderNames: the header a warning should name for each provider.
func TestCarrierHeaderNames(t *testing.T) {
	if got := CarrierHeader(ClaudeCode); got != "X-Claude-Code-Session-Id" {
		t.Errorf("CarrierHeader(ClaudeCode) = %q", got)
	}
	if got := CarrierHeader(Codex); got != "X-Client-Request-Id" {
		t.Errorf("CarrierHeader(Codex) = %q", got)
	}
	if got := CarrierHeader(Muse); got != "" {
		t.Errorf("CarrierHeader(Muse) = %q, want none", got)
	}
}
