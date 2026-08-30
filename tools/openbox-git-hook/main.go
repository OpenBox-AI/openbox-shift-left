// Command openbox-git-hook is the legacy standalone entrypoint the OpenBox
// `prepare-commit-msg` hook used to shell out to (STORY-SL-5). As of
// STORY-SL4-WIRE-2 (OD17) the git hook is folded into the unified engine as
// `openbox hook git <sub>`; this binary is reduced to a THIN ALIAS over
// git.RunHook and kept only for backward compatibility. Both share exactly one
// engine, so the fail-open safety contract lives in one place (see RunHook).
//
// SAFETY CONTRACT (the git analog of SL-4's INV-3): always exits 0 — a non-zero
// prepare-commit-msg ABORTS the developer's commit — writes nothing to stdout,
// and swallows every failure (incl. panics). Installs via this alias bake back a
// hook that re-invokes `openbox-git-hook prepare-commit-msg`.
package main

import (
	"log"
	"os"

	obgit "github.com/openbox-ai/openbox-shift-left/internal/adapters/common/git"
)

func main() {
	// observe-only: exit 0 no matter what — even a double-panic unwinds through
	// this deferred os.Exit(0) rather than aborting the commit.
	defer os.Exit(0)
	logger := log.New(os.Stderr, "openbox-git-hook: ", 0)
	obgit.RunHook(os.Args[1:], []string{"prepare-commit-msg"}, logger.Printf)
}
