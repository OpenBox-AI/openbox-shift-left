package codex

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/client"
)

const contentThread = "0000aaaa-0000-4000-8000-000000000001"

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		panic(err)
	}
	return t
}

// installRollout files a fixture the way Codex does, under a date directory the
// reader does not know in advance.
func installRollout(t *testing.T, fixture, thread string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", fixture))
	if err != nil {
		t.Fatal(err)
	}
	return installRolloutBytes(t, raw, thread)
}

func installRolloutBytes(t *testing.T, raw []byte, thread string) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "2026", "10", "01")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "rollout-2026-10-01T16-08-54-"+thread+".jsonl"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

var farDeadline = func() time.Time { return time.Now().Add(10 * time.Second) }

func texts(items []CallItem) []string {
	var out []string
	for _, it := range items {
		out = append(out, it.Type+":"+it.Text+it.Arguments)
	}
	return out
}

func TestReadCallContent_JoinsByUsageAndSplitsInputFromOutput(t *testing.T) {
	root := installRollout(t, "rollout-content.jsonl", contentThread)

	// The second call: its output is the reasoning summary and the tool call;
	// its input is everything before, including the first call's own output and
	// the tool result it produced.
	got, st := ReadCallContent(root, contentThread,
		CallJoin{At: at("2026-10-01T09:09:07.4Z"), Input: 1100, Output: 60, CacheRead: 1000}, farDeadline())
	if st != ContentVerified {
		t.Fatalf("state %q, want verified", st)
	}
	if got.ResponseID != "resp_two" {
		t.Errorf("response id %q", got.ResponseID)
	}
	wantResp := []string{"reasoning_summary:Neutral summary two.", `tool_call:{"cmd":"neutral"}`}
	if g := texts(got.Response); strings.Join(g, "|") != strings.Join(wantResp, "|") {
		t.Errorf("response = %v, want %v", g, wantResp)
	}
	wantReq := []string{
		"message:Neutral developer text.", "message:Neutral user prompt one.", "message:Neutral assistant text one.",
		"tool_call:neutral tool input one", "tool_output:neutral tool output one",
	}
	if g := texts(got.Request); strings.Join(g, "|") != strings.Join(wantReq, "|") {
		t.Errorf("request = %v, want %v", g, wantReq)
	}
	if got.Request[0].Role != "developer" || got.Request[3].Name != "exec" || got.Request[3].CallID != "call_1" {
		t.Errorf("structure lost: %+v", got.Request)
	}
}

func TestReadCallContent_FirstAndLastCall(t *testing.T) {
	root := installRollout(t, "rollout-content.jsonl", contentThread)
	first, st := ReadCallContent(root, contentThread,
		CallJoin{At: at("2026-10-01T09:09:00.9Z"), Input: 1000, Output: 50, CacheRead: 400}, farDeadline())
	if st != ContentVerified || len(first.Request) != 2 || len(first.Response) != 2 {
		t.Fatalf("first call: state %q request %v response %v", st, texts(first.Request), texts(first.Response))
	}
	last, st := ReadCallContent(root, contentThread,
		CallJoin{At: at("2026-10-01T09:09:11.1Z"), Input: 1200, Output: 30, CacheRead: 1100}, farDeadline())
	if st != ContentVerified || len(last.Response) != 1 || last.Response[0].Text != "Neutral final reply." {
		t.Fatalf("last call: state %q response %v", st, texts(last.Response))
	}
	if n := len(last.Request); n != 8 {
		t.Errorf("last call's request holds %d item(s), want the whole earlier conversation (8): %v", n, texts(last.Request))
	}
}

func TestReadCallContent_TokenMismatchIsNoJoinNeverAGuess(t *testing.T) {
	root := installRollout(t, "rollout-content.jsonl", contentThread)
	for name, j := range map[string]CallJoin{
		"input differs":   {At: at("2026-10-01T09:09:07.4Z"), Input: 1101, Output: 60, CacheRead: 1000},
		"output differs":  {At: at("2026-10-01T09:09:07.4Z"), Input: 1100, Output: 61, CacheRead: 1000},
		"cache differs":   {At: at("2026-10-01T09:09:07.4Z"), Input: 1100, Output: 60, CacheRead: 999},
		"too far in time": {At: at("2026-10-01T10:09:07.4Z"), Input: 1100, Output: 60, CacheRead: 1000},
	} {
		got, st := ReadCallContent(root, contentThread, j, farDeadline())
		if st != ContentNoJoin || len(got.Request)+len(got.Response) != 0 {
			t.Errorf("%s: state %q, %d item(s); want no_join and no content", name, st, len(got.Request)+len(got.Response))
		}
	}
}

// Two calls with the same counts are told apart by time; two equally near are
// not told apart at all.
func TestReadCallContent_IdenticalUsageIsDisambiguatedByTimeOrRefused(t *testing.T) {
	base, err := os.ReadFile(filepath.Join("testdata", "rollout-content.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var recs []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(string(base)), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatal(err)
		}
		recs = append(recs, m)
	}
	// Re-time call three's usage record so it duplicates call two's counts.
	for _, m := range recs {
		p, _ := m["payload"].(map[string]any)
		if m["type"] == "token_usage_record" && p["response_id"] == "resp_three" {
			p["usage"] = map[string]any{"input_tokens": 1100, "cached_input_tokens": 1000, "cache_write_input_tokens": 0,
				"output_tokens": 60, "reasoning_output_tokens": 20, "total_tokens": 1160}
		}
	}
	var buf strings.Builder
	for _, m := range recs {
		b, _ := json.Marshal(m)
		buf.Write(b)
		buf.WriteByte('\n')
	}
	root := installRolloutBytes(t, []byte(buf.String()), contentThread)

	two, st := ReadCallContent(root, contentThread, CallJoin{At: at("2026-10-01T09:09:07.4Z"), Input: 1100, Output: 60, CacheRead: 1000}, farDeadline())
	if st != ContentVerified || two.ResponseID != "resp_two" {
		t.Errorf("nearest to call two: state %q id %q", st, two.ResponseID)
	}
	three, st := ReadCallContent(root, contentThread, CallJoin{At: at("2026-10-01T09:09:11.2Z"), Input: 1100, Output: 60, CacheRead: 1000}, farDeadline())
	if st != ContentVerified || three.ResponseID != "resp_three" {
		t.Errorf("nearest to call three: state %q id %q", st, three.ResponseID)
	}
	// Exactly between the two (09:09:07.397 and 09:09:11.159).
	mid := at("2026-10-01T09:09:07.397Z").Add(at("2026-10-01T09:09:11.159Z").Sub(at("2026-10-01T09:09:07.397Z")) / 2)
	if _, st := ReadCallContent(root, contentThread, CallJoin{At: mid, Input: 1100, Output: 60, CacheRead: 1000}, farDeadline()); st != ContentNoJoin {
		t.Errorf("equidistant records: state %q, want no_join", st)
	}
}

func TestReadCallContent_DriftIsUnverifiedWithNoContent(t *testing.T) {
	join := CallJoin{At: at("2026-10-01T09:09:07.4Z"), Input: 1100, Output: 60, CacheRead: 1000}
	cases := map[string][]byte{}
	raw, _ := os.ReadFile(filepath.Join("testdata", "rollout-content-drift.jsonl"))
	cases["usage lost its numbers"] = raw
	good, _ := os.ReadFile(filepath.Join("testdata", "rollout-content.jsonl"))
	cases["thread id differs"] = []byte(strings.Replace(string(good), `"id":"`+contentThread+`","session_id"`, `"id":"someone-else","session_id"`, 1))
	cases["no session_meta"] = []byte(strings.SplitN(string(good), "\n", 2)[1])
	cases["record without a type"] = []byte(strings.Replace(string(good), `"type":"response_item"`, `"kind":"response_item"`, 1))
	cases["not json"] = append([]byte("this is not json\n"), good...)
	for name, body := range cases {
		root := installRolloutBytes(t, body, contentThread)
		got, st := ReadCallContent(root, contentThread, join, farDeadline())
		if st != ContentUnverified || len(got.Request)+len(got.Response) != 0 || got.ResponseID != "" {
			t.Errorf("%s: state %q content %+v; want unverified and nothing", name, st, got)
		}
	}
}

func TestReadCallContent_AbsentLogAndUnsafeThread(t *testing.T) {
	root := t.TempDir()
	if _, st := ReadCallContent(root, contentThread, CallJoin{}, farDeadline()); st != ContentLogAbsent {
		t.Errorf("no file: %q", st)
	}
	if _, st := ReadCallContent("", contentThread, CallJoin{}, farDeadline()); st != ContentLogAbsent {
		t.Errorf("no root: %q", st)
	}
	for _, bad := range []string{"", "..", "a/b", "*", "a*", "[x]"} {
		if _, st := ReadCallContent(root, bad, CallJoin{}, farDeadline()); st != ContentLogAbsent {
			t.Errorf("thread %q: %q, want log_absent without globbing", bad, st)
		}
	}
}

func TestReadCallContent_ExpiredDeadlineIsTimeout(t *testing.T) {
	root := installRollout(t, "rollout-content.jsonl", contentThread)
	_, st := ReadCallContent(root, contentThread,
		CallJoin{At: at("2026-10-01T09:09:07.4Z"), Input: 1100, Output: 60, CacheRead: 1000}, time.Now().Add(-time.Second))
	if st != ContentTimeout {
		t.Errorf("state %q, want timeout", st)
	}
}

// A line past the cap is replaced by a marker, not parsed and not dropped
// silently; the join around it is unaffected.
func TestReadCallContent_OversizeLineBecomesMarker(t *testing.T) {
	good, _ := os.ReadFile(filepath.Join("testdata", "rollout-content.jsonl"))
	lines := strings.Split(strings.TrimSpace(string(good)), "\n")
	huge := `{"timestamp":"2026-10-01T09:08:57.5Z","ordinal":99,"type":"response_item","payload":{"type":"message","role":"user",` +
		`"content":[{"type":"input_text","text":"` + strings.Repeat("x", maxRolloutLineBytes+10) + `"}]}}`
	body := strings.Join(lines[:4], "\n") + "\n" + huge + "\n" + strings.Join(lines[4:], "\n") + "\n"
	root := installRolloutBytes(t, []byte(body), contentThread)
	got, st := ReadCallContent(root, contentThread,
		CallJoin{At: at("2026-10-01T09:09:00.9Z"), Input: 1000, Output: 50, CacheRead: 400}, farDeadline())
	if st != ContentVerified {
		t.Fatalf("state %q", st)
	}
	var marker bool
	for _, it := range got.Request {
		if it.Type == "omitted" {
			marker = true
		}
		if strings.Contains(it.Text, "xxxx") {
			t.Error("oversize line content leaked")
		}
	}
	if !marker {
		t.Errorf("no omitted marker: %v", texts(got.Request))
	}
}

// Only reasoning summaries are read from a reasoning item; the opaque blob has
// no Go name to egress.
func TestReadCallContent_NeverBindsOpaqueBlobsOrMetadata(t *testing.T) {
	root := installRollout(t, "rollout-content.jsonl", contentThread)
	got, _ := ReadCallContent(root, contentThread,
		CallJoin{At: at("2026-10-01T09:09:11.1Z"), Input: 1200, Output: 30, CacheRead: 1100}, farDeadline())
	b, _ := json.Marshal(got)
	for _, banned := range []string{"opaque-blob", "base_instructions", "Neutral base instructions", "/work/neutral", "create_time"} {
		if strings.Contains(string(b), banned) {
			t.Errorf("%q reached the content", banned)
		}
	}
}

// rolloutContentAllowedFields classifies every key the fixture's relevant
// records carry, bound or ignored, the way the usage reader's allowlist does:
// a key nobody has classified fails the test.
func TestRolloutContentAllowlistIsExhaustive(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "rollout-content.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var line struct {
			Type    string                     `json:"type"`
			Payload map[string]json.RawMessage `json:"payload"`
		}
		if err := json.Unmarshal([]byte(l), &line); err != nil {
			t.Fatal(err)
		}
		if line.Type != "response_item" && line.Type != "token_usage_record" && line.Type != "session_meta" {
			continue
		}
		for k := range line.Payload {
			if _, ok := rolloutContentAllowedFields[k]; !ok {
				t.Errorf("%s payload key %q is neither bound nor ignored in rolloutContentAllowedFields", line.Type, k)
			}
		}
	}
}

func TestBodiesRedactBeforeAttachAndStayValidJSON(t *testing.T) {
	secret := "AKIA" + "IOSFODNN7EXAMPLE"
	redact := func(s string) string { return strings.ReplaceAll(s, secret, "[REDACTED]") }
	items := []CallItem{
		{Type: "message", Role: "user", Text: "key is " + secret + " ok"},
		{Type: "tool_call", CallID: "c1", Name: "exec", Arguments: `{"k":"` + secret + `"}`},
	}
	req := RequestBodyJSON(items, redact)
	resp := ResponseBodyJSON("resp_x", items, redact)
	for _, b := range []string{req, resp} {
		if strings.Contains(b, secret) || !json.Valid([]byte(b)) {
			t.Errorf("body not redacted/valid: %s", b)
		}
	}
	var doc struct {
		ResponseID string     `json:"response_id"`
		Output     []CallItem `json:"output"`
	}
	if err := json.Unmarshal([]byte(resp), &doc); err != nil || doc.ResponseID != "resp_x" || len(doc.Output) != 2 {
		t.Errorf("response doc = %+v, err %v", doc, err)
	}
	if RequestBodyJSON(nil, redact) != "" || ResponseBodyJSON("r", nil, redact) != "" {
		t.Error("an empty side must give no body")
	}
}

func TestBodiesAreBoundedNewestFirst(t *testing.T) {
	var items []CallItem
	for i := 0; i < 40; i++ {
		items = append(items, CallItem{Type: "message", Role: "user", Text: strings.Repeat("z", 4000) + fmt.Sprintf("|n%02d", i)})
	}
	req := RequestBodyJSON(items, nil)
	if len(req) > client.MaxModelCallBodyBytes || !json.Valid([]byte(req)) {
		t.Fatalf("request %d bytes, valid %v", len(req), json.Valid([]byte(req)))
	}
	if !strings.Contains(req, "|n39") {
		t.Error("the newest item was cut")
	}
	if strings.Contains(req, "|n00") {
		t.Error("the oldest item survived a full budget")
	}

	// One output far over the budget still yields a valid, bounded body.
	big := []CallItem{{Type: "message", Role: "assistant", Text: strings.Repeat("é", 200000)}}
	resp := ResponseBodyJSON("r", big, nil)
	if len(resp) == 0 || len(resp) > client.MaxModelCallBodyBytes || !json.Valid([]byte(resp)) {
		t.Errorf("response %d bytes, valid %v", len(resp), json.Valid([]byte(resp)))
	}
}

func TestReadCallContent_ExcludedRecordIsNotACandidate(t *testing.T) {
	root := installRollout(t, "rollout-content.jsonl", contentThread)
	j := CallJoin{At: at("2026-10-01T09:09:07.4Z"), Input: 1100, Output: 60, CacheRead: 1000,
		Exclude: func(id string) bool { return id == "resp_two" }}
	got, st := ReadCallContent(root, contentThread, j, farDeadline())
	if st != ContentNoJoin || got.ResponseID != "" {
		t.Errorf("state %q id %q; an excluded record must leave no join", st, got.ResponseID)
	}
}
