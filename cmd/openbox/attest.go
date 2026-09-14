package main

import (
	"os"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	obgit "github.com/openbox-ai/openbox-shift-left/internal/adapters/common/git"
)

// attestProvider names the tool a commit hook is running under, from the
// markers the tool leaves in the environment. It decides which private key
// signs the attestation, so the absence of a marker returns "" rather than a
// default: with an "otherwise claude-code" arm, a commit a human made in a
// plain shell would sign as the tool.
//
// CODEX_THREAD_ID is Codex's own documented tier-0 signal and is already read
// on the session path. CLAUDECODE and CLAUDE_CODE_ENTRYPOINT are observed
// present in a live Claude Code process but are not documented upstream, so
// treat them as evidence, not contract: if they disappear, commits made from
// Claude Code stop carrying an attestation and keep their trailer, which is
// the same behaviour as any machine with no credentials. That degradation is
// why the no-marker branch has to be real and tested rather than a fallback.
func attestProvider(getenv func(string) string) string {
	if getenv == nil {
		return ""
	}
	if getenv(obgit.EnvCodexThreadID) != "" {
		return "codex"
	}
	for _, marker := range []string{"CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT"} {
		if getenv(marker) != "" {
			return "claude-code"
		}
	}
	return ""
}

func attestContext() (obgit.AttestContext, bool) {
	creds, err := devconfig.ResolveCredentials()
	if err != nil || creds.DID == "" || creds.PrivateKeyB64 == "" {
		return obgit.AttestContext{}, false
	}

	ctx := obgit.AttestContext{
		DID:           creds.DID,
		PrivateKeyB64: creds.PrivateKeyB64,
		Adapter:       "openbox-cli",
		ThreadID:      os.Getenv(obgit.EnvCodexThreadID),
	}

	return ctx, true
}
