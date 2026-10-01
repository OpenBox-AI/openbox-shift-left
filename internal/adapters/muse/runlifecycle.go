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
//     its SubagentStart does, and there is no parent session id in any payload
//     (subagentparent.go reads the parent off its journal and folds the child
//     into the parent's session; only an unlinked child reaches here as its own).
//
// Core must never receive a run's events without that run's WorkflowStarted
// first, so the adapter keeps one small record per session (runState) saying
// which run the session is on and whether SessionEnd or SubagentStop sealed it.
// The first event of a session with no record opens a run at its current run
// (generation 0 unless a resume already continued it). A sealed session is
// reopened only by a UserPromptSubmit, the one event `muse resume` can start
// with: it continues as a new run (continue-as-new, the same Bump a Claude Code
// SessionStart source=resume does). Every other event of a sealed session is a
// straggler: an observed one is dropped, a gated one is evaluated under the
// sealed run and opens nothing. A run's SessionStarted is spooled BEFORE the
// event that opened it. A new run id is also what leaves a resumed session
// unlatched, because the halt latch is keyed by run.

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

const (
	// lifecycleRetention is how long a record outlives its last write before a
	// sweep removes it. A sealed record is what tells a resumed session from a
	// new one, so it must outlast any plausible gap between `muse` sessions;
	// past it the session is treated as unseen. 30 days is twice the
	// reconciler's state retention, and the cost of an over-long keep is one
	// small file per session or subagent.
	lifecycleRetention = 30 * 24 * time.Hour
	// lifecycleSweepEvery rate-limits the directory walk: a skill-reminder
	// subagent per turn would otherwise pay for it on every one.
	lifecycleSweepEvery = time.Hour
	// maxLifecycleSweep bounds the removals of one pass.
	maxLifecycleSweep = 200

	lifecycleSweepMarker = ".swept"
)

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
	if sealed {
		l.sweep()
	}
}

// sweep removes records (the sessions' and the subagent links') nothing has
// written for lifecycleRetention, at most once per lifecycleSweepEvery and a
// bounded number per pass, so state for ended sessions does not pile up.
// Callers hold lock; a removal races only with a record 30 days stale.
func (l lifecycle) sweep() {
	now := l.now()
	marker := filepath.Join(l.Dir, lifecycleSweepMarker)
	if info, err := os.Stat(marker); err == nil && now.Sub(info.ModTime()) < lifecycleSweepEvery {
		return
	}
	if err := os.MkdirAll(l.Dir, 0o700); err != nil {
		return
	}
	if err := hookflow.AtomicWriteFile(marker, nil, 0o600); err != nil {
		l.logf("run lifecycle: sweep marker not written, skipping: %v", err)
		return
	}
	_ = os.Chtimes(marker, now, now)
	removed := 0
	for _, dir := range []string{l.Dir, filepath.Join(l.Dir, subagentLinkDir)} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		// The bound is on removals, not entries looked at, so fresh files at the
		// front of the listing cannot keep the old ones behind them from going.
		for _, e := range entries {
			if removed >= maxLifecycleSweep {
				return
			}
			if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
				continue
			}
			info, err := e.Info()
			if err != nil || now.Sub(info.ModTime()) < lifecycleRetention {
				continue
			}
			if os.Remove(filepath.Join(dir, e.Name())) == nil {
				removed++
			}
		}
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
	// A folded subagent event is one more event of its parent's run: its
	// SubagentStart is not that run's start and its SubagentStop is not its end.
	folded := ev.folded()
	ends := hook.ends() && !folded
	childStart := hook == HookSubagentStart && !folded

	switch {
	case folded && hook == HookSubagentStop:
		// A subagent ending reports nothing inside its parent's session, and
		// must never reopen a parent run that has already ended. It is the one
		// per-child hook that always runs, so it also trims stale records.
		l.sweep()
		return l.current(sid), transition{Drop: true}

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

	case childStart && open:
		// The child's first model call raced ahead of its SubagentStart and
		// already opened its run.
		return l.current(sid), transition{Drop: true}

	case open:
		run := l.current(sid)
		if ends {
			l.save(sid, run, true)
		}
		return run, transition{}

	case have && ends:
		// A second end of a run that already ended: nothing to report, and no
		// reason to resurrect a run just to end it again.
		return l.current(sid), transition{Drop: true}
	}

	// A sealed run is reopened only by what a resumed session starts with: the
	// prompt that resumed it. Anything else of a sealed session is a straggler
	// (a hook racing teardown, a late observer, a folded subagent's event) and
	// would otherwise mint a run nothing ever ends.
	if have && st.Sealed && !(hook == HookUserPromptSubmit && !folded) {
		if hook.Gated() {
			// Still answered, and fail-closed: /evaluate decides under the run
			// the session last had, whose latch (if any) is consulted first. No
			// run is opened, so there is no SessionStarted to spool, and the
			// call neither denies without a verdict nor escapes evaluation.
			return l.current(sid), transition{}
		}
		return l.current(sid), transition{Drop: true}
	}

	// No run, or a sealed one a prompt resumed: open a run (a resume sends no
	// SessionStart of its own).
	resumed := have && st.Sealed
	var run RunIdentity
	if resumed {
		run = l.bump(sid)
	} else {
		run = l.current(sid)
	}
	l.save(sid, run, ends)
	// An unfolded SubagentStart is itself the child's SessionStarted; every
	// other hook needs one synthesised ahead of it.
	return run, transition{Open: !childStart, Resumed: resumed}
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
