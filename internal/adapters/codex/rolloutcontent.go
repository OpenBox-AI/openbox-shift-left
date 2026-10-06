package codex

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
)

// This file reads the CONTENT of one model call out of a Codex rollout. It is a
// reader of its own, beside usage.go's numbers-and-summaries reader and not a
// widening of it: that reader's allowlist guarantees it binds no message body,
// and that guarantee stays true. Here the allowlist is
// rolloutContentAllowedFields, and TestRolloutContentAllowlistIsExhaustive fails
// when the rollout grows a key nobody has classified.
//
// Codex 0.156.1 writes, per model call, the call's output items
// (assistant message, reasoning, tool calls) and then a `token_usage_record`
// line carrying the call's own usage and the provider's response id; the
// tool results the call's tools produce, and the next user turn, follow it and
// are the NEXT call's input. So a call is delimited by its token_usage_record:
// its response is the output items since the previous record, its request is
// every item before it except those. The telemetry export carries no response
// id, so the call is found by its token counts and its time.

// ContentState is what a content read settled on. Every state except
// ContentVerified comes with no content at all: a rollout that cannot be read
// whole is not read partly.
type ContentState string

const (
	// ContentVerified: the call was found and its items decoded.
	ContentVerified ContentState = "verified"
	// ContentLogAbsent: no rollout file for the thread (yet).
	ContentLogAbsent ContentState = "log_absent"
	// ContentNoJoin: the rollout reads cleanly but no single record matches the
	// call's counts and time. Never a guess among near matches.
	ContentNoJoin ContentState = "no_join"
	// ContentUnverified: the rollout's shape is not the one this reader was
	// written against.
	ContentUnverified ContentState = "unverified"
	// ContentTimeout: the deadline ended the read.
	ContentTimeout ContentState = "timeout"
)

// CallJoin is what the telemetry record says about the call: when it was
// exported and its token counts. Input includes the cached tokens, as in both
// the export and the rollout.
type CallJoin struct {
	At                       time.Time
	Input, Output, CacheRead int
	// Exclude, when set, names records that are not this call's: a rollout
	// record already given to another call of the thread. A record with the
	// same counts that an earlier call owns must not be taken for this one's
	// while its own is still unwritten.
	Exclude func(responseID string) bool
}

// CallItem is one rollout item, reduced to what a body carries. Type is
// message | reasoning_summary | tool_call | tool_output | omitted | other.
type CallItem struct {
	Type      string `json:"type"`
	Role      string `json:"role,omitempty"`
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Text      string `json:"text,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// CallContent is one call: the provider's response id, the items the call
// produced and the items it was given. Texts are raw (not yet redacted).
type CallContent struct {
	ResponseID string
	Request    []CallItem
	Response   []CallItem
}

const (
	// maxRolloutLineBytes bounds one line. A longer line is not parsed: in the
	// span being read it becomes an `omitted` item.
	maxRolloutLineBytes = 1 << 20
	// rolloutJoinWindow is how far a record's time may sit from the telemetry
	// record's and still be the same call.
	rolloutJoinWindow = 30 * time.Second
	// contentWindowBytes bounds the history kept while scanning: newest items
	// win, which is all a request body keeps.
	contentWindowBytes = 4 * client.MaxModelCallBodyBytes
	// bodyOverhead reserves the {"input":[]} / {"response_id":"…","output":[]}
	// wrapper.
	bodyOverhead = 160
)

// rolloutContentAllowedFields classifies every payload key of the records this
// reader decodes (session_meta, response_item, token_usage_record).
var rolloutContentAllowedFields = map[string]string{
	"type":        "bound: discriminator",
	"id":          "bound on session_meta (thread check); IGNORED on items",
	"role":        "bound: message role",
	"content":     "bound: message text (content-gated, redacted)",
	"summary":     "bound: reasoning summary text (content-gated, redacted)",
	"call_id":     "bound: pairs a tool call with its output",
	"name":        "bound: tool name",
	"arguments":   "bound: function call arguments (content-gated, redacted)",
	"input":       "bound: custom tool call input (content-gated, redacted)",
	"action":      "bound: local shell call action (content-gated, redacted)",
	"output":      "bound: tool output (content-gated, redacted)",
	"response_id": "bound: provider response id",
	"usage":       "bound: the call's own token counts, the join key",

	"encrypted_content":                          "IGNORED: opaque provider blob, never egressed",
	"internal_chat_message_metadata_passthrough": "IGNORED: internal correlation only",
	"status":             "IGNORED: structural",
	"cwd":                "IGNORED: path, not content",
	"originator":         "IGNORED: structural",
	"cli_version":        "IGNORED: structural",
	"base_instructions":  "IGNORED: system prompt, not a Codex content class",
	"model_provider":     "IGNORED: structural",
	"session_id":         "IGNORED: may be the parent's, never the thread check",
	"thread_id":          "IGNORED: structural",
	"turn_id":            "IGNORED: structural",
	"root_turn_id":       "IGNORED: structural",
	"turn_token_usage":   "IGNORED: window delta, not the call",
	"thread_token_usage": "IGNORED: cumulative, not the call",
}

type contentEnvelope struct {
	Type      string          `json:"type"`
	Timestamp string          `json:"timestamp"`
	Payload   json.RawMessage `json:"payload"`
}

type contentUsage struct {
	Input  *int `json:"input_tokens"`
	Cached *int `json:"cached_input_tokens"`
	Output *int `json:"output_tokens"`
}

type contentPayload struct {
	Type       string          `json:"type"`
	ID         string          `json:"id"`
	Role       string          `json:"role"`
	Content    []contentPart   `json:"content"`
	Summary    []contentPart   `json:"summary"`
	CallID     string          `json:"call_id"`
	Name       string          `json:"name"`
	Arguments  json.RawMessage `json:"arguments"`
	Input      json.RawMessage `json:"input"`
	Action     json.RawMessage `json:"action"`
	Output     json.RawMessage `json:"output"`
	ResponseID string          `json:"response_id"`
	Usage      *contentUsage   `json:"usage"`
}

type contentPart struct {
	Text string `json:"text"`
}

// ReadCallContent finds the call in the thread's rollout under sessionsRoot
// and returns its items. The wait is the caller's: this makes one pass (two
// scans of the file) and reports what the file holds now; the caller polls
// for a rollout that is still being written. Content is returned only with
// ContentVerified.
func ReadCallContent(sessionsRoot, threadID string, join CallJoin, deadline time.Time) (CallContent, ContentState) {
	path := rolloutPath(sessionsRoot, threadID)
	if path == "" {
		return CallContent{}, ContentLogAbsent
	}
	target, st := findCall(path, threadID, join, deadline)
	if st != ContentVerified {
		return CallContent{}, st
	}
	return collectCall(path, target, deadline)
}

// SessionsRoot is Codex's sessions directory as this process resolves it:
// <CODEX_HOME>/sessions, beside the config.toml the install already finds.
// `openbox init` resolves it once and writes it into the telemetry unit,
// because the daemon that reads the rollouts has no $HOME of its own.
func SessionsRoot() string { return filepath.Join(filepath.Dir(ConfigTOMLPath()), "sessions") }

// rolloutPath is <root>/YYYY/MM/DD/rollout-*-<thread>.jsonl, or "". The thread
// id is checked as a file-name component before it reaches a glob.
func rolloutPath(root, thread string) string {
	if root == "" || !safeThreadID(thread) {
		return ""
	}
	matches, err := filepath.Glob(filepath.Join(root, "*", "*", "*", "rollout-*-"+thread+".jsonl"))
	if err != nil || len(matches) == 0 {
		return ""
	}
	sort.Strings(matches)
	for i := len(matches) - 1; i >= 0; i-- {
		if fi, err := os.Stat(matches[i]); err == nil && fi.Mode().IsRegular() {
			return matches[i]
		}
	}
	return ""
}

func safeThreadID(s string) bool {
	if s == "" || s == "." || s == ".." || len(s) > 128 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
		default:
			return false
		}
	}
	return true
}

// scanLines calls fn for each line with its 0-based number. over marks a line
// past the cap (line then holds only its head); last marks a final line with
// no newline, which may be a write in progress. fn returns stop to end the scan.
func scanLines(path string, deadline time.Time, fn func(n int, line []byte, over, last bool) (stop bool)) (ContentState, bool) {
	f, err := os.Open(path)
	if err != nil {
		return ContentLogAbsent, false
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 64<<10)
	for n := 0; ; n++ {
		if n%64 == 0 && time.Now().After(deadline) {
			return ContentTimeout, false
		}
		var buf []byte
		over := false
		last := false
		for {
			chunk, err := r.ReadSlice('\n')
			if !over {
				buf = append(buf, chunk...)
				if len(buf) > maxRolloutLineBytes {
					over = true
					buf = buf[:256]
				}
			}
			if errors.Is(err, bufio.ErrBufferFull) {
				continue
			}
			if err != nil {
				if !errors.Is(err, io.EOF) {
					return ContentUnverified, false
				}
				last = true
				if len(chunk) == 0 && len(buf) == 0 {
					return ContentVerified, true // clean end of file
				}
			}
			break
		}
		if fn(n, bytes.TrimRight(buf, "\r\n"), over, last) {
			return ContentVerified, false
		}
		if last {
			return ContentVerified, true
		}
	}
}

type callTarget struct {
	line       int
	responseID string
}

// findCall is the first scan: verify the file's shape and pick the one
// token_usage_record that is this call.
func findCall(path, thread string, join CallJoin, deadline time.Time) (callTarget, ContentState) {
	type cand struct {
		line int
		id   string
		dist time.Duration
	}
	var (
		cands   []cand
		drift   bool
		sawMeta bool
	)
	st, _ := scanLines(path, deadline, func(n int, line []byte, over, last bool) bool {
		if len(bytes.TrimSpace(line)) == 0 {
			return false
		}
		if over {
			if n == 0 {
				drift = true // the head line is session_meta and is small
			}
			return drift
		}
		var env contentEnvelope
		if err := json.Unmarshal(line, &env); err != nil {
			if last {
				return false // a write in progress, not drift
			}
			drift = true
			return true
		}
		if env.Type == "" {
			drift = true
			return true
		}
		if n == 0 {
			var p contentPayload
			if env.Type != "session_meta" || json.Unmarshal(env.Payload, &p) != nil || p.ID != thread {
				drift = true
				return true
			}
			sawMeta = true
			return false
		}
		if env.Type != "token_usage_record" {
			return false
		}
		var p contentPayload
		if json.Unmarshal(env.Payload, &p) != nil || p.Usage == nil ||
			p.Usage.Input == nil || p.Usage.Output == nil || p.Usage.Cached == nil {
			drift = true
			return true
		}
		if *p.Usage.Input != join.Input || *p.Usage.Output != join.Output || *p.Usage.Cached != join.CacheRead {
			return false
		}
		ts, err := time.Parse(time.RFC3339Nano, env.Timestamp)
		if err != nil {
			drift = true
			return true
		}
		d := ts.Sub(join.At)
		if d < 0 {
			d = -d
		}
		if d <= rolloutJoinWindow && (join.Exclude == nil || !join.Exclude(p.ResponseID)) {
			cands = append(cands, cand{n, p.ResponseID, d})
		}
		return false
	})
	if drift {
		return callTarget{}, ContentUnverified
	}
	if st != ContentVerified {
		return callTarget{}, st
	}
	if !sawMeta {
		return callTarget{}, ContentUnverified
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].dist < cands[j].dist })
	switch {
	case len(cands) == 0:
		return callTarget{}, ContentNoJoin
	case len(cands) > 1 && cands[1].dist == cands[0].dist:
		return callTarget{}, ContentNoJoin
	}
	return callTarget{line: cands[0].line, responseID: cands[0].id}, ContentVerified
}

// collectCall is the second scan: items up to the target record, split into
// what came before the call's own output and the output itself.
func collectCall(path string, target callTarget, deadline time.Time) (CallContent, ContentState) {
	var (
		hist, cur []CallItem
		done      bool
		drift     bool
	)
	st, _ := scanLines(path, deadline, func(n int, line []byte, over, last bool) bool {
		if n == 0 || len(bytes.TrimSpace(line)) == 0 {
			return false
		}
		if n >= target.line {
			done = n == target.line
			return true
		}
		if over {
			if bytes.Contains(line, []byte(`"type":"response_item"`)) {
				cur = append(cur, CallItem{Type: "omitted"})
			}
			return false
		}
		var env contentEnvelope
		if err := json.Unmarshal(line, &env); err != nil {
			if last {
				return false
			}
			drift = true
			return true
		}
		switch env.Type {
		case "response_item":
			var p contentPayload
			if json.Unmarshal(env.Payload, &p) != nil || p.Type == "" {
				drift = true
				return true
			}
			cur = append(cur, itemsOf(p)...)
		case "token_usage_record":
			hist = append(hist, cur...)
			cur = nil
			trimWindow(&hist)
		}
		return false
	})
	if drift {
		return CallContent{}, ContentUnverified
	}
	if st != ContentVerified {
		return CallContent{}, st
	}
	if !done {
		return CallContent{}, ContentUnverified // the file changed under the two scans
	}
	var out CallContent
	out.ResponseID = target.responseID
	out.Request = hist
	for _, it := range cur {
		if isOutputItem(it) {
			out.Response = append(out.Response, it)
		} else {
			out.Request = append(out.Request, it)
		}
	}
	return out, ContentVerified
}

// trimWindow drops the oldest items past contentWindowBytes, always keeping
// the newest one.
func trimWindow(items *[]CallItem) {
	total := 0
	for _, it := range *items {
		total += itemBytes(it)
	}
	drop := 0
	for total > contentWindowBytes && drop < len(*items)-1 {
		total -= itemBytes((*items)[drop])
		drop++
	}
	if drop > 0 {
		*items = append([]CallItem(nil), (*items)[drop:]...)
	}
}

func itemBytes(it CallItem) int { return len(it.Text) + len(it.Arguments) + len(it.Name) + 48 }

// isOutputItem: what the model produced. Tool results, user and developer
// messages and anything unrecognised are input.
func isOutputItem(it CallItem) bool {
	switch it.Type {
	case "reasoning_summary", "tool_call":
		return true
	case "message":
		return it.Role == "assistant"
	case "other":
		return strings.HasSuffix(it.Name, "_call")
	}
	return false
}

// itemsOf reduces one response_item to zero or more items. A type this reader
// has no shape for becomes a name-only `other` item: present in the
// conversation, without a body it was never written to read.
func itemsOf(p contentPayload) []CallItem {
	switch p.Type {
	case "message":
		var parts []string
		for _, c := range p.Content {
			if c.Text != "" {
				parts = append(parts, c.Text)
			}
		}
		return []CallItem{{Type: "message", Role: p.Role, Text: capText(strings.Join(parts, "\n"))}}
	case "reasoning":
		var out []CallItem
		for _, s := range p.Summary {
			if s.Text != "" {
				out = append(out, CallItem{Type: "reasoning_summary", Text: capText(s.Text)})
			}
		}
		return out
	case "function_call", "custom_tool_call", "local_shell_call":
		args := flexText(p.Arguments)
		if args == "" {
			args = flexText(p.Input)
		}
		if args == "" {
			args = flexText(p.Action)
		}
		return []CallItem{{Type: "tool_call", CallID: p.CallID, Name: p.Name, Arguments: capText(args)}}
	case "function_call_output", "custom_tool_call_output", "local_shell_call_output":
		return []CallItem{{Type: "tool_output", CallID: p.CallID, Text: capText(flexText(p.Output))}}
	}
	return []CallItem{{Type: "other", Name: p.Type}}
}

// capText keeps the newest hookflow.MaxRedactBody bytes of a text, the bound
// the redactor is sized for.
func capText(s string) string {
	if len(s) <= hookflow.MaxRedactBody {
		return s
	}
	return strings.ToValidUTF8(s[len(s)-hookflow.MaxRedactBody:], "")
}

// flexText reads a field that is a string in one record and a list of text
// parts or an object in another.
func flexText(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return ""
	}
	switch raw[0] {
	case '"':
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s
		}
	case '[':
		var parts []json.RawMessage
		if json.Unmarshal(raw, &parts) == nil {
			var out []string
			for _, p := range parts {
				var obj struct {
					Text string `json:"text"`
				}
				var s string
				switch {
				case json.Unmarshal(p, &s) == nil:
					out = append(out, s)
				case json.Unmarshal(p, &obj) == nil && obj.Text != "":
					out = append(out, obj.Text)
				}
			}
			return strings.Join(out, "\n")
		}
	}
	return string(raw)
}

// ---------------------------------------------------------------------------
// Bodies.

// RequestBodyJSON renders the call's input items as {"input":[...]}, redacted
// (nil leaves text as is) and bounded to client.MaxModelCallBodyBytes. Items
// are kept newest first until the budget is spent and emitted in their
// original order, so the body is always valid JSON and always holds the latest
// items; an item that alone exceeds the budget is kept as its tail. "" when
// there is nothing to say.
func RequestBodyJSON(items []CallItem, redact func(string) string) string {
	if len(items) == 0 {
		return ""
	}
	items = redactItems(items, redact)
	budget := client.MaxModelCallBodyBytes - bodyOverhead
	kept, spent := 0, 0
	for i := len(items) - 1; i >= 0; i-- {
		n := marshalSize(items[i]) + 1
		if spent+n > budget {
			break
		}
		spent += n
		kept++
	}
	if kept == 0 {
		squeezed, ok := squeezeItem(items[len(items)-1], budget)
		if !ok {
			return ""
		}
		return docJSON(map[string]any{"input": []CallItem{squeezed}})
	}
	return docJSON(map[string]any{"input": items[len(items)-kept:]})
}

// ResponseBodyJSON renders the call's output items as
// {"response_id":"…","output":[...]} (the shape Muse's reply uses), redacted
// and bounded; when over budget the longest texts are cut to their tails.
func ResponseBodyJSON(responseID string, items []CallItem, redact func(string) string) string {
	if len(items) == 0 {
		return ""
	}
	items = redactItems(items, redact)
	budget := client.MaxModelCallBodyBytes
	build := func() string {
		return docJSON(map[string]any{"response_id": responseID, "output": items})
	}
	for range 64 {
		body := build()
		if body == "" || len(body) <= budget {
			return body
		}
		// Cut the longest text (or arguments) by half, tail kept.
		idx, longest, args := -1, 0, false
		for i, it := range items {
			if len(it.Text) > longest {
				idx, longest, args = i, len(it.Text), false
			}
			if len(it.Arguments) > longest {
				idx, longest, args = i, len(it.Arguments), true
			}
		}
		if idx < 0 || longest <= 1 {
			return ""
		}
		cp := append([]CallItem(nil), items...)
		if args {
			cp[idx].Arguments = tailString(cp[idx].Arguments, longest/2)
		} else {
			cp[idx].Text = tailString(cp[idx].Text, longest/2)
		}
		items = cp
	}
	return ""
}

func redactItems(items []CallItem, redact func(string) string) []CallItem {
	out := make([]CallItem, len(items))
	copy(out, items)
	if redact == nil {
		return out
	}
	for i := range out {
		if out[i].Text != "" {
			out[i].Text = redact(out[i].Text)
		}
		if out[i].Arguments != "" {
			out[i].Arguments = redact(out[i].Arguments)
		}
	}
	return out
}

func squeezeItem(it CallItem, budget int) (CallItem, bool) {
	for range 64 {
		if marshalSize(it) <= budget {
			return it, true
		}
		switch {
		case len(it.Text) >= len(it.Arguments) && len(it.Text) > 1:
			it.Text = tailString(it.Text, len(it.Text)/2)
		case len(it.Arguments) > 1:
			it.Arguments = tailString(it.Arguments, len(it.Arguments)/2)
		default:
			return CallItem{}, false
		}
	}
	return CallItem{}, false
}

func tailString(s string, n int) string {
	if n >= len(s) {
		return s
	}
	return strings.ToValidUTF8(s[len(s)-n:], "")
}

func marshalSize(v any) int { return len(docJSON(v)) }

// docJSON is json.Marshal without HTML escaping, so a body reads as the
// conversation did. "" on a marshal error.
func docJSON(v any) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return ""
	}
	return strings.TrimRight(buf.String(), "\n")
}
