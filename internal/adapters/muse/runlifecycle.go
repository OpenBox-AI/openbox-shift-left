package muse

import (
	"context"
	"encoding/json"
	"hash/fnv"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gofrs/flock"

	obgit "github.com/openbox-ai/openbox-shift-left/internal/adapters/common/git"
	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
)

// Muse gives this adapter no reliable lifecycle to hang a run on:
//
//   - `muse resume` fires NO SessionStart. It sends UserPromptSubmit and the
//     rest under the OLD session id, after that session already received its
//     SessionEnd.
//   - A subagent runs under its OWN session id and is announced by
//     SubagentStart/SubagentStop. Its first model call can reach a hook before
//     its SubagentStart does, and there is no parent session id in any payload.
//
// Core must never receive a run's events without that run's WorkflowStarted
// first, so the adapter keeps one small record per session (runState) saying
// which run the session is on and whether SessionEnd or SubagentStop sealed it.
// The first event of a session with no record, or whose run was sealed, opens a
// run: a session never seen starts at its current run (generation 0 unless a
// resume already continued it), a sealed one continues as a new run
// (continue-as-new, the same Bump a Claude Code SessionStart source=resume
// does), and that run's SessionStarted is spooled BEFORE the event itself. A
// new run id is also what leaves a resumed session unlatched, because the halt
// latch is keyed by run.

// runState is one session's lifecycle record.
type runState struct {
	SessionID string `json:"session_id"`
	// RunID is the wire run id the record describes: the session id itself at
	// generation 0, else the minted run id.
	RunID string `json:"run_id"`
	// Sealed reports that SessionEnd (or SubagentStop) closed RunID.
	Sealed    bool  `json:"sealed"`
	UpdatedAt int64 `json:"updated_at"`
}

// transition is what the lifecycle decided for one hook invocation.
type transition struct {
	// Open: spool the run's SessionStarted before anything else of this hook.
	Open bool
	// Resumed: the run being opened continues a sealed one.
	Resumed bool
	// Drop: the event has nothing to add (a second end of an ended run, or a
	// SubagentStart for a child whose first event already opened it).
	Drop bool
}

// ends reports whether a hook closes the run its session is on.
func (h HookName) ends() bool { return h == HookSessionEnd || h == HookSubagentStop }

// lifecycleLockWait bounds the wait for a session's lifecycle lock. It is short
// on purpose: the lock covers a file read, a file write and one spool append.
const lifecycleLockWait = 1500 * time.Millisecond

const lifecycleStripes = 64

type lifecycle struct {
	Dir  string
	Runs obgit.RunStore
	Now  func() time.Time
	Log  *log.Logger
}

func lifecycleDir(spoolDir string) string { return filepath.Join(spoolDir, "lifecycle") }

func (l lifecycle) logf(format string, args ...any) {
	if l.Log != nil {
		l.Log.Printf(format, args...)
	}
}

func (l lifecycle) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

func (l lifecycle) statePath(sessionID string) string {
	var b strings.Builder
	for _, r := range sessionID {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(sessionID))
	return filepath.Join(l.Dir, b.String()+"-"+strconv.FormatUint(uint64(h.Sum32()), 16)+".json")
}

// lock serialises the lifecycle decision of one session across hook processes:
// two hooks of a session that both saw "no run" would each open one. It is a
// striped lock so the lock files never accumulate per session. A filesystem
// that cannot be locked proceeds unlocked with one log line, never blocking the
// hook.
func (l lifecycle) lock(sessionID string) (unlock func()) {
	if err := os.MkdirAll(l.Dir, 0o700); err != nil {
		l.logf("run lifecycle: no lock (%v)", err)
		return func() {}
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(sessionID))
	fl := flock.New(filepath.Join(l.Dir, ".lock."+strconv.Itoa(int(h.Sum32()%lifecycleStripes))))
	ctx, cancel := context.WithTimeout(context.Background(), lifecycleLockWait)
	defer cancel()
	switch ok, err := fl.TryLockContext(ctx, 10*time.Millisecond); {
	case ok:
		return func() { _ = fl.Unlock() }
	case err != nil && ctx.Err() == nil:
		l.logf("run lifecycle: lock unavailable, continuing unlocked: %v", err)
	default:
		l.logf("run lifecycle: lock still held after %s, continuing unlocked", lifecycleLockWait)
	}
	return func() {}
}

func (l lifecycle) load(sessionID string) (runState, bool) {
	raw, err := os.ReadFile(l.statePath(sessionID))
	if err != nil {
		if !os.IsNotExist(err) {
			l.logf("run lifecycle: record unreadable, treating the session as new: %v", err)
		}
		return runState{}, false
	}
	var st runState
	if err := json.Unmarshal(raw, &st); err != nil || st.SessionID != sessionID {
		l.logf("run lifecycle: record corrupt, treating the session as new")
		return runState{}, false
	}
	return st, true
}

// save records the state. A failed write is logged and never blocks the hook;
// the cost is that the next hook of the session may open a run again.
func (l lifecycle) save(sessionID string, run RunIdentity, sealed bool) {
	runID := sessionID
	if run.RunID != "" {
		runID = run.RunID
	}
	data, err := json.Marshal(runState{SessionID: sessionID, RunID: runID, Sealed: sealed, UpdatedAt: l.now().UnixNano()})
	if err == nil {
		err = os.MkdirAll(l.Dir, 0o700)
	}
	if err == nil {
		err = hookflow.AtomicWriteFile(l.statePath(sessionID), data, 0o600)
	}
	if err != nil {
		l.logf("run lifecycle: state not recorded: %v", err)
	}
}

// current is the run a session is on according to the run store: generation 0
// when no record exists. An unreadable record falls back to generation 0 with
// one log line; a run-identity lookup must never block a call or drop an event.
func (l lifecycle) current(sessionID string) RunIdentity {
	rec, err := l.Runs.Read(sessionID)
	if err != nil {
		l.logf("run identity: record unreadable, continuing at generation 0: %v", err)
		return RunIdentity{}
	}
	return RunIdentity{Generation: rec.Generation, RunID: rec.RunID}
}

func (l lifecycle) bump(sessionID string) RunIdentity {
	rec, err := l.Runs.Bump(sessionID)
	if err != nil {
		l.logf("run identity: bump failed, continuing at generation 0: %v", err)
		return RunIdentity{}
	}
	return RunIdentity{Generation: rec.Generation, RunID: rec.RunID, ContinuedFrom: rec.PreviousRunID}
}

// advance decides which run a hook's event belongs to and whether that run has
// to be opened first. Callers hold lock(ev.SessionID).
func (l lifecycle) advance(hook HookName, ev *HookEvent) (RunIdentity, transition) {
	sid := ev.SessionID
	st, have := l.load(sid)
	open := have && !st.Sealed

	switch {
	case hook == HookSessionStart:
		// An explicit start: a resume source continues the session as a new run.
		var run RunIdentity
		if isBumpSource(ev.Source) {
			run = l.bump(sid)
		} else {
			run = l.current(sid)
		}
		l.save(sid, run, false)
		return run, transition{}

	case hook == HookSubagentStart && open:
		// The child's first model call raced ahead of its SubagentStart and
		// already opened its run.
		return l.current(sid), transition{Drop: true}

	case open:
		run := l.current(sid)
		if hook.ends() {
			l.save(sid, run, true)
		}
		return run, transition{}

	case have && hook.ends():
		// A second end of a run that already ended: nothing to report, and no
		// reason to resurrect a run just to end it again.
		return l.current(sid), transition{Drop: true}
	}

	// No run, or a sealed one: open a run. Whatever the hook is, the session
	// is live again (a resume sends no SessionStart of its own).
	resumed := have && st.Sealed
	var run RunIdentity
	if resumed {
		run = l.bump(sid)
	} else {
		run = l.current(sid)
	}
	l.save(sid, run, hook.ends())
	// A SubagentStart is itself the child's SessionStarted; every other hook
	// needs one synthesised ahead of it.
	return run, transition{Open: hook != HookSubagentStart, Resumed: resumed}
}

// openEvent builds the SessionStarted that opens a run no SessionStart
// announced, from the event that found it unopened.
func (m Mapper) openEvent(trigger HookName, e *HookEvent, resumed bool) (client.DevEvent, bool) {
	start := &HookEvent{
		SessionID:      e.SessionID,
		Cwd:            e.Cwd,
		Model:          e.Model,
		PermissionMode: e.PermissionMode,
	}
	if resumed {
		start.Source = "resume"
	}
	ev, ok := m.Map(HookSessionStart, start)
	if !ok {
		return ev, false
	}
	if ev.Metadata == nil {
		ev.Metadata = map[string]any{}
	}
	// trigger is the hook that found the run unopened (the schema's per-type
	// key for what set an event off).
	ev.Metadata["trigger"] = string(trigger)
	if trigger == HookSubagentStop {
		ev.Metadata = mergeMetadata(ev.Metadata, subagentMetadata(e))
	}
	return ev, true
}
