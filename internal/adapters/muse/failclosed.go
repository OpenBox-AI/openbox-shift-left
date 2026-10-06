package muse

import (
	"fmt"
	providerspi "github.com/openbox-ai/openbox-shift-left/internal/provider"
	"io"
)

// RunFailClosed is the body of the deny-only onFailure successor, which Muse
// runs when a gate handler crashes, times out or answers invalidly. It exists
// because those failures are otherwise an allow. It reads no config, no
// identity and no network, and writes nothing under the OpenBox home: a gate
// that failed may well be failing because one of those is missing, and the
// successor has to work exactly then.
//
// A gated event exits FaultExitCode with one line on stderr: Muse reads exit 2
// as a block on a blocking event, and nothing is written to stdout for it to
// reject. An event that gates nothing exits 0, since a denial there would only
// stop the developer working. An event Muse does not have is refused: the one
// way to reach this with no event is a hand-edited settings file, and an
// unreadable successor must not read as permission.
func RunFailClosed(event string, stderr io.Writer) int {
	h, err := ParseHookName(event)
	if err != nil {
		fmt.Fprintf(stderr, "openbox: gate unavailable and %q is not a Muse hook event; refusing\n", event)
		return FaultExitCode
	}
	if !h.Gated() {
		return 0
	}
	fmt.Fprintf(stderr, "openbox: gate unavailable for %s; denied (fail-closed)\n", h)
	return FaultExitCode
}

// RunFailClosed is the engine's provider.FailClosedRunner.
func (Engine) RunFailClosed(event string, stderr io.Writer) int { return RunFailClosed(event, stderr) }

var _ providerspi.FailClosedRunner = Engine{}
