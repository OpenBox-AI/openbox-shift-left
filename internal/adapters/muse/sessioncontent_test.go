package muse

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
)

// contentRoot lays a fixture out as Muse does, under today's date directory.
func contentRoot(t *testing.T, sessionID, fixture string) (root, logPath string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", fixture))
	if err != nil {
		t.Fatal(err)
	}
	return contentRootFrom(t, sessionID, string(data))
}

func contentRootFrom(t *testing.T, sessionID, body string) (root, logPath string) {
	t.Helper()
	root = t.TempDir()
	logPath = filepath.Join(dateDirs(root, time.Now())[0], sessionID, sessionLogName)
	writeLog(t, logPath, body)
	return root, logPath
}

func soon() time.Time { return time.Now().Add(5 * time.Second) }

func TestReadResponseContentToolCallResponse(t *testing.T) {
	root, _ := contentRoot(t, "sess-c1", "session-jsonl-content.jsonl")
	c, st := ReadResponseContent(root, "sess-c1", []string{"resp_a"}, soon())
	if st != ContentVerified {
		t.Fatalf("state = %v, want verified", st)
	}
	if got := strings.Join(c.Summaries, "|"); got != "Neutral summary one.|Neutral summary two." {
		t.Errorf("summaries = %q", got)
	}
	if len(c.ToolCalls) != 1 || c.ToolCalls[0] != (ToolCall{CallID: "call_0001", Name: "bash", Args: `{"command":"ls"}`}) {
		t.Errorf("tool calls = %+v", c.ToolCalls)
	}
	if c.Text != "" {
		t.Errorf("text = %q, want empty for a tool-call response", c.Text)
	}
	if c.FinishReason != "tool_calls" || c.DurationMS != 1500 || !c.Complete {
		t.Errorf("finish/duration/complete = %q %d %v", c.FinishReason, c.DurationMS, c.Complete)
	}
}

func TestReadResponseContentTextResponse(t *testing.T) {
	root, _ := contentRoot(t, "sess-c1", "session-jsonl-content.jsonl")
	c, st := ReadResponseContent(root, "sess-c1", []string{"resp_b"}, soon())
	if st != ContentVerified || c.Text != "Neutral final reply." {
		t.Fatalf("state=%v text=%q", st, c.Text)
	}
	if c.FinishReason != "" || c.DurationMS != 900 || !c.Complete {
		t.Errorf("finish/duration/complete = %q %d %v", c.FinishReason, c.DurationMS, c.Complete)
	}
	if strings.Contains(c.Text, "tool output") || len(c.ToolCalls) != 0 || len(c.Summaries) != 0 {
		t.Errorf("content leaked from another record: %+v", c)
	}
}

func TestReadResponseContentSeveralIDsAggregateInRequestedOrder(t *testing.T) {
	root, _ := contentRoot(t, "sess-c1", "session-jsonl-content.jsonl")
	c, st := ReadResponseContent(root, "sess-c1", []string{"resp_b", "resp_a"}, soon())
	if st != ContentVerified || c.Text != "Neutral final reply." || len(c.ToolCalls) != 1 || len(c.Summaries) != 2 {
		t.Fatalf("state=%v content=%+v", st, c)
	}
	if strings.Contains(c.Text, "Unrelated") {
		t.Error("a response that was not asked for was read")
	}
}

func TestReadResponseContentUnknownIDIsAbsent(t *testing.T) {
	root, _ := contentRoot(t, "sess-c1", "session-jsonl-content.jsonl")
	c, st := ReadResponseContent(root, "sess-c1", []string{"resp_nope"}, soon())
	if st != ContentAbsent || c.Text != "" || len(c.ToolCalls) != 0 {
		t.Fatalf("state=%v content=%+v", st, c)
	}
}

func TestReadResponseContentNoLogIsAbsent(t *testing.T) {
	c, st := ReadResponseContent(t.TempDir(), "sess-none", []string{"resp_a"}, soon())
	if st != ContentAbsent || c.Text != "" {
		t.Fatalf("state=%v content=%+v", st, c)
	}
	if _, st := ReadResponseContent("", "sess-c1", []string{"resp_a"}, soon()); st != ContentAbsent {
		t.Errorf("empty root state = %v", st)
	}
	if _, st := ReadResponseContent(t.TempDir(), "../x", []string{"resp_a"}, soon()); st != ContentAbsent {
		t.Errorf("unsafe session id state = %v", st)
	}
}

func TestReadResponseContentRefusesEchoID(t *testing.T) {
	root, _ := contentRoot(t, "sess-c1", "session-jsonl-content.jsonl")
	body, _ := os.ReadFile(filepath.Join("testdata", "session-jsonl-content.jsonl"))
	appendLog(t, filepath.Join(dateDirs(root, time.Now())[0], "sess-c1", sessionLogName),
		strings.ReplaceAll(string(body), "resp_b", EchoResponseID))
	if _, st := ReadResponseContent(root, "sess-c1", []string{EchoResponseID}, soon()); st != ContentAbsent {
		t.Fatalf("echo id state = %v, want absent", st)
	}
}

// The envelope's schema_version:2 is drift; the nested schema_version:2 on a
// kind this reader does not read (seen live) is not.
func TestReadResponseContentDriftIsUnverifiedWithNoContent(t *testing.T) {
	root, _ := contentRoot(t, "sess-c1", "session-jsonl-content-drift.jsonl")
	c, st := ReadResponseContent(root, "sess-c1", []string{"resp_a", "resp_b"}, soon())
	if st != ContentUnverified {
		t.Fatalf("state = %v, want unverified", st)
	}
	assertEmptyContent(t, c)
}

func TestReadResponseContentIgnoresNestedSchemaVersionOnOtherKinds(t *testing.T) {
	body, _ := os.ReadFile(filepath.Join("testdata", "session-jsonl-content.jsonl"))
	if !strings.Contains(string(body), `"kind":"model_input_trace_recorded","schema_version":2`) {
		t.Fatal("fixture lost its nested schema_version:2 line")
	}
	root, _ := contentRootFrom(t, "sess-c1", string(body))
	if _, st := ReadResponseContent(root, "sess-c1", []string{"resp_b"}, soon()); st != ContentVerified {
		t.Fatalf("state = %v, want verified", st)
	}
}

func TestReadResponseContentDriftVariants(t *testing.T) {
	body, _ := os.ReadFile(filepath.Join("testdata", "session-jsonl-content.jsonl"))
	good := string(body)
	cases := map[string]string{
		"payload_schema_version 2 on a read kind": strings.Replace(good,
			`"payload_type":"runtime.session","payload_schema_version":1,"payload":{"event":{"kind":"assistant_message_committed"`,
			`"payload_type":"runtime.session","payload_schema_version":2,"payload":{"event":{"kind":"assistant_message_committed"`, 1),
		"message without text":   strings.Replace(good, `"text":"Neutral final reply.",`, ``, 1),
		"message with null text": strings.Replace(good, `"text":"Neutral final reply."`, `"text":null`, 1),
		"tool call without name": strings.Replace(good, `"name":"bash",`, ``, 1),
		"tool call args not a string": strings.Replace(good,
			`"args":"{\"command\":\"ls\"}"`, `"args":{"command":"ls"}`, 1),
		"corrupt matched line": strings.Replace(good,
			`"text":"Neutral final reply.","provider_item_id":"p4"}}}`, `"text":"Neutral final reply.","provider_item_id":"p4"}}`, 1),
	}
	for name, mutated := range cases {
		t.Run(name, func(t *testing.T) {
			if mutated == good {
				t.Fatal("mutation did not apply")
			}
			root, _ := contentRootFrom(t, "sess-c1", mutated)
			c, st := ReadResponseContent(root, "sess-c1", []string{"resp_a", "resp_b"}, soon())
			if st != ContentUnverified {
				t.Fatalf("state = %v, want unverified", st)
			}
			assertEmptyContent(t, c)
		})
	}
}

func TestReadResponseContentNoEnvelopeInProbeWindowIsUnverified(t *testing.T) {
	var b strings.Builder
	for i := 0; i < minProbeLines+2; i++ {
		b.WriteString(`{"frame_schema_version":1,"retained_frame":"x"}` + "\n")
	}
	root, _ := contentRootFrom(t, "sess-c1", b.String())
	c, st := ReadResponseContent(root, "sess-c1", []string{"resp_a"}, soon())
	if st != ContentUnverified {
		t.Fatalf("state = %v, want unverified", st)
	}
	assertEmptyContent(t, c)
	// A log that has only just begun is not drift.
	root, _ = contentRootFrom(t, "sess-c1", `{"frame_schema_version":1}`+"\n")
	if _, st := ReadResponseContent(root, "sess-c1", []string{"resp_a"}, soon()); st != ContentAbsent {
		t.Errorf("young log state = %v, want absent", st)
	}
}

func TestReadResponseContentIgnoresTornLastLine(t *testing.T) {
	body, _ := os.ReadFile(filepath.Join("testdata", "session-jsonl-content.jsonl"))
	torn := string(body) + `{"schema_version":1,"payload":{"event":{"kind":"assistant_message_committed","response_id":"resp_a","text":"half`
	root, _ := contentRootFrom(t, "sess-c1", torn)
	c, st := ReadResponseContent(root, "sess-c1", []string{"resp_a"}, soon())
	if st != ContentVerified || c.Text != "" {
		t.Fatalf("state=%v text=%q", st, c.Text)
	}
}

func TestReadResponseContentFindsSubagentLogUnderParent(t *testing.T) {
	body, _ := os.ReadFile(filepath.Join("testdata", "session-jsonl-content.jsonl"))
	root := t.TempDir()
	parent := filepath.Join(dateDirs(root, time.Now())[0], "sess-parent")
	writeLog(t, filepath.Join(parent, sessionLogName), string(body)[:strings.Index(string(body), "\n")+1])
	writeLog(t, filepath.Join(parent, subagentDir, "sess-child", sessionLogName), string(body))
	c, st := ReadResponseContent(root, "sess-child", []string{"resp_b"}, soon())
	if st != ContentVerified || c.Text != "Neutral final reply." {
		t.Fatalf("state=%v text=%q", st, c.Text)
	}
	// And the parent's own id reaches its subagent's log too.
	if c, st := ReadResponseContent(root, "sess-parent", []string{"resp_b"}, soon()); st != ContentVerified || c.Text == "" {
		t.Fatalf("parent lookup state=%v text=%q", st, c.Text)
	}
}

func TestReadResponseContentBoundsTextPerResponse(t *testing.T) {
	big := strings.Repeat("a", hookflow.MaxRedactBody+4096)
	body := contentEnvelopeLines(
		`{"kind":"model_response_created","response_id":"resp_big"}`,
		`{"kind":"assistant_message_committed","message_id":"m","response_id":"resp_big","text":"`+big+`"}`,
		`{"kind":"model_completed","duration_ms":1,"model":"m"}`,
	)
	root, _ := contentRootFrom(t, "sess-c1", body)
	c, st := ReadResponseContent(root, "sess-c1", []string{"resp_big"}, soon())
	if st != ContentVerified {
		t.Fatalf("state = %v", st)
	}
	if n := len(c.Text); n == 0 || n > hookflow.MaxRedactBody {
		t.Errorf("text bytes = %d, want 1..%d", n, hookflow.MaxRedactBody)
	}
}

func TestReadResponseContentBoundIsInBytesNotRunes(t *testing.T) {
	cjk := strings.Repeat("漢", hookflow.MaxRedactBody/3+2000) // > MaxRedactBody bytes, far fewer runes
	body := contentEnvelopeLines(
		`{"kind":"model_response_created","response_id":"resp_cjk"}`,
		`{"kind":"assistant_message_committed","message_id":"m","response_id":"resp_cjk","text":"`+cjk+`"}`,
	)
	root, _ := contentRootFrom(t, "sess-c1", body)
	c, _ := ReadResponseContent(root, "sess-c1", []string{"resp_cjk"}, soon())
	if len(c.Text) > hookflow.MaxRedactBody || len(c.Text) == 0 {
		t.Errorf("text bytes = %d", len(c.Text))
	}
	if !json.Valid([]byte(`"` + c.Text + `"`)) {
		t.Error("truncation split a rune")
	}
}

func TestReadResponseContentOversizeLineIsAbsentNotPartial(t *testing.T) {
	huge := strings.Repeat("x", maxContentLineBytes+1024)
	body := contentEnvelopeLines(
		`{"kind":"model_response_created","response_id":"resp_h"}`,
		`{"kind":"assistant_tool_calls_committed","message_id":"m","response_id":"resp_h","tool_calls":[{"id":"i","call_id":"c","name":"write","args":"`+huge+`"}]}`,
		`{"kind":"model_completed","duration_ms":1,"model":"m"}`,
	)
	root, _ := contentRootFrom(t, "sess-c1", body)
	c, st := ReadResponseContent(root, "sess-c1", []string{"resp_h"}, soon())
	if st != ContentAbsent {
		t.Fatalf("state = %v, want absent", st)
	}
	assertEmptyContent(t, c)
}

func TestReadResponseContentOversizeLineOfAnotherKindIsStepped(t *testing.T) {
	huge := strings.Repeat("x", maxContentLineBytes+1024)
	body := contentEnvelopeLines(
		`{"kind":"model_response_created","response_id":"resp_s"}`,
		`{"kind":"tool_result_batch_committed","results":[{"text":"`+huge+`","tool_call_id":"c"}]}`,
		`{"kind":"assistant_message_committed","message_id":"m","response_id":"resp_s","text":"after"}`,
	)
	root, _ := contentRootFrom(t, "sess-c1", body)
	c, st := ReadResponseContent(root, "sess-c1", []string{"resp_s"}, soon())
	if st != ContentVerified || c.Text != "after" {
		t.Fatalf("state=%v text=%q", st, c.Text)
	}
}

func TestReadResponseContentHonoursDeadline(t *testing.T) {
	root, _ := contentRoot(t, "sess-c1", "session-jsonl-content.jsonl")
	c, st := ReadResponseContent(root, "sess-c1", []string{"resp_b"}, time.Now().Add(-time.Second))
	if st != ContentTimeout {
		t.Fatalf("state = %v, want timeout", st)
	}
	assertEmptyContent(t, c)
}

func TestContentReaderResumesWithoutRescanning(t *testing.T) {
	body, _ := os.ReadFile(filepath.Join("testdata", "session-jsonl-content.jsonl"))
	full := string(body)
	cut := strings.Index(full, `{"kind":"assistant_message_committed"`)
	cut = strings.LastIndex(full[:cut], "\n") + 1 // start of resp_b's message line
	root, logPath := contentRootFrom(t, "sess-c1", full[:cut])
	r := NewContentReader(root, "sess-c1", []string{"resp_b"})
	c, st := r.Read(soon())
	if st != ContentVerified || c.Text != "" || c.Complete == false {
		t.Fatalf("first read: state=%v content=%+v", st, c)
	}
	off := r.files[logPath].offset
	if off != int64(cut) {
		t.Fatalf("offset = %d, want %d", off, cut)
	}
	// The next line arrives torn, then whole: the offset never passes a torn line.
	line := full[cut:]
	appendLog(t, logPath, line[:20])
	if _, st := r.Read(soon()); st != ContentVerified || r.files[logPath].offset != off {
		t.Fatalf("torn tail moved the offset to %d (state %v)", r.files[logPath].offset, st)
	}
	appendLog(t, logPath, line[20:])
	c, st = r.Read(soon())
	if st != ContentVerified || c.Text != "Neutral final reply." {
		t.Fatalf("resumed read: state=%v text=%q", st, c.Text)
	}
	// A second pass adds nothing twice.
	if c2, _ := r.Read(soon()); c2.Text != c.Text {
		t.Errorf("resumed text = %q, want %q", c2.Text, c.Text)
	}
}

func TestContentReaderTimeoutThenResume(t *testing.T) {
	root, _ := contentRoot(t, "sess-c1", "session-jsonl-content.jsonl")
	r := NewContentReader(root, "sess-c1", []string{"resp_b"})
	if _, st := r.Read(time.Now().Add(-time.Second)); st != ContentTimeout {
		t.Fatalf("state = %v, want timeout", st)
	}
	if c, st := r.Read(soon()); st != ContentVerified || c.Text != "Neutral final reply." {
		t.Fatalf("after timeout: state=%v text=%q", st, c.Text)
	}
}

func TestContentReaderDriftIsSticky(t *testing.T) {
	root, _ := contentRoot(t, "sess-c1", "session-jsonl-content-drift.jsonl")
	r := NewContentReader(root, "sess-c1", []string{"resp_a"})
	for i := 0; i < 2; i++ {
		if c, st := r.Read(soon()); st != ContentUnverified {
			t.Fatalf("read %d: state = %v", i, st)
		} else {
			assertEmptyContent(t, c)
		}
	}
}

func TestResponseBodyJSONShapeAndRedaction(t *testing.T) {
	c := ResponseContent{
		Text:         "reply SECRET",
		Summaries:    []string{"why SECRET"},
		ToolCalls:    []ToolCall{{CallID: "c1", Name: "bash", Args: `{"a":"SECRET"}`}},
		FinishReason: "stop",
	}
	redact := func(s string) string { return strings.ReplaceAll(s, "SECRET", "[R]") }
	out := ResponseBodyJSON("resp_x", c, redact)
	if strings.Contains(out, "SECRET") {
		t.Fatalf("unredacted: %s", out)
	}
	var got struct {
		ResponseID   string `json:"response_id"`
		FinishReason string `json:"finish_reason"`
		Output       []struct {
			Type      string `json:"type"`
			Text      string `json:"text"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"output"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if got.ResponseID != "resp_x" || got.FinishReason != "stop" || len(got.Output) != 3 {
		t.Fatalf("doc = %+v", got)
	}
	if got.Output[0].Type != "reasoning_summary" || got.Output[0].Text != "why [R]" ||
		got.Output[1].Type != "message" || got.Output[1].Text != "reply [R]" ||
		got.Output[2].Type != "tool_call" || got.Output[2].CallID != "c1" || got.Output[2].Name != "bash" ||
		got.Output[2].Arguments != `{"a":"[R]"}` {
		t.Errorf("output = %+v", got.Output)
	}
	if ResponseBodyJSON("resp_x", ResponseContent{}, nil) != "" {
		t.Error("an empty response must produce no body")
	}
}

// The content-blind join reader must stay content-blind: its record types may
// not grow a field that could hold text.
func TestJoinReaderStaysContentBlind(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "sessionlog.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		"joinRecord": {"CallID", "PayloadKind", "PayloadType", "RecKind", "RecordType", "RecordedAt", "SchemaVersion", "ToolName"},
		"intent":     {"At", "ToolName", "ToolUseID"},
	}
	found := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok {
			return true
		}
		wantFields, tracked := want[ts.Name.Name]
		st, isStruct := ts.Type.(*ast.StructType)
		if !tracked || !isStruct {
			return true
		}
		found[ts.Name.Name] = true
		var got []string
		for _, fl := range st.Fields.List {
			for _, nm := range fl.Names {
				got = append(got, nm.Name)
			}
		}
		sort.Strings(got)
		sort.Strings(wantFields)
		if strings.Join(got, ",") != strings.Join(wantFields, ",") {
			t.Errorf("%s fields = %v, want %v: the join reader must not decode content", ts.Name.Name, got, wantFields)
		}
		return true
	})
	for name := range want {
		if !found[name] {
			t.Errorf("type %s not found in sessionlog.go", name)
		}
	}
}

func assertEmptyContent(t *testing.T, c ResponseContent) {
	t.Helper()
	if c.Text != "" || len(c.Summaries) != 0 || len(c.ToolCalls) != 0 || c.FinishReason != "" || c.Complete {
		t.Errorf("content returned with a non-verified state: %+v", c)
	}
}

// contentEnvelopeLines wraps event JSON objects in the observed envelope, after
// a frame header line.
func contentEnvelopeLines(events ...string) string {
	var b strings.Builder
	b.WriteString(`{"frame_schema_version":1,"retained_frame":"x"}` + "\n")
	for i, e := range events {
		b.WriteString(`{"schema_version":1,"sequence":` + itoa(i+2) + `,"record_type":"event","payload_type":"runtime.session","payload_schema_version":1,"payload":{"event":` + e + `}}` + "\n")
	}
	return b.String()
}

func itoa(i int) string { b, _ := json.Marshal(i); return string(b) }

// A journal replaced by a shorter one is read from the start, and what the
// old one contributed is forgotten.
func TestContentReaderResetsWhenTheJournalShrinks(t *testing.T) {
	root, logPath := contentRoot(t, "sess-c1", "session-jsonl-content.jsonl")
	r := NewContentReader(root, "sess-c1", []string{"resp_b"})
	if c, st := r.Read(soon()); st != ContentVerified || c.Text != "Neutral final reply." {
		t.Fatalf("first read = %v %q", st, c.Text)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.SplitAfter(string(data), "\n")
	// Keep only the first line (a frame header): shorter, and without resp_b.
	if err := os.WriteFile(logPath, []byte(lines[0]), 0o600); err != nil {
		t.Fatal(err)
	}
	if c, st := r.Read(soon()); st == ContentVerified {
		t.Errorf("stale content survived the shrink: %q", c.Text)
	}
}
