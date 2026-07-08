// Command openbox-cc-hook is the observe-only entrypoint the OpenBox Claude Code
// plugin wires every hook to (STORY-SL-4). It reads a hook payload on stdin,
// maps it to a normalized OpenBox event, and spools it locally; SessionEnd (and
// the `flush` subcommand) drain the spool to OpenBox off the hot path.
//
// SAFETY CONTRACT (INV-3 / D7 — observe-only, never block):
//   - It ALWAYS exits 0. A non-zero exit (or exit 2) would block/deny the tool
//     call; this binary must never do that.
//   - It writes NOTHING to stdout. On SessionStart/UserPromptSubmit, stdout from
//     an exit-0 hook is injected into the model's context — so all diagnostics
//     go to stderr only, and even those are terse and secret-free (INV-1).
//   - Any failure (bad payload, missing identity, unreachable OpenBox) is logged
//     to stderr and swallowed; the tool call proceeds unaffected.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	claudecode "github.com/openbox-ai/openbox-shift-left/adapters/claude-code"
)

// flushBudget bounds SessionEnd/flush delivery so session teardown is never held
// up (kept under the SessionEnd hook timeout in plugin/hooks.json).
const flushBudget = 12 * time.Second

func main() {
	// Guarantee exit 0 even on an unexpected panic — never block a tool call.
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "openbox-cc-hook: recovered: %v\n", r)
		}
		os.Exit(0)
	}()

	logger := log.New(os.Stderr, "openbox-cc-hook: ", 0)

	if len(os.Args) < 2 {
		logger.Printf("usage: openbox-cc-hook <HookName|flush>")
		return
	}
	sub := os.Args[1]

	if sub == "flush" {
		runFlush(logger, "")
		return
	}

	hook, err := claudecode.ParseHookName(sub)
	if err != nil {
		logger.Printf("%v", err)
		return
	}

	// Hot path: parse + spool. Only the DID is resolved here (no secret I/O).
	id, err := claudecode.ResolveIdentity()
	if err != nil {
		logger.Printf("no identity, dropping %s event: %v", hook, err)
		return
	}
	ev, err := claudecode.ParseHookEvent(os.Stdin)
	if err != nil {
		logger.Printf("dropping %s event: %v", hook, err)
		return
	}

	ad := claudecode.New(id, claudecode.DefaultSpoolDir())
	if _, err := ad.Observe(hook, ev); err != nil {
		logger.Printf("spool %s event: %v", hook, err)
		// fall through — SessionEnd still tries to flush what is already spooled
	}

	// SessionEnd delivers the session's spooled events off the hot path.
	if hook == claudecode.HookSessionEnd {
		runFlush(logger, ev.SessionID)
	}
}

// runFlush drains the spool through the AIP-signed client. A missing/invalid
// full credential set leaves events spooled for a later `flush` (fail-open).
// sessionID=="" flushes every spooled session.
func runFlush(logger *log.Logger, sessionID string) {
	creds, err := claudecode.ResolveCredentials()
	if err != nil {
		logger.Printf("flush skipped (events remain spooled): %v", err)
		return
	}
	cl, err := creds.NewClient(logger)
	if err != nil {
		logger.Printf("flush skipped (client init): %v", err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), flushBudget)
	defer cancel()

	ad := claudecode.New(creds.Identity(), claudecode.DefaultSpoolDir())
	var n int
	if sessionID == "" {
		n, err = ad.FlushAll(ctx, cl)
	} else {
		n, err = ad.Flush(ctx, sessionID, cl)
	}
	if err != nil {
		logger.Printf("flush ended early after %d event(s): %v", n, err)
	}
}
