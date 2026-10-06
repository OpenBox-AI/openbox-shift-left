package git

import (
	"strings"
	"testing"
)

// TestCanonicalRemote_StripsCredentials iNV-1: a canonical remote identity is
// derived from the origin URL for commit metadata, so a credential embedded
// in that URL must never survive into it.
func TestCanonicalRemote_StripsCredentials(t *testing.T) {
	cases := map[string]string{
		"git@github.com:acme/app.git":                    "github.com/acme/app",
		"https://github.com/acme/app.git":                "github.com/acme/app",
		"https://x-token:ghp_secret@github.com/acme/app": "github.com/acme/app",
		"ssh://git@github.com/acme/app.git":              "github.com/acme/app",
		"":                                               "",
	}
	for in, want := range cases {
		if got := canonicalRemote(in); got != want {
			t.Errorf("canonicalRemote(%q) = %q, want %q", in, got, want)
		}
		if strings.Contains(canonicalRemote(in), "ghp_secret") {
			t.Errorf("credential leaked into the canonical repo identity from %q", in)
		}
	}
}
