package hookflow

import (
	"io"
	"log"

	"github.com/openbox-ai/openbox-shift-left/internal/client"
)

// Mapper is the observe half every adapter provides: it maps one native hook
// payload to a normalized event, or reports false when the payload maps to
// nothing (missing session id, bad DID, an event with no contract type).
type Mapper[H ~string, E any] interface {
	Map(hook H, e *E) (client.DevEvent, bool)
}

// Adapter is one tool's realization of the Provider Adapter Contract: its
// Mapper over the shared Engine. Each adapter package names its instantiation
// (`type Adapter = hookflow.Adapter[HookName, HookEvent, Mapper]`).
type Adapter[H ~string, E any, M Mapper[H, E]] struct {
	Mapper M
	*Engine
}

// NewAdapter builds an Adapter spooling under spoolDir and writing Advisory
// records to the default sink.
func NewAdapter[H ~string, E any, M Mapper[H, E]](m M, spoolDir string) *Adapter[H, E, M] {
	return &Adapter[H, E, M]{Mapper: m, Engine: NewEngine(spoolDir)}
}

// Observe is the hot path: map one hook payload to a normalized event and hand
// it to the engine, which threads the tool call's duration and spools it. A
// payload that maps to nothing is silently dropped.
func (a *Adapter[H, E, M]) Observe(hook H, e *E) (spooled bool, err error) {
	ev, ok := a.Mapper.Map(hook, e)
	if !ok {
		return false, nil
	}
	if err := a.Record(ev); err != nil {
		return false, err
	}
	return true, nil
}

// HaltReplayer is how a halted run answers one gated call, shared by the
// pre-gate check (a run already halted before this call started) and the gate
// itself (EnforceGate.OnHalted, for a halt the call's OWN drain discovers):
// spool the call's observe copy, then replay the latch with no server round
// trip -- the latch is the decided state. triple picks the contract and the
// tool name/kind labels the replay renders and records under.
func (a *Adapter[H, E, M]) HaltReplayer(logger *log.Logger, stdout io.Writer, hook H, e *E, sessionID string, nudge func(), triple func() (OutputContract, string, string)) func(SessionHaltInfo) ApplyResult {
	return func(info SessionHaltInfo) ApplyResult {
		if _, err := a.Observe(hook, e); err != nil {
			logger.Printf("spool %s event: %v", hook, err)
		}
		nudge()
		c, toolName, toolKind := triple()
		dec, res := SessionHaltReplay(logger, stdout, info, false, nil, toolName, c)
		RecordEnforcement(logger, sessionID, toolKind, dec, res)
		return res
	}
}
