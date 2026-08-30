package corpusfixture

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

const recordedBody = `{"model":"claude-x","system":[{"type":"text","text":"a recorded system preamble"}],` +
	`"messages":[` +
	`{"role":"user","content":[{"type":"text","text":"a recorded prompt carrying private prose"}]},` +
	`{"role":"system","content":"a recorded system-reminder block"},` +
	`{"role":"assistant","content":[` +
	`{"type":"thinking","thinking":"recorded chain of thought","signature":"AbCd=="},` +
	`{"type":"tool_use","id":"toolu_x","name":"Read","input":{"file_path":"/w/x.go","body":"recorded file contents"}}]},` +
	`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_x","content":"recorded tool output"}]}],` +
	`"tools":[{"name":"Read","description":"product tool description"}],"max_tokens":9}`

// TestSubstitutePromptTextPreservesRuneGeometry is the property the fixture
// scrub rests on: a soak test asserting an oversized body, and a replay test
// asserting byte counts, both stop meaning what they say if a substitution
// resizes the body.
func TestSubstitutePromptTextPreservesRuneGeometry(t *testing.T) {
	got := SubstitutePromptText(recordedBody)
	if a, b := utf8.RuneCountInString(got), utf8.RuneCountInString(recordedBody); a != b {
		t.Errorf("body is %d runes after substitution, was %d", a, b)
	}
	if !json.Valid([]byte(got)) {
		t.Fatal("substitution produced invalid JSON")
	}
}

// TestSubstitutePromptTextRemovesRecordedProse names the whole point: no
// recorded free text reaches disk.
func TestSubstitutePromptTextRemovesRecordedProse(t *testing.T) {
	got := SubstitutePromptText(recordedBody)
	for _, recorded := range []string{
		"a recorded prompt carrying private prose",
		"a recorded system-reminder block",
		"a recorded system preamble",
		"recorded chain of thought",
		"recorded file contents",
		"recorded tool output",
		"product tool description",
	} {
		if strings.Contains(got, recorded) {
			t.Errorf("recorded prose survived: %q", recorded)
		}
	}
}

// TestSubstitutePromptTextKeepsStructure holds the other half: a substitution
// that ate a block type or a tool-use id would leave a fixture that parses and
// replays while describing an exchange no provider would ever send.
func TestSubstitutePromptTextKeepsStructure(t *testing.T) {
	got := SubstitutePromptText(recordedBody)
	for _, structural := range []string{
		`"model":"claude-x"`, `"type":"text"`, `"role":"assistant"`,
		`"type":"tool_use"`, `"id":"toolu_x"`, `"name":"Read"`,
		`"tool_use_id":"toolu_x"`, `"signature":"AbCd=="`, `"max_tokens":9`,
		`"tools":[{"name":"Read"`,
	} {
		if !strings.Contains(got, structural) {
			t.Errorf("structural field lost: %s", structural)
		}
	}
}

// TestSubstitutePromptTextIsIdempotent is what makes a regenerated fixture
// reviewable: a second pass must produce the same bytes, or every regeneration
// is an unreadable diff.
func TestSubstitutePromptTextIsIdempotent(t *testing.T) {
	once := SubstitutePromptText(recordedBody)
	if twice := SubstitutePromptText(once); twice != once {
		t.Error("a second substitution changed the body")
	}
}

// TestSubstitutePromptTextLeavesNonRequestJSONAlone keeps the rule from firing
// on a document that is not a model-call request, where it would destroy
// evidence.
func TestSubstitutePromptTextLeavesNonRequestJSONAlone(t *testing.T) {
	other := `{"resourceLogs":[{"body":{"stringValue":"an OTLP log line"}}]}`
	if got := SubstitutePromptText(other); got != other {
		t.Errorf("substitution rewrote a non-request document:\n got %s\nwant %s", got, other)
	}
}

// TestScanRejectsRecordedPromptText is the gate that makes the substitution
// permanent.
func TestScanRejectsRecordedPromptText(t *testing.T) {
	doc := `{"request":{"body":` + jsonQuote(recordedBody) + `}}`
	v := Scan([]byte(doc))
	if len(v) == 0 {
		t.Fatal("Scan accepted a recorded prompt body carrying verbatim prose")
	}
}

// TestScanAcceptsSubstitutedPromptText is the other direction: a gate that
// rejected its own generator's output would be worse than no gate.
func TestScanAcceptsSubstitutedPromptText(t *testing.T) {
	doc := `{"request":{"body":` + jsonQuote(SubstitutePromptText(recordedBody)) + `}}`
	if v := Scan([]byte(doc)); len(v) != 0 {
		t.Fatalf("Scan rejected a substituted body: %d violation(s), first: %s", len(v), v[0])
	}
}

// TestScanRejectsASystemReminder names the leak vector itself.
func TestScanRejectsASystemReminder(t *testing.T) {
	doc := `{"note":"` + "<system-reminder>anything at all</system-reminder>" + `"}`
	if v := Scan([]byte(doc)); len(v) == 0 {
		t.Fatal("Scan accepted a document carrying a system-reminder block")
	}
}

// TestScanRejectsRecordedTelemetryContent covers the same class on the other
// lane: the OTLP corpus carries prompt and response bodies as attribute
// values.
func TestScanRejectsRecordedTelemetryContent(t *testing.T) {
	for _, key := range []string{"prompt", "response", "tool_input", "tool_parameters"} {
		doc := `{"attributes":[{"key":"` + key + `","value":{"stringValue":"a recorded body"}}]}`
		if v := Scan([]byte(doc)); len(v) == 0 {
			t.Errorf("Scan accepted recorded content under %q", key)
		}
		clean := `{"attributes":[{"key":"` + key + `","value":{"stringValue":` + jsonQuote(SyntheticProse(15)) + `}}]}`
		if v := Scan([]byte(clean)); len(v) != 0 {
			t.Errorf("Scan rejected filler under %q: %s", key, v[0])
		}
	}
}

// TestSanitizeSubstitutesRecordedContent holds the generator half: Sanitize
// must produce exactly what Scan admits, or every regeneration fails its own
// gate.
func TestSanitizeSubstitutesRecordedContent(t *testing.T) {
	doc := `{"request":{"body":` + jsonQuote(recordedBody) + `},` +
		`"attributes":[{"key":"response","value":{"stringValue":"a recorded reply"}}]}`
	clean, err := Sanitize([]byte(doc))
	if err != nil {
		t.Fatalf("Sanitize: %v", err)
	}
	if v := Scan(clean); len(v) != 0 {
		t.Fatalf("Sanitize output fails Scan: %d violation(s), first: %s", len(v), v[0])
	}
	if strings.Contains(string(clean), "a recorded reply") {
		t.Error("Sanitize left a recorded telemetry body verbatim")
	}
}

func jsonQuote(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// TestSubstitutePromptTextDoesNotRewriteKeys is the collision drill, and it is
// here because the collision happened.
func TestSubstitutePromptTextDoesNotRewriteKeys(t *testing.T) {
	body := `{"messages":[{"role":"assistant","content":[` +
		`{"type":"tool_use","id":"toolu_a","name":"Edit","input":{"mode":"text"}},` +
		`{"type":"text","text":"a recorded reply"}]}]}`
	got := SubstitutePromptText(body)
	var top struct {
		Messages []struct {
			Content []map[string]any `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(got), &top); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, got)
	}
	blocks := top.Messages[0].Content
	if blocks[0]["type"] != "tool_use" || blocks[1]["type"] != "text" {
		t.Errorf("block discriminators were rewritten: %v, %v", blocks[0]["type"], blocks[1]["type"])
	}
	if _, ok := blocks[1]["text"]; !ok {
		t.Errorf("the text key was rewritten: %v", blocks[1])
	}
	if blocks[1]["text"] == "a recorded reply" {
		t.Error("the recorded reply survived")
	}
}

// TestSubstitutePromptTextRecursesIntoDescriptionObjects covers a schema
// property literally named "description", where the key holds an object rather
// than a string and a walker that only handled the string case walked past the
// prose underneath it.
func TestSubstitutePromptTextRecursesIntoDescriptionObjects(t *testing.T) {
	body := `{"messages":[],"tools":[{"name":"Read","input_schema":{"properties":` +
		`{"description":{"type":"string","description":"recorded schema prose"}}}}]}`
	if got := SubstitutePromptText(body); strings.Contains(got, "recorded schema prose") {
		t.Error("prose under a property named description survived")
	}
}

// TestSubstituteSSEDeltasRemovesRecordedText covers the response side. A
// recorded event stream carries the model's reply as deltas, which the
// request-body rule never sees.
func TestSubstituteSSEDeltasRemovesRecordedText(t *testing.T) {
	sse := "event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"a recorded reply"}}` +
		"\n\nevent: content_block_start\n" +
		`data: {"type":"content_block_start","content_block":{"type":"text","text":"more recorded prose"}}` +
		"\n\n"
	got := SubstituteSSEDeltas(sse)
	if utf8.RuneCountInString(got) != utf8.RuneCountInString(sse) {
		t.Errorf("stream is %d runes, was %d", utf8.RuneCountInString(got), utf8.RuneCountInString(sse))
	}
	for _, recorded := range []string{"a recorded reply", "more recorded prose"} {
		if strings.Contains(got, recorded) {
			t.Errorf("recorded delta survived: %q", recorded)
		}
	}
	if !strings.Contains(got, `"type":"text_delta"`) || !strings.Contains(got, "event: content_block_delta") {
		t.Error("frame structure was rewritten")
	}
	if got == sse {
		t.Error("nothing changed")
	}
}

// TestScanRejectsAMalformedContentBlock is the second, independent gate.
func TestScanRejectsAMalformedContentBlock(t *testing.T) {
	for name, body := range map[string]string{
		"discriminator rewritten": `{"messages":[{"role":"user","content":[{"type":"This paragraph is","This":"x"}]}]}`,
		"type-named field gone":   `{"messages":[{"role":"user","content":[{"type":"text"}]}]}`,
		"no type at all":          `{"messages":[{"role":"user","content":[{"text":"x"}]}]}`,
	} {
		doc := `{"request":{"body":` + jsonQuote(body) + `}}`
		if v := Scan([]byte(doc)); len(v) == 0 {
			t.Errorf("%s: Scan accepted a malformed content block", name)
		}
	}
	ok := `{"messages":[{"role":"user","content":[{"type":"text","text":` +
		jsonQuote(SyntheticProse(12)) + `}]}]}`
	doc := `{"request":{"body":` + jsonQuote(ok) + `}}`
	if v := Scan([]byte(doc)); len(v) != 0 {
		t.Errorf("Scan rejected a well-formed substituted block: %s", v[0])
	}
}

// TestScanRejectsRecordedSSEDeltas covers the response side of the same class.
func TestScanRejectsRecordedSSEDeltas(t *testing.T) {
	stream := "event: content_block_delta\n" +
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"a recorded reply"}}` + "\n\n"
	doc := `{"response":{"body":` + jsonQuote(stream) + `}}`
	if v := Scan([]byte(doc)); len(v) == 0 {
		t.Fatal("Scan accepted an event stream carrying the model's recorded reply")
	}
	clean := `{"response":{"body":` + jsonQuote(SubstituteSSEDeltas(stream)) + `}}`
	if v := Scan([]byte(clean)); len(v) != 0 {
		t.Fatalf("Scan rejected a substituted stream: %s", v[0])
	}
}
