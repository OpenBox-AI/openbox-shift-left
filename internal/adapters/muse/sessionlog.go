package muse

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Muse journals each session to an append-only `session.jsonl`. Before it runs a
// tool batch it records a `tool_batch.effect.started` envelope naming the call
// (`payload.record.call_id`, which is the hook's own tool_use_id) and the tool.
// That file is how a tool action that never reached a hook (a payload over the
// hook's size cap skips the hook entirely) can still be seen. Everything here
// is read-only, and only the join fields are ever decoded: a line's content
// (prompts, tool arguments, model messages) is skipped byte by byte and never
// copied, logged or kept.
//
// The format was observed on Muse 1.4.1 (testdata/README.md). Each line is one
// of: a frame header or retained-marker object with no `record_type`, which is
// skipped; or an envelope
//
//	{"schema_version":1, "stream":{..}, "recorded_at":<unix microseconds>,
//	 "record_type":"event", "payload_type":"<dotted name>",
//	 "payload":{"kind":"..", "record":{"kind":"started","call_id":"..","tool_name":".."}}}
//
// Unknown payload types and kinds are normal and skipped. The reader gives up
// (the pass is "unverified", see readOutcome.DecodeErr) only when the format
// looks changed: an envelope without a payload_type, an envelope whose
// schema_version is not 1, a started record missing its tool name or time, or
// minProbeLines or more lines in a pass of which none is an envelope at all.
// It stops BEFORE such a line and stays there, so every later pass is
// unverified too until the adapter learns the new format.
//
// A complete line that is not a JSON object at all is a torn or corrupt write
// (a crash mid-line, then an append), not a new format: the pass stops AFTER it,
// counts it and is unverified, and the next pass goes on. Staying before it
// would blind the reconciler for the rest of that session over one line.

const (
	sessionLogName = "session.jsonl"
	subagentDir    = "subagent"

	payloadStarted = "tool_batch.effect.started"
	kindToolBatch  = "tool_batch_effect"
	kindStarted    = "started"
	schemaVersion  = "1"

	// minProbeLines is how many committed lines a pass needs before finding no
	// envelope among them means the format changed rather than that the log has
	// only just begun with its frame header.
	minProbeLines = 8

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

// intent is the join view of one tool_batch.effect.started record.
type intent struct {
	ToolName string
	// ToolUseID is record.call_id, which equals the hook's tool_use_id. Empty
	// when the record carried none, which leaves the ordinal join.
	ToolUseID string
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
	// Envelopes counts the committed lines that were record envelopes.
	Envelopes int
	// Oversize counts lines over maxLineBytes that were stepped over.
	Oversize int
	// Unjoinable counts oversize lines that may be tool intents whose join
	// fields (payload type, call id, tool name) were not seen before the cap.
	Unjoinable int
	// Rotated is set when the file was replaced or shortened and read afresh.
	Rotated bool
	// DecodeErr is set when the log does not look like the observed format (see
	// the top of this file). For a line outside the schema the pass stopped
	// before it and the cursor stays there; for a corrupt line (Corrupt) and for
	// a pass with no envelope at all, it is past them.
	DecodeErr error
	// Corrupt counts complete lines that were not a JSON object, stepped over.
	Corrupt int
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
		if out.BytesRead > 0 {
			// A line must not carry the pass past its byte budget: it is left
			// for the next pass, which starts on it. The first line of a pass
			// has no such limit, so a line longer than the budget still gets
			// read once (within the deadline) rather than never.
			sc.budget = maxBytes - out.BytesRead
		}
		rec, st := sc.scan()
		if st == lineTooLong {
			// Past the line cap: the rest is skipped in bulk. The line is
			// committed (a pass that refused it would never get past it), and
			// counted. It still counts as an intent when the fields seen before
			// the cap were enough to be one. A line that may be an intent but
			// whose join fields lie past the cap is counted as Unjoinable: an
			// action was journaled that nothing can be joined to, and a payload
			// that large is exactly the case this reader exists for.
			complete := sc.skipRest()
			out.BytesRead += sc.n
			if !complete {
				break
			}
			it, isIntent, ok := rec.intent()
			unjoinable := rec.PayloadType == "" || isIntent && !ok
			if (isIntent && ok || unjoinable) && !rec.recordedAt().IsZero() && !rec.recordedAt().Before(cutoff) {
				// Not old enough: left, uncommitted, for a later pass.
				break
			}
			out.Oversize++
			if rec.envelope() {
				out.Envelopes++
			}
			switch {
			case isIntent && ok:
				out.Intents = append(out.Intents, it)
			case unjoinable:
				out.Unjoinable++
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
		if st == lineBad {
			out.DecodeErr = errSchema
			out.Corrupt++
			out.Cursor.Offset += sc.n
			out.Lines++
			break
		}
		if st != lineOK {
			out.DecodeErr = errSchema
			break
		}
		if rec.RecordType != "" {
			// An envelope. A frame header or retained marker has no record_type
			// and is skipped.
			if rec.PayloadType == "" || (rec.SchemaVersion != "" && rec.SchemaVersion != schemaVersion) {
				out.DecodeErr = errSchema
				break
			}
			out.Envelopes++
			if it, isIntent, ok := rec.intent(); isIntent {
				if !ok {
					out.DecodeErr = errSchema
					break
				}
				if !it.At.Before(cutoff) {
					break
				}
				out.Intents = append(out.Intents, it)
			}
		}
		out.Cursor.Offset += sc.n
		out.Lines++
	}
	if out.DecodeErr == nil && out.Lines >= minProbeLines && out.Envelopes == 0 {
		out.DecodeErr = errSchema
	}
	return out, nil
}

// joinRecord holds the only fields of a log line that are ever decoded.
type joinRecord struct {
	SchemaVersion string
	RecordType    string
	PayloadType   string
	// RecordedAt is unix microseconds, as written.
	RecordedAt  string
	PayloadKind string
	RecKind     string
	CallID      string
	ToolName    string
}

// envelope reports whether the line carried the two envelope type fields.
func (r joinRecord) envelope() bool { return r.RecordType != "" && r.PayloadType != "" }

// recordedAt is the line's own time, zero when not seen or not usable.
func (r joinRecord) recordedAt() time.Time {
	us, err := strconv.ParseInt(r.RecordedAt, 10, 64)
	if err != nil || us <= 0 {
		return time.Time{}
	}
	return time.UnixMicro(us)
}

// intent reads the record as a tool-execution intent. isIntent is false for any
// other line. ok is false when it is an intent the join cannot use, which means
// the format is not the observed one.
func (r joinRecord) intent() (it intent, isIntent, ok bool) {
	if r.PayloadType != payloadStarted {
		return intent{}, false, true
	}
	us, err := strconv.ParseInt(r.RecordedAt, 10, 64)
	if err != nil || us <= 0 || r.ToolName == "" || r.PayloadKind != kindToolBatch || r.RecKind != kindStarted {
		return intent{}, true, false
	}
	return intent{ToolName: r.ToolName, ToolUseID: r.CallID, At: time.UnixMicro(us)}, true, true
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
	// budget, when positive, is how many bytes this line may consume before it
	// is left uncommitted; checked with the deadline.
	budget int64
}

// expired reports whether the deadline or the line's byte budget is spent.
func (s *lineScanner) expired() bool {
	if s.budget > 0 && s.n >= s.budget {
		return true
	}
	return !s.deadline.IsZero() && time.Now().After(s.deadline)
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
	if s.n%deadlineStride == 0 && s.expired() {
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
		err = s.members(&rec, pathRoot)
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
// whether it ended in a newline; a deadline, the line's byte budget or the end
// of the file leaves it uncommitted.
func (s *lineScanner) skipRest() bool {
	for {
		chunk, err := s.r.ReadSlice('\n')
		s.n += int64(len(chunk))
		switch {
		case err == nil:
			return true
		case errors.Is(err, bufio.ErrBufferFull):
			if s.expired() {
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

// Objects the scanner descends into; every other object is skipped whole.
const (
	pathRoot    = ""
	pathPayload = "payload"
	pathRecord  = "payload.record"
)

// target says what to do with one member: keep a short string or number in dst,
// descend into an object at child, or (both zero) skip it.
type target struct {
	dst   *string
	child string
}

func (r *joinRecord) target(path, key string) target {
	switch path {
	case pathRoot:
		switch key {
		case "schema_version":
			return target{dst: &r.SchemaVersion}
		case "record_type":
			return target{dst: &r.RecordType}
		case "payload_type":
			return target{dst: &r.PayloadType}
		case "recorded_at":
			return target{dst: &r.RecordedAt}
		case "payload":
			return target{child: pathPayload}
		}
	case pathPayload:
		switch key {
		case "kind":
			return target{dst: &r.PayloadKind}
		case "record":
			return target{child: pathRecord}
		}
	case pathRecord:
		switch key {
		case "kind":
			return target{dst: &r.RecKind}
		case "call_id":
			return target{dst: &r.CallID}
		case "tool_name":
			return target{dst: &r.ToolName}
		}
	}
	return target{}
}

// members reads the members of an object whose '{' is already consumed, at
// path. Only the join fields are kept; everything else is stepped over.
func (s *lineScanner) members(rec *joinRecord, path string) error {
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
		tg := rec.target(path, key)
		switch {
		case tg.child != "" && b == '{':
			if err := s.members(rec, tg.child); err != nil {
				return err
			}
		case tg.dst != nil && b == '"':
			val, truncated, serr := s.str(maxFieldLen)
			if serr != nil {
				return serr
			}
			if !truncated {
				*tg.dst = val
			}
		case tg.dst != nil && b != '{' && b != '[':
			tok, serr := s.literal(b, maxFieldLen)
			if serr != nil {
				return serr
			}
			*tg.dst = tok
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
