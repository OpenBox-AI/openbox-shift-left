package telemetryemit

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/internal/telemetry"
)

// Emitter is the production caller the mapper was missing.
type Emitter struct {
	Spool  hookflow.Spool
	Mapper *Mapper

	// DID is a resolver, not a value. The hook path never had the problem because
	// every hook is a fresh process.
	DID func() string

	// Warn reports trouble.
	Warn    func(format string, args ...any)
	Flush   func(sessionID string)
	Verbose func(format string, args ...any)

	mu       sync.Mutex
	drops    map[string]int
	emitted  int
	lastWarn time.Time
}

// dropWarnInterval unthrottled, the signal that something is wrong would
// itself be the thing that makes the log unreadable; and this daemon's stdio
// is the only place a silently-not-recording lane can be noticed at all.
const dropWarnInterval = 30 * time.Second

var _ telemetry.Emitter = (*Emitter)(nil)

// Emit maps one record and spools whatever it produced. It returns nil on
// every path, deliberately.
func (e *Emitter) Emit(_ context.Context, rec telemetry.Record) error {
	if e == nil {
		return nil
	}
	ev, outcome := e.Mapper.EventFor(rec)
	if outcome != Emitted {
		e.record(outcome, rec)
		return nil
	}

	if e.DID != nil {
		ev.DeveloperDID = e.DID()
	}
	if ev.DeveloperDID == "" {
		e.record(dropNoDID, rec)
		return nil
	}

	if err := e.Spool.Append(ev); err != nil {
		e.record(dropSpoolFailed, rec)
		e.warnThrottled("openbox telemetry: cannot spool a model-call turn: %v", err)
		return nil
	}

	e.mu.Lock()
	e.emitted++
	e.mu.Unlock()

	if e.Verbose != nil {
		e.Verbose("  telemetry: recorded %s turn %s", rec.EventName, ev.OtelRequestID)
	}
	if e.Flush != nil {
		e.Flush(ev.SessionID)
	}
	return nil
}

const (
	dropNoDID       = Outcome(-1)
	dropSpoolFailed = Outcome(-2)
)

func reasonName(o Outcome) string {
	switch o {
	case dropNoDID:
		return "no-developer-did"
	case dropSpoolFailed:
		return "spool-write-failed"
	}
	return o.String()
}

// record skips are counted for the verbose view but never warned about: most
// records are legitimately uninteresting, and warning on them would train a
// reader to ignore the log.
func (e *Emitter) record(o Outcome, rec telemetry.Record) {
	name := reasonName(o)

	e.mu.Lock()
	if e.drops == nil {
		e.drops = map[string]int{}
	}
	e.drops[name]++
	n := e.drops[name]
	e.mu.Unlock()

	if e.Verbose != nil {
		e.Verbose("  telemetry: %s (%s)", name, rec.EventName)
	}
	if o.IsDrop() || o < 0 {
		e.warnThrottled("openbox telemetry: dropped a record (%s); %d so far. The lane is receiving but not recording.", name, n)
	}
}

func (e *Emitter) warnThrottled(format string, args ...any) {
	if e.Warn == nil {
		return
	}
	e.mu.Lock()
	if time.Since(e.lastWarn) < dropWarnInterval {
		e.mu.Unlock()
		return
	}
	e.lastWarn = time.Now()
	e.mu.Unlock()
	e.Warn(format, args...)
}

// Stats renders the counters for the doctor's recording line and for shutdown.
func (e *Emitter) Stats() (emitted int, drops map[string]int) {
	if e == nil {
		return 0, nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make(map[string]int, len(e.drops))
	for k, v := range e.drops {
		out[k] = v
	}
	return e.emitted, out
}

// String is the one-line summary for the daemon's log on shutdown.
func (e *Emitter) String() string {
	emitted, drops := e.Stats()
	if len(drops) == 0 {
		return fmt.Sprintf("%d turns recorded", emitted)
	}
	keys := make([]string, 0, len(drops))
	for k := range drops {
		keys = append(keys, k)
	}
	sort.Strings(keys) // stable output: this is read by humans comparing runs
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, drops[k]))
	}
	return fmt.Sprintf("%d turns recorded, other outcomes: %s", emitted, strings.Join(parts, " "))
}
