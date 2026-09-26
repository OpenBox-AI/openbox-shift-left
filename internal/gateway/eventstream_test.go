package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/client/memhttptest"
)

func sseHeader() http.Header {
	h := http.Header{}
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	return h
}

func frame(event, data string) string {
	return "event: " + event + "\ndata: " + data + "\n\n"
}

// claudeAIStream is the shape a live claude.ai turn streamed: pings and the
// surface's own conversation_ready around the Messages-format frames, a text
// block split per token, and a tool call whose input arrives as partial JSON.
func claudeAIStream() string {
	var b strings.Builder
	b.WriteString(frame("ping", `{"type":"ping"}`))
	b.WriteString(frame("conversation_ready", `{"type":"conversation_ready"}`))
	b.WriteString(frame("message_start", `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5-5","content":[],"stop_reason":null,"usage":{"input_tokens":12}}}`))
	b.WriteString(frame("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`))
	for _, tok := range []string{"Ha", " Noi", " is the capital."} {
		b.WriteString(frame("content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":%q}}`, tok)))
		b.WriteString(frame("ping", `{"type":"ping"}`))
	}
	b.WriteString(frame("content_block_stop", `{"type":"content_block_stop","index":0}`))
	b.WriteString(frame("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"tu_1","name":"places_search","input":{}}}`))
	b.WriteString(frame("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"query\":"}}`))
	b.WriteString(frame("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"Ha Noi\"}"}}`))
	b.WriteString(frame("content_block_stop", `{"type":"content_block_stop","index":1}`))
	b.WriteString(frame("message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":9}}`))
	b.WriteString(frame("message_stop", `{"type":"message_stop"}`))
	return b.String()
}

type assembledForTest struct {
	ID         string         `json:"id"`
	Model      string         `json:"model"`
	StopReason string         `json:"stop_reason"`
	Usage      map[string]any `json:"usage"`
	Assembly   assemblyNote   `json:"openbox_assembly"`
	Content    []struct {
		Type  string          `json:"type"`
		Text  string          `json:"text"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	} `json:"content"`
}

// TestAStreamedReplyIsReassembledIntoOneMessage is the load-bearing assertion:
// the stored reply is the message the stream described, not hundreds of frames.
func TestAStreamedReplyIsReassembledIntoOneMessage(t *testing.T) {
	got := assembleEventStream(claudeAIStream(), sseHeader())

	var m assembledForTest
	if err := json.Unmarshal([]byte(got), &m); err != nil {
		t.Fatalf("assembled reply does not parse: %v\n%s", err, got)
	}
	if m.ID != "msg_1" || m.Model != "claude-opus-5-5" || m.StopReason != "tool_use" {
		t.Errorf("message fields = %+v", m)
	}
	if m.Usage["input_tokens"] != float64(12) || m.Usage["output_tokens"] != float64(9) {
		t.Errorf("usage = %v, want start and delta usage merged", m.Usage)
	}
	if len(m.Content) != 2 {
		t.Fatalf("content has %d blocks, want 2: %s", len(m.Content), got)
	}
	if m.Content[0].Type != "text" || m.Content[0].Text != "Ha Noi is the capital." {
		t.Errorf("text block = %+v", m.Content[0])
	}
	if m.Content[1].Name != "places_search" || string(m.Content[1].Input) != `{"query":"Ha Noi"}` {
		t.Errorf("tool block = %+v input=%s", m.Content[1], m.Content[1].Input)
	}
	if m.Assembly.Incomplete {
		t.Error("a stream that reached message_stop must not be marked incomplete")
	}
	if strings.Join(m.Assembly.SkippedEventTypes, ",") != "conversation_ready,ping" {
		t.Errorf("skipped_event_types = %v, want the surface's own frames named", m.Assembly.SkippedEventTypes)
	}
	if strings.Contains(got, "text_delta") {
		t.Error("delta frames reached the assembled reply")
	}
	if !strings.HasSuffix(got, "]}") {
		t.Errorf("content must be the last key: %.120q", got[max(0, len(got)-120):])
	}
}

// TestACutStreamIsMarkedIncomplete: a stream that ends mid-reply keeps the prefix
// that arrived, says so, and never turns the cut note into model output.
func TestACutStreamIsMarkedIncomplete(t *testing.T) {
	full := claudeAIStream()
	cut := full[:strings.Index(full, " is the capital.")] + bodyCutNote

	got := assembleEventStream(cut, sseHeader())
	var m assembledForTest
	if err := json.Unmarshal([]byte(got), &m); err != nil {
		t.Fatalf("assembled reply does not parse: %v\n%s", err, got)
	}
	if !m.Assembly.Incomplete {
		t.Error("a stream with no message_stop must be marked incomplete")
	}
	if len(m.Content) != 1 || m.Content[0].Text != "Ha Noi" {
		t.Errorf("content = %+v, want the text that arrived before the cut", m.Content)
	}
	if strings.Contains(got, "truncated here") {
		t.Error("the cut note leaked into the assembled reply")
	}
}

// TestBodiesThatAreNotAnthropicStreamsPassThrough: reassembly is tolerant, never
// a schema. Another format, another content type, or a marker is kept verbatim.
func TestBodiesThatAreNotAnthropicStreamsPassThrough(t *testing.T) {
	jsonHeader := http.Header{}
	jsonHeader.Set("Content-Type", "application/json")
	openAIStream := frame("response.output_text.delta", `{"type":"response.output_text.delta","delta":"hi"}`)
	marker := "[openbox: not captured; the zstd body could not be decoded]"

	for name, tc := range map[string]struct {
		body string
		h    http.Header
	}{
		"json reply":          {claudeAIStream(), jsonHeader},
		"other stream format": {openAIStream, sseHeader()},
		"marker":              {marker, sseHeader()},
		"not even frames":     {"plain text", sseHeader()},
	} {
		if got := assembleEventStream(tc.body, tc.h); got != tc.body {
			t.Errorf("%s: body changed to %.120q", name, got)
		}
	}
}

// TestADecodeCutStreamStillReportsTruncationThroughTheRelay: reassembly drops the
// cut note with the frames, so the relay must read the cut first or the row
// would claim a complete reply.
func TestADecodeCutStreamStillReportsTruncationThroughTheRelay(t *testing.T) {
	var b strings.Builder
	b.WriteString(frame("message_start", `{"type":"message_start","message":{"id":"msg_big","type":"message","role":"assistant","model":"m","content":[]}}`))
	b.WriteString(frame("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`))
	delta := frame("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"word "}}`)
	for b.Len() < 2*maxCaptureInputBytes {
		b.WriteString(delta)
	}
	b.WriteString(frame("message_stop", `{"type":"message_stop"}`))
	compressed := gzipOf(t, b.String())

	upstream := memhttptest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(compressed)
	}))
	t.Cleanup(upstream.Close)
	em := &recordingEmitter{}
	srv := serveGateway(t, wire(t, upstream.URL, em, nil, nil))

	resp, err := probeClient().Get(srv.URL + "/v1/messages")
	if err != nil {
		t.Fatalf("request through gateway: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	c := em.await(t, 1)[0]
	if !c.ResponseTruncated {
		t.Error("a reply the decode step cut must report ResponseTruncated")
	}
	var m assembledForTest
	if err := json.Unmarshal([]byte(c.ResponseBody), &m); err != nil {
		t.Fatalf("stored reply is not the assembled document: %v (%.120q)", err, c.ResponseBody)
	}
	if m.ID != "msg_big" || !m.Assembly.Incomplete {
		t.Errorf("stored reply = id %q incomplete %v, want msg_big marked incomplete", m.ID, m.Assembly.Incomplete)
	}
}
