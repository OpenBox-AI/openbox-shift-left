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

// TestSubstitutePromptTextPreservesRuneGeometry is the property the fixture scrub
// rests on: a soak test asserting an oversized body, and a replay test asserting
// byte counts, both stop meaning what they say if a substitution resizes the body.
func TestSubstitutePromptTextPreservesRuneGeometry(t *testing.T) {
	got := SubstitutePromptText(recordedBody)
	if a, b := utf8.RuneCountInString(got), utf8.RuneCountInString(recordedBody); a != b {
		t.Errorf("body is %d runes after substitution, was %d", a, b)
	}
	if !json.Valid([]byte(got)) {
		t.Fatal("substitution produced invalid JSON")
	}
}

// TestSubstitutePromptTextRemovesRecordedProse names the whole point: no recorded
// free text reaches disk.
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
// reviewable: a second pass must produce the same bytes, or every regeneration is
// an unreadable diff.
func TestSubstitutePromptTextIsIdempotent(t *testing.T) {
	once := SubstitutePromptText(recordedBody)
	if twice := SubstitutePromptText(once); twice != once {
		t.Error("a second substitution changed the body")
	}
}

// TestSubstitutePromptTextLeavesNonRequestJSONAlone keeps the rule from firing on
// a document that is not a model-call request, where it would destroy evidence.
func TestSubstitutePromptTextLeavesNonRequestJSONAlone(t *testing.T) {
	other := `{"resourceLogs":[{"body":{"stringValue":"an OTLP log line"}}]}`
	if got := SubstitutePromptText(other); got != other {
		t.Errorf("substitution rewrote a non-request document:\n got %s\nwant %s", got, other)
	}
}

// TestScanRejectsRecordedPromptText is the gate that makes the substitution
// permanent. Without it the rule lives only in the generator, and a fixture
// hand-edited or produced by an older extractor sails through.
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

// TestScanRejectsASystemReminder names the leak vector itself. The provider
// injects the developer's global configuration into the first prompt inside this
// tag, which is how a third party's private file reached a committed fixture.
func TestScanRejectsASystemReminder(t *testing.T) {
	doc := `{"note":"` + "<system-reminder>anything at all</system-reminder>" + `"}`
	if v := Scan([]byte(doc)); len(v) == 0 {
		t.Fatal("Scan accepted a document carrying a system-reminder block")
	}
}

// TestScanRejectsRecordedTelemetryContent covers the same class on the other
// lane: the OTLP corpus carries prompt and response bodies as attribute values.
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

// TestSanitizeSubstitutesRecordedContent holds the generator half: Sanitize must
// produce exactly what Scan admits, or every regeneration fails its own gate.
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
