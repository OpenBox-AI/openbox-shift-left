package claudecode

import (
	"context"
	"fmt"
	"io"
	"log"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
)

// RewakeExitCode is what Claude Code watches for on an `asyncRewake` handler:
// exit 2 wakes the session and shows the process's stderr to the model as a
// system reminder.
const RewakeExitCode = 2

// rewakeHookTimeoutSec bounds the background watcher. It must cover core's
// approval window (30 minutes by default) or the host would kill the watcher
// mid-wait and the decision would land unannounced.
const rewakeHookTimeoutSec = 2700

// RunRewake is the background half of the PreToolUse registration (E9 §2.2 the
// background half). It never writes to stdout: a background handler's stdout
// is not the hook response, and the only channel that reaches the model here
// is stderr on exit 2.
func RunRewake(stdin io.Reader, wake io.Writer, logger *log.Logger) int {
	defer devconfig.Pin()()
	defer func() {
		if r := recover(); r != nil {
			logger.Printf("rewake recovered: %v", r)
		}
	}()

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
	devEv, ok := New(id, DefaultSpoolDir()).Mapper.Map(HookPreToolUse, ev)
	if !ok {
		return 0
	}

	msg, ok := hookflow.AwaitRewake(context.Background(), logger, client.ApprovalKeyFor(devEv), evaluator.NewClient)
	if !ok {
		return 0
	}
	fmt.Fprintln(wake, msg)
	return RewakeExitCode
}
