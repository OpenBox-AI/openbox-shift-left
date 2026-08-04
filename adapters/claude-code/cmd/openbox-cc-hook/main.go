// Command openbox-cc-hook is the legacy standalone entrypoint the OpenBox Claude
// Code plugin used to wire every hook to (STORY-SL-4). As of STORY-SL4-WIRE-2
// the plugin invokes the unified engine `openbox hook claude-code <event>`
// instead; this binary is reduced to a THIN ALIAS over claudecode.RunHook and
// kept only for backward compatibility. Both share exactly one engine, so the
// observe-only safety contract is defined in one place (see RunHook).
//
// SAFETY CONTRACT (INV-3): always exits 0; all diagnostics go to stderr; RunHook
// swallows every failure (incl. panics). In OBSERVE mode (the default) it writes
// NOTHING to stdout. It shares the one engine, so if OPENBOX_ENFORCE is set it
// honors enforce mode too — a PreToolUse may then emit a Claude Code
// permissionDecision (deny/ask) to stdout (E6-S2), still exiting 0.
package main

import (
	"log"
	"os"

	claudecode "github.com/openbox-ai/openbox-shift-left/adapters/claude-code"
)

func main() {
	// observe-only: exit 0 no matter what — even a panic escaping RunHook's own
	// recover (a double-panic) unwinds through this deferred os.Exit(0) rather
	// than crashing the process with a non-zero (blocking) status.
	defer os.Exit(0)
	logger := log.New(os.Stderr, "openbox-cc-hook: ", 0)
	var sub string
	if len(os.Args) >= 2 {
		sub = os.Args[1]
	}
	claudecode.RunHook(sub, os.Stdin, os.Stdout, logger)
}
