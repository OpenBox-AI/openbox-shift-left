package transport

import (
	"strings"
	"testing"
)

// TestCandidatesForHost: a host several providers reach names every one of
// them, most specific carrier first, so a caller can disambiguate by carrier
// instead of trusting whichever row happened to match first.
func TestCandidatesForHost(t *testing.T) {
	for _, tc := range []struct {
		host string
		want []string
	}{
		{"api.anthropic.com", []string{"claude-code"}},
		{"api.anthropic.com:443", []string{"claude-code"}},
		{"claude.ai", []string{"claude-code"}},
		{"api.openai.com", []string{"codex"}},
		{"chatgpt.com", []string{"codex"}},
		{"chat.chatgpt.com", []string{"codex"}},
		{"api.meta.ai", []string{"claude-code", "muse", "codex"}},
		{"API.META.AI:443", []string{"claude-code", "muse", "codex"}},
		{"auth.openai.com", nil},
		{"example.com", nil},
		{"", nil},
	} {
		got := CandidatesForHost(tc.host)
		if strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Errorf("CandidatesForHost(%q) = %v, want %v", tc.host, got, tc.want)
		}
	}
}

// TestSharedHostRowsAreIntercepted: api.meta.ai is in the claude-code, codex
// and muse rows, so any one of them alone intercepts it.
func TestSharedHostRowsAreIntercepted(t *testing.T) {
	for _, p := range []string{"claude-code", "codex", "muse"} {
		if !AllowlistFor(p).Allows("api.meta.ai:443") {
			t.Errorf("provider %q does not intercept api.meta.ai", p)
		}
	}
}
