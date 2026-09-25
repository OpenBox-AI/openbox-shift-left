package git

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
)

//   - Sessions in different worktrees never collide; the worktree filter is
//     exact, so each commit resolves to its own repo's session.
//   - Sessions in the same worktree resolve by recency: the committing session
//     refreshed its record on the PreToolUse that fired ms before the commit,
//     so it is the freshest.
const (
	EnvSessionDir = "OPENBOX_SESSION_DIR" // overrides the registry location
	EnvSessionTTL = "OPENBOX_SESSION_TTL" // staleness cutoff, in seconds
)

const defaultSessionTTL = 8 * time.Hour

// SessionRecord is one active developer session's liveness record.
type SessionRecord struct {
	SessionID string `json:"session_id"`
	Cwd       string `json:"cwd"`
	UpdatedAt int64  `json:"updated_at"` // unix nanoseconds (sub-second recency tiebreak)
	// Tool names the adapter that touched this session ("claude-code",
	// "codex", ...). Empty on a record written before this field existed, or
	// on a store this package does not know how to attribute -- a commit
	// event's routing (R2, "agent commits only") treats that the same as any
	// other tool it cannot confirm: no event, never a guess.
	Tool string `json:"tool,omitempty"`
}

// DefaultSessionDir is the shared registry location used by both the adapter
// writer and the hook resolver.
func DefaultSessionDir() string {
	if p := os.Getenv(EnvSessionDir); p != "" {
		return p
	}
	return filepath.Join(devconfig.ConfigDir(), "sessions")
}

// WriteSessionRecord creates or refreshes a session's liveness record (a
// "touch"). The write is atomic (temp + rename) so a concurrent resolver never
// reads a partial file. tool is the adapter doing the writing ("claude-code",
// "codex", ...); it is what a commit event's routing (R2) checks a marker
// against, so a caller must pass its own name rather than "".
func WriteSessionRecord(dir, sessionID, cwd, tool string, now time.Time) error {
	// Invalid → skip silently (best-effort; never blocks a hook).
	if err := ValidateSessionID(sessionID); err != nil {
		return nil
	}
	return writeRecordFile(dir, sessionRecordPath(dir, sessionID),
		SessionRecord{SessionID: sessionID, Cwd: cwd, UpdatedAt: now.UnixNano(), Tool: tool})
}

// RemoveSessionRecord deletes a session's record (the adapter's SessionEnd).
func RemoveSessionRecord(dir, sessionID string) error {
	if sessionID == "" {
		return nil
	}
	if err := os.Remove(sessionRecordPath(dir, sessionID)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func sessionRecordPath(dir, sessionID string) string {
	return filepath.Join(dir, sanitizeForFile(sessionID)+".json")
}

// RunRecord is one session's continue-as-new lineage: unlike SessionRecord it
// outlives SessionEnd, because the generation a `--resume` continues from still
// has to be known after the session that opened it is sealed. Only `--resume`
// reaches this record: a `/clear` mints a new session id, so it has no prior
// run under its own id to continue. Generation 0 (the original run) has no record at all;
// a record only exists from the first bump onward.
type RunRecord struct {
	SessionID     string `json:"session_id"`
	Generation    int    `json:"generation"`
	RunID         string `json:"run_id"`
	PreviousRunID string `json:"previous_run_id,omitempty"`
	UpdatedAt     int64  `json:"updated_at"` // unix nanoseconds; the bump's own clock read
}

// RunDir is where run records live: a SUBDIRECTORY of the session registry,
// never a flat sibling. SessionResolver.resolveFromRegistry (session.go:139)
// scans every *.json directly under DefaultSessionDir() and unmarshals each
// as a SessionRecord; a flat "<id>.run.json" sibling would parse and be
// rejected only by the accident of an empty Cwd failing withinWorktree. A
// subdirectory is skipped structurally by that scan's own e.IsDir() check, so
// the exclusion cannot be undone by a later field addition to RunRecord. Do
// not flatten this back to a sibling file.
func RunDir(sessionDir string) string {
	return filepath.Join(sessionDir, "runs")
}

func runRecordPath(dir, sessionID string) string {
	return filepath.Join(dir, sanitizeForFile(sessionID)+".json")
}

// RunStore reads and bumps one machine's run records. The zero value is the
// production store: Dir resolves to RunDir(DefaultSessionDir()), NewID mints
// a v4 UUID, Now is time.Now. A test sets Dir to a temp directory (and may
// override NewID/Now to pin a value) so it never touches the developer's
// real registry -- the same reason SessionResolver's readDir/readFile/now
// fields exist.
type RunStore struct {
	Dir   string
	NewID func() (string, error)
	Now   func() time.Time
}

func (s RunStore) dir() string {
	if s.Dir != "" {
		return s.Dir
	}
	return RunDir(DefaultSessionDir())
}

// newID never uses uuid.New(): that constructor panics on entropy failure,
// and a panic inside the SessionStart hook is not fail-open (INV-3, R6).
func (s RunStore) newID() (string, error) {
	if s.NewID != nil {
		return s.NewID()
	}
	id, err := uuid.NewRandom()
	if err != nil {
		return "", err
	}
	return id.String(), nil
}

func (s RunStore) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Read returns the current run record for sessionID. err is nil and the
// record is the honest generation-0 zero value when no record exists yet --
// that is the ordinary, overwhelmingly common state, not a fault. err is
// non-nil (and the returned record is the zero value) when the directory is
// unreadable, the file is not valid JSON, or the record is INCONSISTENT: a
// Generation >= 1 record whose RunID fails uuid.Parse, or a Generation == 0
// record with a non-empty RunID. The caller (hookrun.go) treats any non-nil
// err as generation 0 plus one stderr line (R6): a run-identity read must
// never block a tool call or drop an event.
func (s RunStore) Read(sessionID string) (RunRecord, error) {
	data, err := os.ReadFile(runRecordPath(s.dir(), sessionID))
	if err != nil {
		if os.IsNotExist(err) {
			return RunRecord{SessionID: sessionID}, nil
		}
		return RunRecord{}, fmt.Errorf("git: run record for %q unreadable: %w", sessionID, err)
	}
	var rec RunRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return RunRecord{}, fmt.Errorf("git: run record for %q is not valid JSON: %w", sessionID, err)
	}
	if err := validateRunRecord(rec); err != nil {
		return RunRecord{}, err
	}
	return rec, nil
}

func validateRunRecord(rec RunRecord) error {
	if rec.Generation == 0 {
		if rec.RunID != "" {
			return fmt.Errorf("git: run record for %q inconsistent: generation 0 with a non-empty run_id %q", rec.SessionID, rec.RunID)
		}
		return nil
	}
	if _, err := uuid.Parse(rec.RunID); err != nil {
		return fmt.Errorf("git: run record for %q inconsistent: generation %d with an unparseable run_id %q: %w",
			rec.SessionID, rec.Generation, rec.RunID, err)
	}
	return nil
}

// Bump mints a new run for sessionID: generation+1, a fresh UUID run id, and
// PreviousRunID pointing at the run this one continues from. An absent OR
// corrupt/inconsistent old record is treated identically -- starting fresh at
// generation 1 with PreviousRunID = sessionID (V7/R5) -- because a broken old
// record must never propagate into the new one; the only failure Bump itself
// reports is the mint or the write failing, which the caller (hookrun.go)
// treats as generation 0 plus one stderr line (R6): a run-identity write must
// never block a tool call or drop an event.
func (s RunStore) Bump(sessionID string) (RunRecord, error) {
	old, err := s.Read(sessionID)
	if err != nil {
		old = RunRecord{SessionID: sessionID}
	}
	prev := old.RunID
	if prev == "" {
		prev = sessionID
	}
	newID, err := s.newID()
	if err != nil {
		return RunRecord{}, fmt.Errorf("git: minting a new run id for %q: %w", sessionID, err)
	}
	rec := RunRecord{
		SessionID:     sessionID,
		Generation:    old.Generation + 1,
		RunID:         newID,
		PreviousRunID: prev,
		UpdatedAt:     s.now().UnixNano(),
	}
	if err := s.write(rec); err != nil {
		return RunRecord{}, fmt.Errorf("git: writing the bumped run record for %q: %w", sessionID, err)
	}
	return rec, nil
}

func (s RunStore) write(rec RunRecord) error {
	dir := s.dir()
	return writeRecordFile(dir, runRecordPath(dir, rec.SessionID), rec)
}

// writeRecordFile marshals rec and installs it at path through hookflow's
// atomic writer, so a concurrent reader never observes a partial file and a
// crash after the rename cannot leave a zero-length one -- a zeroed run record
// restarts run identity at generation 0. dir is passed rather than derived
// from path, so an empty dir still fails at MkdirAll.
func writeRecordFile(dir, path string, rec any) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	return hookflow.AtomicWriteFile(path, data, 0o600)
}

func sanitizeForFile(id string) string {
	var b strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	if b.Len() == 0 {
		return "unknown"
	}
	return b.String()
}
