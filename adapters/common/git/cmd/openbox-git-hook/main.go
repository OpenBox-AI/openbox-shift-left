// Command openbox-git-hook is the standalone entrypoint the OpenBox
// `prepare-commit-msg` hook shells out to (STORY-SL-5). It stamps an
// `OpenBox-Session:` trailer onto the commit message so the pushed commit can be
// bound to its session(s) server-side (SL-6).
//
// SAFETY CONTRACT (observe-only, the git analog of SL-4's INV-3):
//   - It ALWAYS exits 0. A non-zero exit from prepare-commit-msg ABORTS the
//     developer's commit; this binary must never do that.
//   - Any failure (bad args, git error, unreadable message file) is logged to
//     stderr and swallowed; the commit proceeds, unstamped, and SL-6 marks it
//     unattributed. Nothing is written to stdout.
//
// Per OD17 (single `openbox` engine) this standalone binary is folded into
// `openbox hook git prepare-commit-msg` in a follow-up (mirrors how SL-4 shipped
// cmd/openbox-cc-hook, later absorbed by SL4-WIRE-2). The library it calls
// (adapters/common/git) is unchanged by that move.
package main

import (
	"fmt"
	"log"
	"os"

	obgit "github.com/openbox-ai/openbox-shift-left/adapters/common/git"
)

func main() {
	// Guarantee exit 0 even on an unexpected panic — never abort a commit.
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "openbox-git-hook: recovered: %v\n", r)
		}
		os.Exit(0)
	}()

	logger := log.New(os.Stderr, "openbox-git-hook: ", 0)

	if len(os.Args) < 2 {
		logger.Printf("usage: openbox-git-hook <prepare-commit-msg|post-commit|install> [args...]")
		return
	}
	sub := os.Args[1]
	rest := os.Args[2:]

	g := obgit.Git{} // ambient git, current repo
	resolver := obgit.SessionResolver{}

	switch sub {
	case "prepare-commit-msg":
		if _, err := g.RunPrepareCommitMsg(rest, resolver, logger.Printf); err != nil {
			logger.Printf("%v", err) // logged only — still exit 0
		}
	case "post-commit":
		// Optional, non-authoritative local notes mirror (S3 R5).
		if err := g.WriteNoteMirror("HEAD", g.ResolveSessions(resolver)); err != nil {
			logger.Printf("note mirror skipped: %v", err)
		}
	case "install":
		runInstall(logger, g, rest)
	default:
		logger.Printf("unknown subcommand %q", sub)
	}
}

// runInstall writes the prepare-commit-msg hook into the current repo's hooks
// dir, pointing it back at this binary. Convenience for local/dev use; the CLI
// (SL-2 wiring) is the production installer.
func runInstall(logger *log.Logger, g obgit.Git, _ []string) {
	hooksDir, err := g.HooksDir()
	if err != nil {
		logger.Printf("install: locating hooks dir: %v", err)
		return
	}
	self, err := os.Executable()
	if err != nil || self == "" {
		self = "openbox-git-hook"
	}
	if err := obgit.InstallHook(hooksDir, obgit.HookConfig{Command: self}); err != nil {
		logger.Printf("install: %v", err)
		return
	}
	logger.Printf("installed prepare-commit-msg hook in %s", hooksDir)
}
