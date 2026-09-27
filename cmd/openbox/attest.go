package main

import (
	obgit "github.com/openbox-ai/openbox-shift-left/internal/adapters/common/git"
)

// attestProvider names the tool a commit hook is running under, from the
// markers the tool leaves in the environment. The marker routes commit
// events; it never signs anything -- the absence of a marker returns ""
// rather than a default, so a commit a human made in a plain shell is never
// attributed to a tool. It decides commit-event routing (newCommitSink,
// "agent commits only"): the same marker, checked against the resolved
// session's own tool, is what lets a hand commit in a governed worktree keep
// its trailer without ever producing a CommitCreated event.
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
