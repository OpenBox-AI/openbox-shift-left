package muse

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
)

// This file reads the CONTENT of a Muse model response out of the session
// journal: the assistant's reply, its reasoning summaries and its tool calls.
// It is deliberately separate from sessionlog.go, whose reader is content-blind
// and must stay so (TestJoinReaderStaysContentBlind).
//
// Observed on Muse 1.4.1, every line of interest is an envelope
//
//	{"schema_version":1, ..., "payload_type":"runtime.session",
//	 "payload_schema_version":1, "payload":{"event":{"kind":"<kind>", ...}}}
//
// and the five kinds read here are
//
//	model_response_created          {response_id}
//	reasoning_summary_committed     {response_id, text}
//	assistant_message_committed     {response_id, text}            (the reply)
//	assistant_tool_calls_committed  {response_id, tool_calls[{call_id,name,args}]}
//	model_completed                 {finish_reason?, duration_ms}  (NO response_id)
//
// model_completed is joined by sequence: it belongs to the call whose
// model_response_created most recently preceded it. `output.chunk` is TOOL
// output and `reasoning_committed.text` is empty (encrypted); neither is read.
//
// Each wanted kind is decoded into a typed struct, never a generic map, and
// every other line is dropped without being retained. The result is all or
// nothing: any sign the format changed yields ContentUnverified with no
// content at all.

const (
	// EchoResponseID is the constant response id of Muse's echo provider. It
	// identifies nothing, so it is never stashed and never joined on.
	EchoResponseID = "muse-tui-echo"

	// maxContentLineBytes bounds one line held in memory. A longer line that
	// carries a content kind makes the lookup absent rather than partial.
	maxContentLineBytes = 4 << 20
	// maxParentDirs bounds how many session directories of one day are tried
	// when a session's own directory does not exist (a subagent's log lives
	// under its parent's).
	maxParentDirs = 256

	kindResponseCreated = "model_response_created"
	kindMessage         = "assistant_message_committed"
	kindSummary         = "reasoning_summary_committed"
	kindToolCalls       = "assistant_tool_calls_committed"
	kindCompleted       = "model_completed"

	envelopePrefix = `{"schema_version":`
)

// ContentState is how far a content lookup can be trusted.
type ContentState string

const (
	// ContentVerified: the journal was read in the known format and the
	// requested response was found. The content is complete as of the read.
	ContentVerified ContentState = "verified"
	// ContentAbsent: nothing usable was found (no journal, the response not in
	// it yet, a line past the cap, or the deadline). The content is empty.
	ContentAbsent ContentState = "absent"
	// ContentTimeout: the deadline passed before the journal was read through.
	// The content is empty; reading again may succeed.
	ContentTimeout ContentState = "timeout"
	// ContentUnverified: the journal is not in the format this reader knows.
	// The content is empty; nothing partial is ever returned.
	ContentUnverified ContentState = "unverified"
)

// ToolCall is one tool call a response asked for.
type ToolCall struct {
	CallID string
	Name   string
	// Args is the call's arguments exactly as Muse journaled them (a JSON
	// string). Content: the caller redacts it.
	Args string
}

// ResponseContent is what the journal holds for the requested responses, in
// the order the ids were given. For a single id it is that response exactly.
type ResponseContent struct {
	// Text is the assistant's reply (several replies join with a blank line).
	Text string
	// Summaries are the reasoning summaries, in journal order.
	Summaries []string
	ToolCalls []ToolCall
	// FinishReason is the last non-empty one seen; Muse omits it on some calls.
	FinishReason string
	DurationMS   int64
	// Complete reports that every requested response was found and its
	// model_completed seen. The reply is committed AFTER model_completed, so a
	// caller polling for a reply may still see Text empty when Complete is true.
	Complete bool
}

// ReadResponseContent reads the content of responseIDs from sessionID's
// journal under root (Muse's sessions directory), checking the session's own
// directory first and then the subagent/<sessionID> journal of any recent
// parent. It stops at the deadline (ContentTimeout). Only complete lines are
// read, so a line Muse is still appending is ignored. Callers that poll should
// hold a ContentReader instead, which resumes where the last read stopped.
func ReadResponseContent(root, sessionID string, responseIDs []string, deadline time.Time) (ResponseContent, ContentState) {
	return NewContentReader(root, sessionID, responseIDs).Read(deadline)
}

// ContentReader is ReadResponseContent that remembers how far it has read, so
// a poll scans only what was appended since the previous Read. It is not safe
// for concurrent use. Drift and an oversize content line are sticky: every
// later Read repeats the same state.
type ContentReader struct {
	root, sessionID string
	ids             []string
	acc             *contentAcc
	paths           []string
	files           map[string]*contentFile
	state           ContentState // sticky: ContentUnverified or ContentAbsent from a line past the cap
}

// contentFile is the resume point of one journal.
type contentFile struct {
	offset    int64 // just past the last complete line consumed
	lines     int
	envelopes int
	cur       string // acc.cur at offset
}

// NewContentReader starts a reader for responseIDs; nothing is read until Read.
func NewContentReader(root, sessionID string, responseIDs []string) *ContentReader {
	r := &ContentReader{root: root, sessionID: sessionID, ids: wantedIDs(responseIDs), files: map[string]*contentFile{}}
	r.acc = newContentAcc(r.ids)
	return r
}

// Read scans what is new and returns everything gathered so far. Content comes
// back only with ContentVerified. ContentTimeout means the deadline passed
// mid-read; the next Read carries on from the same place.
func (r *ContentReader) Read(deadline time.Time) (ResponseContent, ContentState) {
	if r.state != "" {
		return ResponseContent{}, r.state
	}
	if len(r.ids) == 0 || r.root == "" || !safeSessionID(r.sessionID) {
		return ResponseContent{}, ContentAbsent
	}
	if len(r.paths) == 0 {
		r.paths = contentLogPaths(r.root, r.sessionID, time.Now())
	}
	// A journal replaced by a shorter one invalidates everything gathered so
	// far: start over rather than mix old content with the new file.
	for path, f := range r.files {
		if info, err := os.Stat(path); err == nil && info.Size() < f.offset {
			r.files = map[string]*contentFile{}
			r.acc = newContentAcc(r.ids)
			break
		}
	}
	for _, path := range r.paths {
		f := r.files[path]
		if f == nil {
			f = &contentFile{}
			r.files[path] = f
		}
		switch scanContentLog(path, f, r.acc, deadline) {
		case scanDrift:
			r.state = ContentUnverified
			return ResponseContent{}, r.state
		case scanOversize:
			r.state = ContentAbsent
			return ResponseContent{}, r.state
		case scanTimeout:
			return ResponseContent{}, ContentTimeout
		}
	}
	if !r.acc.anyFound() {
		return ResponseContent{}, ContentAbsent
	}
	return r.acc.result(), ContentVerified
}

func wantedIDs(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, id := range in {
		if id == "" || id == EchoResponseID || seen[id] || len(id) > maxFieldLen || strings.ContainsAny(id, "\"\\\n") {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// contentLogPaths lists the journals to search: the session's own (main first,
// then its subagents'), else the subagent/<sessionID> journal under a recent
// parent, newest parent first.
func contentLogPaths(root, sessionID string, now time.Time) []string {
	if logs, _ := locateSessionLogs(root, sessionID, "", now); len(logs) > 0 {
		paths := make([]string, 0, len(logs))
		for _, l := range logs {
			paths = append(paths, l.Path)
		}
		return paths
	}
	var paths []string
	tried := 0
	for _, day := range dateDirs(root, now) {
		entries, err := os.ReadDir(day)
		if err != nil {
			continue
		}
		sort.Slice(entries, func(a, b int) bool { return entries[a].Name() > entries[b].Name() })
		for _, e := range entries {
			if tried >= maxParentDirs {
				return paths
			}
			if !e.IsDir() || !safeSessionID(e.Name()) {
				continue
			}
			tried++
			if p := filepath.Join(day, e.Name(), subagentDir, sessionID, sessionLogName); isRegular(p) {
				paths = append(paths, p)
			}
		}
	}
	return paths
}

type scanResult int

const (
	scanDone scanResult = iota
	// scanDrift: the format is not the known one.
	scanDrift
	// scanTimeout: the deadline passed.
	scanTimeout
	// scanOversize: a content line past the cap, which is never read partially.
	scanOversize
)

// contentEnvelope is the typed view of a line carrying a content kind.
type contentEnvelope struct {
	SchemaVersion        *int `json:"schema_version"`
	PayloadSchemaVersion *int `json:"payload_schema_version"`
	Payload              struct {
		Event contentEvent `json:"event"`
	} `json:"payload"`
}

type contentEvent struct {
	Kind         string  `json:"kind"`
	ResponseID   *string `json:"response_id"`
	Text         *string `json:"text"`
	FinishReason *string `json:"finish_reason"`
	DurationMS   *int64  `json:"duration_ms"`
	ToolCalls    []struct {
		CallID *string `json:"call_id"`
		Name   *string `json:"name"`
		Args   *string `json:"args"`
	} `json:"tool_calls"`
}

var contentKinds = []struct {
	kind  string
	probe []byte
}{
	{kindResponseCreated, []byte(`"kind":"` + kindResponseCreated + `"`)},
	{kindMessage, []byte(`"kind":"` + kindMessage + `"`)},
	{kindSummary, []byte(`"kind":"` + kindSummary + `"`)},
	{kindToolCalls, []byte(`"kind":"` + kindToolCalls + `"`)},
	{kindCompleted, []byte(`"kind":"` + kindCompleted + `"`)},
}

func matchContentKind(line []byte) string {
	for _, k := range contentKinds {
		if bytes.Contains(line, k.probe) {
			return k.kind
		}
	}
	return ""
}

func scanContentLog(path string, st *contentFile, acc *contentAcc, deadline time.Time) scanResult {
	f, err := os.Open(path)
	if err != nil {
		return scanDone
	}
	defer f.Close()
	if info, err := f.Stat(); err == nil && info.Size() < st.offset {
		*st = contentFile{} // replaced by a shorter file: start over
	}
	if _, err := f.Seek(st.offset, io.SeekStart); err != nil {
		return scanDone
	}
	br := bufio.NewReaderSize(f, 64<<10)
	acc.cur = st.cur
	var buf []byte
	for {
		if !deadline.IsZero() && time.Now().After(deadline) {
			return scanTimeout
		}
		line, oversize, n, ok := readContentLine(br, &buf)
		if !ok {
			break // end of file; a torn last line stays unconsumed
		}
		st.offset += int64(n)
		st.lines++
		st.cur = acc.cur
		if !bytes.HasPrefix(line, []byte(envelopePrefix)) {
			continue // a frame header or retained marker
		}
		if !bytes.HasPrefix(line, []byte(envelopePrefix+"1,")) {
			return scanDrift
		}
		st.envelopes++
		kind := matchContentKind(line)
		if kind == "" {
			continue
		}
		if oversize {
			if kind != kindResponseCreated && kind != kindCompleted {
				return scanOversize
			}
			continue
		}
		if acc.consume(kind, line) == scanDrift {
			return scanDrift
		}
		st.cur = acc.cur
	}
	if st.lines >= minProbeLines && st.envelopes == 0 {
		return scanDrift
	}
	return scanDone
}

// readContentLine returns the next complete line without its newline. A line
// past maxContentLineBytes comes back oversize with only its leading bytes. ok
// is false at the end of the file, or on a final line with no newline yet. n
// is the bytes the line occupies in the file, newline included.
func readContentLine(br *bufio.Reader, buf *[]byte) (line []byte, oversize bool, n int, ok bool) {
	*buf = (*buf)[:0]
	for {
		chunk, err := br.ReadSlice('\n')
		n += len(chunk)
		if len(*buf) <= maxContentLineBytes {
			*buf = append(*buf, chunk...)
		} else {
			oversize = true
		}
		switch err {
		case nil:
			if len(*buf) > maxContentLineBytes {
				oversize = true
			}
			return bytes.TrimRight(*buf, "\r\n"), oversize, n, true
		case bufio.ErrBufferFull:
			continue
		default: // io.EOF or a read error: the line is unfinished
			return nil, false, 0, false
		}
	}
}

// contentAcc gathers the requested responses while the journals are scanned.
type contentAcc struct {
	ids    []string
	byID   map[string]*respParts
	quoted map[string][]byte
	// cur is the wanted response whose model_response_created most recently
	// preceded the line being read, "" when that call was not asked for.
	cur string
}

type respParts struct {
	found     bool
	completed bool
	budget    int
	texts     []string
	summaries []string
	calls     []ToolCall
	finish    string
	durMS     int64
}

func newContentAcc(ids []string) *contentAcc {
	a := &contentAcc{ids: ids, byID: map[string]*respParts{}, quoted: map[string][]byte{}}
	for _, id := range ids {
		a.byID[id] = &respParts{budget: hookflow.MaxRedactBody}
		a.quoted[id] = []byte(`"` + id + `"`)
	}
	return a
}

func (a *contentAcc) anyFound() bool {
	for _, p := range a.byID {
		if p.found {
			return true
		}
	}
	return false
}

// mentions reports which wanted id a line names, "" when none.
func (a *contentAcc) mentions(line []byte) string {
	for _, id := range a.ids {
		if bytes.Contains(line, a.quoted[id]) {
			return id
		}
	}
	return ""
}

func (a *contentAcc) consume(kind string, line []byte) scanResult {
	switch kind {
	case kindResponseCreated:
		// Every call's marker is read: it moves the sequence cursor.
		ev, ok := decodeKind(line)
		if !ok {
			return scanDrift
		}
		if ev.Kind != kind {
			return scanDone
		}
		if ev.ResponseID == nil || *ev.ResponseID == "" {
			return scanDrift
		}
		a.cur = ""
		if p := a.byID[*ev.ResponseID]; p != nil {
			p.found = true
			a.cur = *ev.ResponseID
		}
	case kindCompleted:
		if a.cur == "" {
			return scanDone
		}
		ev, ok := decodeKind(line)
		if !ok {
			return scanDrift
		}
		if ev.Kind != kind {
			return scanDone
		}
		p := a.byID[a.cur]
		p.completed = true
		if ev.FinishReason != nil && *ev.FinishReason != "" {
			p.finish = *ev.FinishReason
		}
		if ev.DurationMS != nil {
			p.durMS = *ev.DurationMS
		}
	default:
		if a.mentions(line) == "" {
			return scanDone
		}
		ev, ok := decodeKind(line)
		if !ok {
			return scanDrift
		}
		if ev.Kind != kind {
			return scanDone
		}
		if ev.ResponseID == nil || *ev.ResponseID == "" {
			return scanDrift
		}
		p := a.byID[*ev.ResponseID]
		if p == nil {
			return scanDone // the id was only mentioned in passing
		}
		p.found = true
		switch kind {
		case kindMessage, kindSummary:
			if ev.Text == nil {
				return scanDrift
			}
			t := p.take(*ev.Text)
			if t == "" {
				return scanDone
			}
			if kind == kindMessage {
				p.texts = append(p.texts, t)
			} else {
				p.summaries = append(p.summaries, t)
			}
		case kindToolCalls:
			for _, tc := range ev.ToolCalls {
				if tc.CallID == nil || tc.Name == nil || tc.Args == nil {
					return scanDrift
				}
				p.calls = append(p.calls, ToolCall{CallID: *tc.CallID, Name: *tc.Name, Args: p.take(*tc.Args)})
			}
		}
	}
	return scanDone
}

// take spends s against the response's byte budget, cutting on a rune boundary.
func (p *respParts) take(s string) string {
	if p.budget <= 0 {
		return ""
	}
	s = hookflow.TruncateBytes(s, p.budget)
	p.budget -= len(s)
	return s
}

// decodeKind decodes a matched line and checks the format markers. ok is false
// on anything that is not the known shape. The event's Kind may still differ
// from the probe (the probe text sitting inside some other value); the caller
// skips such a line, which is not drift.
func decodeKind(line []byte) (contentEvent, bool) {
	var env contentEnvelope
	if err := json.Unmarshal(line, &env); err != nil {
		return contentEvent{}, false
	}
	if env.SchemaVersion == nil || *env.SchemaVersion != 1 ||
		env.PayloadSchemaVersion == nil || *env.PayloadSchemaVersion != 1 {
		return contentEvent{}, false
	}
	return env.Payload.Event, true
}

func (a *contentAcc) result() ResponseContent {
	var out ResponseContent
	var texts []string
	out.Complete = true
	for _, id := range a.ids {
		p := a.byID[id]
		if !p.found || !p.completed {
			out.Complete = false
		}
		texts = append(texts, p.texts...)
		out.Summaries = append(out.Summaries, p.summaries...)
		out.ToolCalls = append(out.ToolCalls, p.calls...)
		if p.finish != "" {
			out.FinishReason = p.finish
		}
		out.DurationMS += p.durMS
	}
	out.Text = strings.Join(texts, "\n\n")
	return out
}

// responseDoc is the response body Muse's model-call row carries.
type responseDoc struct {
	ResponseID   string       `json:"response_id,omitempty"`
	FinishReason string       `json:"finish_reason,omitempty"`
	Output       []responseOp `json:"output"`
}

type responseOp struct {
	Type      string `json:"type"`
	Text      string `json:"text,omitempty"`
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// ResponseBodyJSON renders c as the response body of one model call:
//
//	{"response_id":"..","finish_reason":"..","output":[
//	  {"type":"reasoning_summary","text":".."},
//	  {"type":"message","text":".."},
//	  {"type":"tool_call","call_id":"..","name":"..","arguments":".."}]}
//
// Every text field goes through redact (nil leaves it as is) BEFORE it is
// marshalled, so a replaced value can never break the JSON. It returns "" when
// c holds no content.
func ResponseBodyJSON(responseID string, c ResponseContent, redact func(string) string) string {
	if redact == nil {
		redact = func(s string) string { return s }
	}
	doc := responseDoc{ResponseID: responseID, FinishReason: c.FinishReason, Output: []responseOp{}}
	for _, s := range c.Summaries {
		doc.Output = append(doc.Output, responseOp{Type: "reasoning_summary", Text: redact(s)})
	}
	if c.Text != "" {
		doc.Output = append(doc.Output, responseOp{Type: "message", Text: redact(c.Text)})
	}
	for _, tc := range c.ToolCalls {
		doc.Output = append(doc.Output, responseOp{Type: "tool_call", CallID: tc.CallID, Name: tc.Name, Arguments: redact(tc.Args)})
	}
	if len(doc.Output) == 0 {
		return ""
	}
	b, err := json.Marshal(doc)
	if err != nil {
		return ""
	}
	return string(b)
}
