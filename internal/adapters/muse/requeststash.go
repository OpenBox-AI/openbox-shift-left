package muse

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
)

// Two small cross-process stashes under the Muse spool, both written by the
// hook process and read by someone else:
//
//   - requests: response_id -> the redacted request body, written when a
//     PostLLMCall hook fires and taken by the telemetry daemon when the matching
//     model-call record arrives. It holds content, so it is written only when
//     content capture is on, lives 10 minutes, and is deleted when taken.
//   - turn index: (session, turn_id) -> the response ids of that turn's model
//     calls, appended by each PostLLMCall and taken by Stop, which has no other
//     way to learn them (the journal carries no turn id). It holds ids only, so
//     it is written whether or not content capture is on.
//
// Files are 0600 under 0700 directories, written atomically (requests) or by
// a single O_APPEND write (the index), and every sweep is bounded.

const (
	stashDirName     = ".openbox"
	requestsDirName  = "requests"
	turnIndexDirName = "turnresponses"

	// requestTTL is how long a stashed request body may wait for its model-call
	// record.
	requestTTL = 10 * time.Minute
	// turnIndexTTL outlives any turn: an entry older than this belongs to a
	// session that never reached Stop.
	turnIndexTTL = 24 * time.Hour
	// maxStashSweep bounds how many directory entries one sweep examines.
	maxStashSweep = 256
)

// ErrUnjoinableResponseID is returned for a response id that cannot be joined
// on: empty, or the echo provider's constant.
var ErrUnjoinableResponseID = errors.New("muse: response id cannot be joined on")

// stashNow is a seam for the clock.
var stashNow = time.Now

// RequestEntry is one stashed request body and where it came from.
type RequestEntry struct {
	SessionID      string    `json:"session_id"`
	ChildSessionID string    `json:"child_session_id,omitempty"`
	TurnID         string    `json:"turn_id,omitempty"`
	Body           string    `json:"body"`
	WrittenAt      time.Time `json:"written_at"`
}

func stashHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:16])
}

func requestsDir(spoolDir string) string {
	return filepath.Join(spoolDir, stashDirName, requestsDirName)
}

func joinableResponseID(id string) error {
	if id == "" || id == EchoResponseID {
		return ErrUnjoinableResponseID
	}
	return nil
}

// PutRequest stashes e.Body under responseID. It writes nothing, and returns
// nil, when capture is off or the body is empty. It stamps WrittenAt and sweeps
// expired entries first.
func PutRequest(spoolDir, responseID string, e RequestEntry, capture bool) error {
	if err := joinableResponseID(responseID); err != nil {
		return err
	}
	if !capture || e.Body == "" || spoolDir == "" {
		return nil
	}
	dir := requestsDir(spoolDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	sweepDir(dir, requestTTL)
	e.WrittenAt = stashNow()
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	return hookflow.AtomicWriteFile(filepath.Join(dir, stashHash(responseID)), data, 0o600)
}

// TakeRequest reads and deletes the entry for responseID. An expired or
// unreadable entry is deleted and reported as not found.
func TakeRequest(spoolDir, responseID string) (RequestEntry, bool) {
	if spoolDir == "" || joinableResponseID(responseID) != nil {
		return RequestEntry{}, false
	}
	path := filepath.Join(requestsDir(spoolDir), stashHash(responseID))
	data, err := os.ReadFile(path)
	if err != nil {
		return RequestEntry{}, false
	}
	os.Remove(path)
	var e RequestEntry
	if json.Unmarshal(data, &e) != nil || e.Body == "" || stashNow().Sub(e.WrittenAt) > requestTTL {
		return RequestEntry{}, false
	}
	return e, true
}

func turnIndexDir(spoolDir, sessionID string) string {
	return filepath.Join(spoolDir, stashDirName, turnIndexDirName, stashHash(sessionID))
}

// AppendTurnResponse records that responseID belongs to the turn. Entries keep
// their append order.
func AppendTurnResponse(spoolDir, sessionID, turnID, responseID string) error {
	if err := joinableResponseID(responseID); err != nil {
		return err
	}
	if spoolDir == "" || sessionID == "" || turnID == "" {
		return errors.New("muse: turn index needs a spool, session and turn id")
	}
	dir := turnIndexDir(spoolDir, sessionID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	sweepDir(dir, turnIndexTTL)
	f, err := os.OpenFile(filepath.Join(dir, stashHash(turnID)), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.WriteString(responseID + "\n")
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	return werr
}

// TakeTurnResponses reads and deletes the response ids of a turn, in append
// order and without repeats. It returns nil when there is none.
func TakeTurnResponses(spoolDir, sessionID, turnID string) []string {
	if spoolDir == "" || sessionID == "" || turnID == "" {
		return nil
	}
	dir := turnIndexDir(spoolDir, sessionID)
	path := filepath.Join(dir, stashHash(turnID))
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var ids []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 4096), maxFieldLen*2)
	for sc.Scan() {
		if id := strings.TrimSpace(sc.Text()); id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	f.Close()
	os.Remove(path)
	os.Remove(dir) // only succeeds once empty
	return ids
}

// sweepDir removes regular files in dir last modified more than ttl ago,
// examining at most maxStashSweep entries.
func sweepDir(dir string, ttl time.Duration) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := stashNow().Add(-ttl)
	for i, e := range entries {
		if i >= maxStashSweep {
			return
		}
		if e.IsDir() {
			continue
		}
		if info, err := e.Info(); err == nil && info.ModTime().Before(cutoff) {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}
