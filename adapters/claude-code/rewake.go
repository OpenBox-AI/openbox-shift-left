package claudecode

import (
	"context"
	"fmt"
	"io"
	"log"

	"github.com/openbox-ai/openbox-shift-left/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/client"
)

// RewakeExitCode is what Claude Code watches for on an `asyncRewake` handler:
// exit 2 wakes the session and shows the process's stderr to the model as a
// system reminder. Any other code is silent.
const RewakeExitCode = 2

// rewakeHookTimeoutSec bounds the background watcher. It must cover core's
// approval window (30 minutes by default) or the host would kill the watcher
// mid-wait and the decision would land unannounced. Nothing blocks on it, so a
// long bound costs nothing but an idle process.
const rewakeHookTimeoutSec = 2700

// RunRewake is the background half of the PreToolUse registration (E9 §2.2
// Tier 2). It runs alongside the gate on every tool call, waits to learn
// whether this call filed an approval, and — if one did and it is decided after
// the gate already denied — writes the outcome to `wake` and returns
// RewakeExitCode so Claude Code injects it as a system reminder.
//
// It returns 0 in every other case, which is the overwhelming majority: no
// approval was filed, or the gate's own bounded hold answered it. Being cheap
// in that case is the design constraint — see hookflow's marker protocol.
//
// It never writes to stdout: a background handler's stdout is not the hook
// response, and the only channel that reaches the model here is stderr on
// exit 2.
func RunRewake(stdin io.Reader, wake io.Writer, logger *log.Logger) int {
	defer devconfig.Pin()()
	defer func() {
		if r := recover(); r != nil {
			logger.Printf("rewake recovered: %v", r)
		}
	}()

	// Inert unless the session is actually gating: without enforce, no approval
	// can be filed, so there is nothing to watch for.
	if !ResolveEnforce() {
		return 0
	}
	id, err := ResolveIdentity()
	if err != nil {
		return 0
	}
	ev, err := ParseHookEvent(stdin)
	if err != nil {
		return 0
	}
	// Every gated class evaluates inline since ADR-0017, so any of them can have
	// filed an approval. The high-risk narrowing that used to stand here would
	// now silently drop the watcher for exactly the classes that newly gained
	// approval holds — a call held, denied at the budget, and then never woken.
	devEv, ok := New(id, DefaultSpoolDir()).Mapper.Map(HookPreToolUse, ev)
	if !ok {
		return 0
	}

	msg, ok := hookflow.AwaitRewake(context.Background(), logger, client.ApprovalKeyFor(devEv), tier2.NewClient)
	if !ok {
		return 0
	}
	fmt.Fprintln(wake, msg)
	return RewakeExitCode
}
