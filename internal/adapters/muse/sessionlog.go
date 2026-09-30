package muse

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Muse journals each session to an append-only `session.jsonl`, and records an
// action's authorisation as a `side_effect_intent` record before it runs. That
// file is how a tool action that never reached a hook (a payload over the
// hook's size cap skips the hook entirely) can still be seen. Everything here
// is read-only, and only the join fields are ever decoded: a line's content
// (tool inputs) is skipped byte by byte and never copied, logged or kept.
//
// The location and the record shape come from research, not from a Muse
// binary: see testdata/README.md. Nothing here may assume them; a line that
// does not look as expected is a counted decode error, not a guess.

const (
	sessionLogName = "session.jsonl"
	subagentDir    = "subagent"
	intentType     = "side_effect_intent"

	// dateDirLookback is how many days back the session's date directory is
	// searched for; a session older than that is found again through the
	// directory its cursor remembers.
	dateDirLookback = 8
	// maxSubagentLogs bounds how many subagent logs one pass looks at.
	maxSubagentLogs = 32

	// maxLineBytes bounds how much of one line is walked field by field. Lines
	// are scanned in constant memory; the cap bounds time. A longer line is
	// stepped over in bulk and counted, keeping whatever join fields were seen
	// before the cap, and a payload past the hook's own 256 KiB cap is far
	// below it.
	maxLineBytes = 8 << 20
	// deadlineStride is how many bytes of a line are scanned between checks of
	// the pass deadline.
	deadlineStride = 64 << 10
	maxKeyBytes    = 64
	maxFieldLen    = 512
)

// sessionLogRoot is Muse's sessions directory. A seam, so a test points it at a
// scratch directory and never at a real home.
var sessionLogRoot = func() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = os.Getenv("HOME")
	}
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".local", "share", "muse", "sessions")
}

// logFile is one journal of a session: the main one, or a subagent's.
type logFile struct {
	// Key names the log in the cursor: "" for the main log, else
	// "subagent/<id>".
	Key  string
	Path string
	// SubagentID is the directory name of a subagent log, which may also be the
	// session id its hooks carry.
	SubagentID string
}

// safeSessionID reports whether id can name a directory under the sessions
// root: a payload-supplied id must never walk out of it.
func safeSessionID(id string) bool {
	if id == "" || id == "." || id == ".." || len(id) > 256 {
		return false
	}
	return !strings.ContainsAny(id, "/\\\x00")
}

// dateDirs lists the YYYY/MM/DD directories to search, newest first, for both
// the local and the UTC calendar since which one Muse uses is undocumented.
func dateDirs(root string, now time.Time) []string {
	seen := map[string]bool{}
	var out []string
	for d := 0; d < dateDirLookback; d++ {
		for _, t := range []time.Time{now.AddDate(0, 0, -d), now.UTC().AddDate(0, 0, -d)} {
			dir := filepath.Join(root, t.Format("2006"), t.Format("01"), t.Format("02"))
			if !seen[dir] {
				seen[dir] = true
				out = append(out, dir)
			}
		}
	}
	return out
}

// locateSessionLogs finds a session's logs: the main one and each subagent's.
// known is the session directory a previous pass resolved, tried first. The
// second result is the session directory found, "" when there is no log.
func locateSessionLogs(root, sessionID, known string, now time.Time) ([]logFile, string) {
	if root == "" || !safeSessionID(sessionID) {
		return nil, ""
	}
	candidates := make([]string, 0, 1+2*dateDirLookback)
	if known != "" {
		candidates = append(candidates, known)
	}
	for _, d := range dateDirs(root, now) {
		candidates = append(candidates, filepath.Join(d, sessionID))
	}
	for _, dir := range candidates {
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			continue
		}
		var logs []logFile
		if main := filepath.Join(dir, sessionLogName); isRegular(main) {
			logs = append(logs, logFile{Path: main})
		}
		entries, _ := os.ReadDir(filepath.Join(dir, subagentDir))
		sort.Slice(entries, func(a, b int) bool { return entries[a].Name() < entries[b].Name() })
		for _, e := range entries {
			if len(logs) > maxSubagentLogs {
				break
			}
			if !e.IsDir() || !safeSessionID(e.Name()) {
				continue
			}
			if p := filepath.Join(dir, subagentDir, e.Name(), sessionLogName); isRegular(p) {
				logs = append(logs, logFile{Key: subagentDir + "/" + e.Name(), Path: p, SubagentID: e.Name()})
			}
		}
		if len(logs) > 0 {
			return logs, dir
		}
	}
	return nil, ""
}

func isRegular(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// fileCursor is how far one log has been read and which file that was: a
// replaced file (another inode) or a shorter one restarts from the top.
type fileCursor struct {
	Offset int64  `json:"offset"`
	Inode  uint64 `json:"inode,omitempty"`
}

// intent is the join view of one side_effect_intent record.
type intent struct {
	ToolName  string
	ToolUseID string
	Seq       string
	At        time.Time
}

// readOutcome is what one bounded pass over a log produced.
type readOutcome struct {
	Intents []intent
	// Cursor is where the next pass starts: just past the last line consumed.
	Cursor fileCursor
	// BytesRead counts what the scanner consumed, including a line that was not
	// committed (a partial last line, or an intent not yet old enough).
	BytesRead int64
	// Lines counts the committed lines.
	Lines int
	// Oversize counts lines over maxLineBytes that were stepped over.
	Oversize int
	// Rotated is set when the file was replaced or shortened and read afresh.
	Rotated bool
	// DecodeErr is set when a line did not look like the expected schema. The
	// pass stopped before it and the cursor stays there.
	DecodeErr error
}

// errSchema is a line that could be read but does not have the join fields.
var errSchema = errors.New("session log line does not match the expected schema")

// readLog reads path from cur, bounded: it stops once maxBytes have been
// consumed (checked between lines, so a line in progress always finishes) or
// the deadline has passed. An intent stamped at or after cutoff is left for a
// later pass, and so is everything after it, so a pass never reconciles an
// action whose hook may still be running.
func readLog(path string, cur fileCursor, cutoff, deadline time.Time, maxBytes int64) (readOutcome, error) {
	out := readOutcome{Cursor: cur}
	f, err := os.Open(path)
	if err != nil {
		return out, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return out, err
	}
	id := fileIdentity(info)
	if (cur.Inode != 0 && id != 0 && cur.Inode != id) || info.Size() < cur.Offset {
		out.Rotated = true
		out.Cursor = fileCursor{}
	}
	out.Cursor.Inode = id
	if _, err := f.Seek(out.Cursor.Offset, io.SeekStart); err != nil {
		return out, err
	}

	br := bufio.NewReaderSize(f, 64<<10)
	for out.BytesRead < maxBytes && time.Now().Before(deadline) {
		sc := &lineScanner{r: br, deadline: deadline}
		rec, st := sc.scan()
		if st == lineTooLong {
			// Past the line cap: the rest is skipped in bulk. The line is
			// committed (a pass that refused it would never get past it), and
			// counted. It still counts as an intent when the fields seen before
			// the cap were enough to be one.
			complete := sc.skipRest()
			out.BytesRead += sc.n
			if !complete {
				break
			}
			out.Oversize++
			if it, ok := rec.intent(); ok && rec.Type == intentType && it.At.Before(cutoff) {
				out.Intents = append(out.Intents, it)
			}
			out.Cursor.Offset += sc.n
			out.Lines++
			continue
		}
		out.BytesRead += sc.n
		if st == lineIncomplete {
			break
		}
		if st == lineBlank {
			out.Cursor.Offset += sc.n
			continue
		}
		if st != lineOK {
			out.DecodeErr = errSchema
			break
		}
		if rec.Type == "" {
			out.DecodeErr = errSchema
			break
		}
		if rec.Type == intentType {
			it, ok := rec.intent()
			if !ok {
				out.DecodeErr = errSchema
				break
			}
			if !it.At.Before(cutoff) {
				break
			}
			out.Intents = append(out.Intents, it)
		}
		out.Cursor.Offset += sc.n
		out.Lines++
	}
	return out, nil
}

// joinRecord holds the only fields of a log line that are ever decoded.
type joinRecord struct {
	Type      string
	ToolUseID string
	ToolName  string
	Timestamp string
	Seq       string
}

func (r joinRecord) intent() (intent, bool) {
	at, err := time.Parse(time.RFC3339Nano, r.Timestamp)
	if err != nil || r.ToolName == "" {
		return intent{}, false
	}
	return intent{ToolName: r.ToolName, ToolUseID: r.ToolUseID, Seq: r.Seq, At: at}, true
}

type lineStatus int

const (
	lineOK lineStatus = iota
	lineBlank
	// lineBad is a complete line that is not a JSON object of the expected
	// shape.
	lineBad
	// lineIncomplete is a line the file ends in the middle of, or a line over
	// maxLineBytes: neither is committed.
	lineIncomplete
	// lineTooLong is a line that went past maxLineBytes before it ended.
	lineTooLong
)

var (
	errSyntax  = errors.New("syntax")
	errEOL     = errors.New("end of line")
	errTooLong = errors.New("line too long")
	errTime    = errors.New("deadline")
)

// lineScanner walks one line of a JSON-lines file in constant memory. It keeps
// a value only when the key is a join field and the value a short string or
// number; every other value, however large, is stepped over byte by byte.
type lineScanner struct {
	r *bufio.Reader
	// n counts the bytes consumed, including the newline.
	n int64
	// deadline, when set, is checked every deadlineStride bytes.
	deadline time.Time
}

func (s *lineScanner) next() (byte, error) {
	b, err := s.r.ReadByte()
	if err != nil {
		return 0, err // io.EOF: the line is unfinished
	}
	s.n++
	if s.n > maxLineBytes {
		return 0, errTooLong
	}
	if s.n%deadlineStride == 0 && !s.deadline.IsZero() && time.Now().After(s.deadline) {
		return 0, errTime
	}
	if b == '\n' {
		return 0, errEOL
	}
	return b, nil
}

func (s *lineScanner) unread() {
	if s.r.UnreadByte() == nil {
		s.n--
	}
}

func isSpace(b byte) bool { return b == ' ' || b == '\t' || b == '\r' }

func (s *lineScanner) skipSpace() (byte, error) {
	for {
		b, err := s.next()
		if err != nil || !isSpace(b) {
			return b, err
		}
	}
}

// scan reads one line and reports what it held.
func (s *lineScanner) scan() (joinRecord, lineStatus) {
	var rec joinRecord
	b, err := s.skipSpace()
	switch {
	case errors.Is(err, errEOL):
		return rec, lineBlank
	case err == nil && b != '{':
		err = errSyntax
	case err == nil:
		err = s.members(&rec)
	}
	if err == nil {
		err = s.endOfLine()
	}
	switch {
	case err == nil:
		return rec, lineOK
	case errors.Is(err, errEOL):
		return rec, lineBad // the newline was consumed inside a value
	case errors.Is(err, errSyntax):
		if s.drain() {
			return rec, lineBad
		}
		return rec, lineIncomplete
	case errors.Is(err, errTooLong):
		return rec, lineTooLong
	default: // io.EOF, errTime
		return rec, lineIncomplete
	}
}

// skipRest steps over the rest of an over-long line in bulk and reports
// whether it ended in a newline; a deadline or the end of the file leaves it
// uncommitted.
func (s *lineScanner) skipRest() bool {
	for {
		chunk, err := s.r.ReadSlice('\n')
		s.n += int64(len(chunk))
		switch {
		case err == nil:
			return true
		case errors.Is(err, bufio.ErrBufferFull):
			if !s.deadline.IsZero() && time.Now().After(s.deadline) {
				return false
			}
		default:
			return false
		}
	}
}

// drain consumes the rest of a malformed line and reports whether it ended in
// a newline.
func (s *lineScanner) drain() bool { return s.skipRest() }

func (s *lineScanner) endOfLine() error {
	for {
		b, err := s.next()
		if errors.Is(err, errEOL) {
			return nil
		}
		if err != nil {
			return err
		}
		if !isSpace(b) {
			return errSyntax
		}
	}
}

// members reads the members of an object whose '{' is already consumed.
func (s *lineScanner) members(rec *joinRecord) error {
	for {
		b, err := s.skipSpace()
		if err != nil {
			return err
		}
		if b == '}' {
			return nil
		}
		if b != '"' {
			return errSyntax
		}
		key, _, err := s.str(maxKeyBytes)
		if err != nil {
			return err
		}
		if b, err = s.skipSpace(); err != nil {
			return err
		} else if b != ':' {
			return errSyntax
		}
		if b, err = s.skipSpace(); err != nil {
			return err
		}
		var dst *string
		switch key {
		case "type":
			dst = &rec.Type
		case "tool_use_id":
			dst = &rec.ToolUseID
		case "tool_name":
			dst = &rec.ToolName
		case "timestamp":
			dst = &rec.Timestamp
		case "seq":
			dst = &rec.Seq
		}
		switch {
		case dst != nil && b == '"' && key != "seq":
			val, truncated, serr := s.str(maxFieldLen)
			if serr != nil {
				return serr
			}
			if !truncated {
				*dst = val
			}
		case dst != nil && key == "seq" && b != '"' && b != '{' && b != '[':
			tok, serr := s.literal(b, maxFieldLen)
			if serr != nil {
				return serr
			}
			*dst = tok
		default:
			if err := s.skipValue(b); err != nil {
				return err
			}
		}
		if b, err = s.skipSpace(); err != nil {
			return err
		}
		switch b {
		case ',':
		case '}':
			return nil
		default:
			return errSyntax
		}
	}
}

// str reads a string whose opening quote is consumed, keeping at most limit
// bytes. truncated reports a string that was longer, whose kept part is then
// not a value anyone should use.
func (s *lineScanner) str(limit int) (val string, truncated bool, err error) {
	var buf []byte
	escaped := false
	for {
		b, err := s.next()
		if err != nil {
			return "", false, err
		}
		if b == '"' {
			break
		}
		keep := func(c byte) {
			if len(buf) < limit {
				buf = append(buf, c)
			} else {
				truncated = true
			}
		}
		keep(b)
		if b == '\\' {
			escaped = true
			c, err := s.next()
			if err != nil {
				return "", false, err
			}
			keep(c)
			if c == 'u' {
				for i := 0; i < 4; i++ {
					h, err := s.next()
					if err != nil {
						return "", false, err
					}
					keep(h)
				}
			}
		}
	}
	if truncated {
		return "", true, nil
	}
	if escaped {
		var un string
		if json.Unmarshal(append(append([]byte{'"'}, buf...), '"'), &un) != nil {
			return "", false, errSyntax
		}
		return un, false, nil
	}
	return string(buf), false, nil
}

// literal reads a number or keyword whose first byte is consumed, stopping
// before its delimiter.
func (s *lineScanner) literal(first byte, limit int) (string, error) {
	if !(first == '-' || (first >= '0' && first <= '9') || first == 't' || first == 'f' || first == 'n') {
		return "", errSyntax
	}
	buf := []byte{first}
	for {
		b, err := s.next()
		if err != nil {
			return "", err
		}
		if isSpace(b) || b == ',' || b == '}' || b == ']' {
			s.unread()
			return string(buf), nil
		}
		if len(buf) >= limit {
			return "", errSyntax
		}
		buf = append(buf, b)
	}
}

// skipValue steps over one JSON value whose first byte is consumed.
func (s *lineScanner) skipValue(first byte) error {
	switch first {
	case '"':
		return s.skipString()
	case '{', '[':
		depth := 1
		for depth > 0 {
			b, err := s.next()
			if err != nil {
				return err
			}
			switch b {
			case '"':
				if err := s.skipString(); err != nil {
					return err
				}
			case '{', '[':
				depth++
			case '}', ']':
				depth--
			}
		}
		return nil
	}
	_, err := s.literal(first, maxFieldLen)
	return err
}

func (s *lineScanner) skipString() error {
	for {
		b, err := s.next()
		if err != nil {
			return err
		}
		switch b {
		case '"':
			return nil
		case '\\':
			if _, err := s.next(); err != nil {
				return err
			}
		}
	}
}
