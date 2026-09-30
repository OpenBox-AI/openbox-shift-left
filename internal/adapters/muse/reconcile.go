package muse

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/flock"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/internal/trace"
)

// Gap detection, not prevention. A tool call Muse ran with no gate record is
// usually one whose hook payload was over the hook's 256 KiB cap: Muse skips
// the hook entirely, so neither the gate nor its fail-closed successor ran.
// Nothing can refuse such a call. What can be done is notice it: at Stop and
// SessionEnd the session's own journal is read for tool authorisations, and
// each one without a gate record becomes a content-free trace finding.
//
// The reconciler never blocks, latches or alters delivery, and never
// egresses: a finding is a local trace line, counted by doctor.

const (
	// passBytes bounds what one pass reads of a session's logs.
	passBytes = 4 << 20

	// Hook time budgets, each well under the handler's own ceiling (5s for
	// Stop, 3s for SessionEnd, whose delivery window must stay intact).
	stopReconcileBudget       = 1500 * time.Millisecond
	sessionEndReconcileBudget = 500 * time.Millisecond

	reasonNoGateRecord = "no_gate_record"

	// evidenceWindow is how far back doctor counts findings.
	evidenceWindow = 7 * 24 * time.Hour

	// stateRetention is how long a session's cursor and gate ledger outlive
	// its last write before a pass sweeps them.
	stateRetention = 14 * 24 * time.Hour
	maxSweep       = 200
)

// Outcomes of a reconcile pass, on the evidence.reconcile stage.
const (
	reconcileOK       = "ok"
	reconcileDisabled = "disabled"
	reconcileError    = "error"
)

// reconcileDir is where a session's cursor and gate ledger live, under the
// adapter's own spool directory, so uninstall's spool sweep removes them.
func reconcileDir(spoolDir string) string { return filepath.Join(spoolDir, "reconcile") }

// stateStem names a session's files: a readable prefix plus a hash, so two ids
// that sanitize alike never share a file.
func stateStem(sessionID string) string {
	var b strings.Builder
	for _, r := range sessionID {
		if b.Len() >= 48 {
			break
		}
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	sum := sha256.Sum256([]byte(sessionID))
	return b.String() + "-" + hex.EncodeToString(sum[:4])
}

// cursorState is everything a session's passes remember.
type cursorState struct {
	// Dir is the session directory the logs were found in.
	Dir   string                `json:"dir,omitempty"`
	Files map[string]fileCursor `json:"files"`
	// Ordinals counts, per tool name, the intents carrying no tool_use_id that
	// were already reconciled: the next one is that tool's next ordinal.
	Ordinals map[string]int `json:"ordinals"`
	// Claimed counts, per tool name, the gate records already matched exactly
	// by an intent's tool_use_id, so the ordinal join does not count them twice.
	Claimed map[string]int `json:"claimed"`
}

func loadCursor(path string) (cursorState, error) {
	st := newCursorState()
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		return newCursorState(), fmt.Errorf("cursor unreadable: %w", err)
	}
	if st.Files == nil {
		st.Files = map[string]fileCursor{}
	}
	if st.Ordinals == nil {
		st.Ordinals = map[string]int{}
	}
	if st.Claimed == nil {
		st.Claimed = map[string]int{}
	}
	return st, nil
}

func newCursorState() cursorState {
	return cursorState{Files: map[string]fileCursor{}, Ordinals: map[string]int{}, Claimed: map[string]int{}}
}

func saveCursor(path string, st cursorState) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return hookflow.AtomicWriteFile(path, raw, 0o600)
}

// gateEntry is one line of the gate ledger: a tool call the gate was asked
// about. Content-free: a name and an id.
type gateEntry struct {
	ToolName  string `json:"tool_name"`
	ToolUseID string `json:"tool_use_id,omitempty"`
}

// ledgerLock keeps two goroutines of one process from interleaving; appends
// across processes rely on O_APPEND with one write per line.
var ledgerLock sync.Mutex

// RecordGateCall notes that the gate was asked about one tool call of a
// session. It is the ledger a reconcile pass joins a session's own journal
// against: written by the hook before it evaluates, so a call whose hook
// started is on it whatever the verdict, and a call whose hook never started
// (an oversize payload) is not. Best effort; an error is returned for the
// caller to log and never to act on.
func RecordGateCall(spoolDir, sessionID, toolName, toolUseID string) (err error) {
	// A gated hook answers a call; nothing in a bookkeeping write may turn into
	// a fault exit that denies it.
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("gate ledger: recovered: %v", p)
		}
	}()
	if spoolDir == "" || !safeSessionID(sessionID) {
		return nil
	}
	line, err := json.Marshal(gateEntry{ToolName: toolName, ToolUseID: toolUseID})
	if err != nil {
		return err
	}
	path := filepath.Join(reconcileDir(spoolDir), stateStem(sessionID)+".gates")
	ledgerLock.Lock()
	defer ledgerLock.Unlock()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if errors.Is(err, os.ErrNotExist) {
		// Only the first call of a spool pays for the directory.
		if err = os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
		f, err = os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	}
	if err != nil {
		return err
	}
	_, werr := f.Write(append(line, '\n'))
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	return werr
}

// gateSet is what the ledgers of a session (and its subagents) hold.
type gateSet struct {
	ids    map[string]bool
	byTool map[string]int
}

func loadGates(spoolDir string, sessionIDs ...string) gateSet {
	g := gateSet{ids: map[string]bool{}, byTool: map[string]int{}}
	for _, sid := range sessionIDs {
		if !safeSessionID(sid) {
			continue
		}
		f, err := os.Open(filepath.Join(reconcileDir(spoolDir), stateStem(sid)+".gates"))
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 4<<10), 64<<10)
		for sc.Scan() {
			var e gateEntry
			if json.Unmarshal(sc.Bytes(), &e) != nil {
				continue
			}
			g.byTool[e.ToolName]++
			if e.ToolUseID != "" {
				g.ids[e.ToolUseID] = true
			}
		}
		f.Close()
	}
	return g
}

// Reconciler joins a session's journal against its gate ledger. Every path is a
// field so a test never touches a real home.
type Reconciler struct {
	// LogRoot is Muse's sessions directory.
	LogRoot string
	// SpoolDir is the adapter's spool directory, home of the cursor and ledger.
	SpoolDir string
	// Now stamps the date-directory search; time.Now when nil.
	Now func() time.Time
	// Emit writes a trace finding; trace.Emit when nil.
	Emit func(trace.Record)
	// MaxBytes bounds the read of one pass; passBytes when zero.
	MaxBytes int64
}

// Result is one pass, in counts only.
type Result struct {
	// NoLog is set when the session has no journal to read.
	NoLog bool
	// Skipped is set when another pass of the same session held the lock.
	Skipped bool
	// Intents is how many tool authorisations this pass reconciled.
	Intents int
	// Gaps is how many of them had no gate record.
	Gaps int
	// Errors counts what was swallowed: an unreadable file, a cursor that could
	// not be kept.
	Errors int
	// Disabled is set when a line did not match the expected schema, which
	// stops the pass: the journal's format is unverified.
	Disabled  bool
	BytesRead int64
	// Oversize counts lines over the line cap that were stepped over.
	Oversize int
}

// Run reconciles sessionID's logs up to the journal's intents older than
// cutoff, within deadline. runID stamps the findings. It returns counts only,
// swallows its errors and never panics.
func (r Reconciler) Run(sessionID, runID string, cutoff, deadline time.Time) (res Result) {
	defer func() {
		if p := recover(); p != nil {
			res.Errors++
		}
	}()
	if !safeSessionID(sessionID) || r.SpoolDir == "" {
		res.NoLog = true
		return res
	}
	now := time.Now
	if r.Now != nil {
		now = r.Now
	}
	emit := trace.Emit
	if r.Emit != nil {
		emit = r.Emit
	}
	maxBytes := r.MaxBytes
	if maxBytes <= 0 {
		maxBytes = passBytes
	}

	cursorPath := filepath.Join(reconcileDir(r.SpoolDir), stateStem(sessionID)+".cursor")
	st, err := loadCursor(cursorPath)
	if err != nil {
		res.Errors++
	}
	logs, dir := locateSessionLogs(r.LogRoot, sessionID, st.Dir, now())
	if len(logs) == 0 {
		res.NoLog = true
		return res
	}

	// One pass per session at a time: two concurrent ones would each read the
	// same cursor and lose the other's offset and ordinal updates. A pass that
	// finds it held has nothing to add; the holder does its work.
	if err := os.MkdirAll(reconcileDir(r.SpoolDir), 0o700); err != nil {
		res.Errors++
		return res
	}
	lock := flock.New(filepath.Join(reconcileDir(r.SpoolDir), stateStem(sessionID)+".lock"))
	switch held, err := lock.TryLock(); {
	case err != nil:
		res.Errors++
		return res
	case !held:
		res.Skipped = true
		return res
	}
	defer func() { _ = lock.Unlock() }()
	// The cursor may have moved while this pass waited to start.
	if st2, err := loadCursor(cursorPath); err == nil {
		st = st2
	}

	ids := []string{sessionID}
	for _, l := range logs {
		if l.SubagentID != "" {
			ids = append(ids, l.SubagentID)
		}
	}
	gates := loadGates(r.SpoolDir, ids...)

	dirty := dir != st.Dir
	st.Dir = dir
	var findings []trace.Record
	for _, l := range logs {
		out, err := readLog(l.Path, st.Files[l.Key], cutoff, deadline, maxBytes-res.BytesRead)
		res.BytesRead += out.BytesRead
		res.Oversize += out.Oversize
		if err != nil {
			res.Errors++
			continue
		}
		for _, it := range out.Intents {
			res.Intents++
			if gapOf(it, gates, &st) {
				res.Gaps++
				findings = append(findings, gapRecord(sessionID, runID, it))
			}
		}
		if out.Cursor != st.Files[l.Key] {
			st.Files[l.Key] = out.Cursor
			dirty = true
		}
		if out.DecodeErr != nil {
			res.Disabled = true
			break
		}
		if res.BytesRead >= maxBytes {
			break
		}
	}
	// The cursor is kept BEFORE a finding is written: a pass that dies between
	// the two under-counts, which the next pass cannot repeat; the other order
	// reports the same call again. A cursor that cannot be kept reports
	// nothing this time, and the next pass finds the same intents again.
	if dirty {
		if err := saveCursor(cursorPath, st); err != nil {
			res.Errors++
			res.Intents, res.Gaps, findings = 0, 0, nil
		}
	}
	for _, f := range findings {
		emit(f)
	}
	if res.Intents > 0 || res.Errors > 0 || res.Disabled || res.Oversize > 0 {
		emit(reconcileRecord(sessionID, runID, res))
	}
	sweepState(r.SpoolDir, now())
	return res
}

// gapOf joins one intent. With a tool_use_id the join is exact; without one it
// is the intent's ordinal among this session's id-less intents of the same tool
// against the gate records of that tool no exact join has claimed, so the pass
// finds how many calls went ungated and not always which of them.
func gapOf(it intent, gates gateSet, st *cursorState) bool {
	if it.ToolUseID != "" {
		if gates.ids[it.ToolUseID] {
			st.Claimed[it.ToolName]++
			return false
		}
		return true
	}
	k := st.Ordinals[it.ToolName]
	st.Ordinals[it.ToolName] = k + 1
	return k >= gates.byTool[it.ToolName]-st.Claimed[it.ToolName]
}

func gapRecord(sessionID, runID string, it intent) trace.Record {
	detail := map[string]any{
		"provider":   provider,
		"session_id": sessionID,
		"run_id":     runID,
		"tool_name":  capStr(it.ToolName),
		"reason":     reasonNoGateRecord,
	}
	if it.ToolUseID != "" {
		detail["tool_use_id"] = capStr(it.ToolUseID)
	}
	return trace.Record{
		Provider:  provider,
		SessionID: sessionID,
		RunID:     runID,
		Stage:     trace.StageEvidenceGap,
		Outcome:   reasonNoGateRecord,
		Detail:    detail,
	}
}

func reconcileRecord(sessionID, runID string, res Result) trace.Record {
	outcome := reconcileOK
	switch {
	case res.Disabled:
		outcome = reconcileDisabled
	case res.Errors > 0:
		outcome = reconcileError
	}
	return trace.Record{
		Provider:  provider,
		SessionID: sessionID,
		RunID:     runID,
		Stage:     trace.StageEvidenceReconcile,
		Outcome:   outcome,
		Detail: map[string]any{
			"intents":  res.Intents,
			"gaps":     res.Gaps,
			"errors":   res.Errors,
			"oversize": res.Oversize,
		},
	}
}

// sweepState removes cursors and ledgers nothing has written for a while, a
// bounded number per pass, so state for ended sessions does not pile up.
func sweepState(spoolDir string, now time.Time) {
	entries, err := os.ReadDir(reconcileDir(spoolDir))
	if err != nil {
		return
	}
	// The bound is on removals, not on entries looked at, so a run of fresh
	// files at the front of the listing cannot keep the old ones behind them
	// from ever being reached.
	removed := 0
	for _, e := range entries {
		if removed >= maxSweep {
			return
		}
		info, err := e.Info()
		if err != nil || e.IsDir() || now.Sub(info.ModTime()) < stateRetention {
			continue
		}
		if os.Remove(filepath.Join(reconcileDir(spoolDir), e.Name())) == nil {
			removed++
		}
	}
}

// EvidenceSummary is what doctor shows of the reconciler.
type EvidenceSummary struct {
	// Gaps counts evidence.gap findings inside the window.
	Gaps int
	// Unverified is set when some session's latest pass disabled itself.
	Unverified bool
}

// SummarizeEvidence counts Muse's evidence.gap findings in the trace at dir
// since the start of the window ending at now. A missing trace directory is an
// empty summary, not an error.
func SummarizeEvidence(traceDir string, now time.Time) (EvidenceSummary, error) {
	var sum EvidenceSummary
	if traceDir == "" {
		return sum, nil
	}
	since := now.Add(-evidenceWindow)
	prefilter := func(line []byte) bool { return bytes.Contains(line, []byte(`"stage":"evidence.`)) }
	recs, _, err := trace.ReadFiltered(traceDir, prefilter, func(r trace.Record) bool {
		return r.Provider == provider && r.TS.After(since) &&
			(r.Stage == trace.StageEvidenceGap || r.Stage == trace.StageEvidenceReconcile)
	})
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return sum, nil
		}
		return sum, err
	}
	latest := map[string]string{} // session -> outcome of its latest pass; recs are time-ordered
	for _, r := range recs {
		switch r.Stage {
		case trace.StageEvidenceGap:
			sum.Gaps++
		case trace.StageEvidenceReconcile:
			latest[r.SessionID] = r.Outcome
		}
	}
	for _, o := range latest {
		if o == reconcileDisabled {
			sum.Unverified = true
		}
	}
	return sum, nil
}

// reconcileOnHook runs one pass for a Stop or SessionEnd hook: the cutoff is
// the hook's own instant, the deadline the hook's start plus its budget.
func reconcileOnHook(hook HookName, sessionID, runID string, hookStart, now time.Time, logger *log.Logger) {
	budget := stopReconcileBudget
	if hook == HookSessionEnd {
		budget = sessionEndReconcileBudget
	}
	res := Reconciler{LogRoot: sessionLogRoot(), SpoolDir: DefaultSpoolDir()}.Run(sessionID, runID, now, hookStart.Add(budget))
	if res.Errors > 0 || res.Disabled || res.Gaps > 0 {
		// Counts only: a line of the journal is never logged.
		logger.Printf("session log reconcile for %s: %d intent(s), %d ungated, %d error(s), disabled=%v",
			sessionID, res.Intents, res.Gaps, res.Errors, res.Disabled)
	}
}
