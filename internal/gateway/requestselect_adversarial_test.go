package gateway

import (
	"encoding/json"
	"strings"
	"testing"
)

// The selector parses attacker-influencable JSON in the REQUEST path of the one
// component that must never break the tool. Every case below is an input a
// provider or a prompt could actually produce, and the bar for all of them is the
// same: a bounded, valid, honest result -- never a panic, never a budget
// violation, never an unmarked window.

// TestJSONEscapingCannotBlowTheBudget is the arithmetic hole worth checking by
// hand rather than by reasoning. json.Marshal escapes `<`, `>` and `&` to their
// six-byte \uXXXX forms, so re-encoding a tail as a JSON string can expand it 6x.
// A message full of those characters -- an HTML paste, which is an entirely
// ordinary thing to put in a prompt -- is the input a fixed escaping allowance
// cannot survive.
//
// What this comment used to say, and why it was wrong. tailAsJSONString reserved
// 16 bytes for the re-encode, and the resulting FALLBACK was recorded here as the
// chosen answer on the grounds that "for a single-message body the tail window IS
// the newest content". That is true only of this test's own body. 65.5% of corpus
// bodies put `tools` after `messages`, so the same fallback over one of those
// stores tool boilerplate and no conversation at all -- measured on 26.5% of live
// model calls. Worst-case sizing stays rejected for the reason it always was: it
// would divide the kept tail by six on every ordinary body to serve the
// pathological one. What ships instead is a measure-and-rescale encode, so these
// bodies now SELECT rather than fall back, and the case that tells those two
// outcomes apart is TestAnEscapingHeavyNewestMessageStillStoresConversation.
func TestJSONEscapingCannotBlowTheBudget(t *testing.T) {
	for _, tc := range []struct{ name, filler string }{
		{"angle brackets", "<"},
		{"ampersands", "&"},
		{"mixed html", "<a&b>"},
		{"quotes and backslashes", `"\`},
		{"control characters", "\x01\x02"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// One message, far over budget, made entirely of expanding bytes.
			payload, err := json.Marshal(strings.Repeat(tc.filler, 200_000))
			if err != nil {
				t.Fatalf("marshal filler: %v", err)
			}
			body := `{"model":"m","messages":[{"role":"user","content":` + string(payload) + `}]}`

			got := selectModelCallRequest(body)

			if len(got) > selectionBudget {
				t.Errorf("selection returned %d bytes, over the %d budget: re-encoding expanded the "+
					"tail past its allowance and nothing caught it", len(got), selectionBudget)
			}
			if strings.HasPrefix(got, markerPrefix) {
				return // the honest fallback; bounded and marked, which is the requirement
			}
			var doc any
			if err := json.Unmarshal([]byte(got), &doc); err != nil {
				t.Errorf("selection produced invalid JSON: %v", err)
			}
		})
	}
}

// TestPathologicalStructuresAreBoundedAndValid the bind is one key name on a
// top-level object, so everything here should either select or fall back -- and
// each case has broken a hand-rolled selector somewhere.
func TestPathologicalStructuresAreBoundedAndValid(t *testing.T) {
	deep := strings.Repeat(`{"a":`, 4000) + "1" + strings.Repeat("}", 4000)
	manyKeys := make([]string, 0, 5000)
	for i := range 5000 {
		manyKeys = append(manyKeys, `"k`+strings.Repeat("x", 4)+string(rune('a'+i%26))+`":1`)
	}

	for _, tc := range []struct{ name, body string }{
		{"messages is null", `{"model":"m","messages":null}`},
		{"a message is null", `{"model":"m","messages":[null,{"role":"user","content":"hi"}]}`},
		{"messages is a string", `{"model":"m","messages":"not an array"}`},
		{"top level is an array", `[{"role":"user","content":"hi"}]`},
		{"top level is a scalar", `42`},
		{"top level is null", `null`},
		{"empty object", `{}`},
		{"empty body", ``},
		{"whitespace only", "   \n\t  "},
		{"duplicate messages keys", `{"messages":[{"role":"user","content":"first"}],"messages":[{"role":"user","content":"LAST"}]}`},
		{"duplicate model keys", `{"model":"a","model":"b","messages":[{"role":"user","content":"hi"}]}`},
		{"deeply nested value", `{"model":` + deep + `,"messages":[{"role":"user","content":"hi"}]}`},
		{"deeply nested message", `{"messages":[` + deep + `]}`},
		{"thousands of top-level keys", `{` + strings.Join(manyKeys, ",") + `,"messages":[{"role":"user","content":"hi"}]}`},
		{"huge number", `{"model":1e400,"messages":[{"role":"user","content":"hi"}]}`},
		{"invalid utf8 in content", `{"model":"m","messages":[{"role":"user","content":"a` + "\xff\xfe" + `b"}]}`},
		{"lone surrogate escape", `{"model":"m","messages":[{"role":"user","content":"\ud800"}]}`},
		{"nul byte in content", "{\"model\":\"m\",\"messages\":[{\"role\":\"user\",\"content\":\"a\x00b\"}]}"},
		{"messages key with unicode escape", `{"messages":[{"role":"user","content":"ESCAPED_KEY"}]}`},
		{"system is a number", `{"model":"m","system":12345,"messages":[{"role":"user","content":"hi"}]}`},
		{"system is null", `{"model":"m","system":null,"messages":[{"role":"user","content":"hi"}]}`},
		{"trailing garbage", `{"model":"m","messages":[]} trailing`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The bar is: it returns. A panic here is a relayed request that never
			// reaches the provider.
			got := selectModelCallRequest(tc.body)

			if len(got) > selectionBudget {
				t.Errorf("returned %d bytes, over the %d budget", len(got), selectionBudget)
			}
			if got == "" {
				if tc.body == "" {
					return // an empty body has no evidence to store
				}
				t.Errorf("a non-empty body selected to nothing at all; that is an unmarked total loss")
			}
			if strings.HasPrefix(got, markerPrefix) {
				return // marked fallback: bounded and honest
			}
			// json.Valid, not Unmarshal into `any`, and the difference is the point.
			// JSON has no number range, so `1e400` is valid JSON that Go cannot bind
			// to a float64. The selector carries `model` as a json.RawMessage and
			// re-emits it verbatim, which is right: storing a value DIFFERENT from
			// the one the provider was sent would be worse evidence than storing an
			// awkward one. Asserting Go-bindability here would have been asserting a
			// property of Go, not of the output.
			if !json.Valid([]byte(got)) {
				t.Errorf("selection produced invalid JSON:\n%q", got[:min(len(got), 300)])
			}
		})
	}
}

// TestAnUnbindableNumberIsPreservedVerbatim the corollary of the note above,
// asserted rather than assumed.
func TestAnUnbindableNumberIsPreservedVerbatim(t *testing.T) {
	got := selectModelCallRequest(`{"model":1e400,"messages":[{"role":"user","content":"hi"}]}`)

	if !strings.Contains(got, "1e400") {
		t.Errorf("a number Go cannot bind was altered or dropped; the stored request must be what "+
			"the provider was sent: %q", got)
	}
	if !json.Valid([]byte(got)) {
		t.Errorf("output is not valid JSON: %q", got)
	}
}

// TestDuplicateMessagesKeysTakeTheOneTheProviderWillUse encoding/json keeps the
// LAST duplicate key, and so does every provider parser that matters. Storing the
// first would mean the evidence describes a conversation the model never saw.
func TestDuplicateMessagesKeysTakeTheOneTheProviderWillUse(t *testing.T) {
	got := selectModelCallRequest(
		`{"messages":[{"role":"user","content":"FIRST"}],"messages":[{"role":"user","content":"LAST"}]}`)

	if strings.Contains(got, "FIRST") {
		t.Errorf("stored the FIRST duplicate messages key; the provider uses the last, so the "+
			"evidence would describe a conversation the model never saw: %q", got)
	}
	if !strings.Contains(got, "LAST") {
		t.Errorf("stored neither duplicate: %q", got)
	}
}

// TestSelectionNeverPanics belt across the whole surface, because a panic in this
// function is a model call that does not happen.
// TestAnOversizedModelDoesNotCostTheConversation `model` was copied verbatim
// because a model id is short. It is short in every honest request, and this
// selector's entire subject is the request that is not honest.
//
// Uncapped, one oversized `model` drives keepNewestMessages' `room` negative, so
// no message fits, the belt fires, and the window is over a document that field
// dominates. A trivially small turn is then absent from all 49,152 stored bytes:
// the "stores boilerplate instead of conversation" defect, reached through the one
// top-level field that had no bound.
func TestAnOversizedModelDoesNotCostTheConversation(t *testing.T) {
	const newest = "SHOULD_SURVIVE_AN_OVERSIZED_MODEL"
	body := `{"model":"` + strings.Repeat("m", 60_000) + `","messages":[{"role":"user","content":"` + newest + `"}]}`

	got := selectModelCallRequest(body)

	if len(got) > selectionBudget {
		t.Errorf("selection returned %d bytes, over the %d budget", len(got), selectionBudget)
	}
	if !strings.Contains(got, newest) {
		t.Errorf("a 16-byte turn was lost to a 60 KB model field; %d bytes stored and none of "+
			"them the conversation", len(got))
	}
}

// TestASubstitutedTailIsReportedInTheNote a single over-budget message is
// rewritten into a marked string: its role and content object are gone. The
// element says so, but the DOCUMENT must too -- otherwise the rewrite counts as
// len(keep) == len(messages), takes the "nothing was dropped" exit, and the note
// is absent from a document that lost most of a message. Loss is acceptable;
// unreported loss is what this file exists to prevent.
func TestASubstitutedTailIsReportedInTheNote(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"user","content":"` + strings.Repeat("q", 200_000) + `"}]}`

	got := selectModelCallRequest(body)
	if strings.HasPrefix(got, markerPrefix) {
		t.Fatalf("expected a selected document, got a fallback: %q", got[:min(len(got), 120)])
	}

	var doc struct {
		Selection *selectionNote `json:"openbox_selection"`
	}
	if err := json.Unmarshal([]byte(got), &doc); err != nil {
		t.Fatalf("not valid JSON: %v", err)
	}
	if doc.Selection == nil {
		t.Fatal("the document reports no loss at all, yet its only message was replaced by a " +
			"marked string; a reader is told nothing went")
	}
	if doc.Selection.OriginalBytes != len(body) {
		t.Errorf("original_bytes = %d, want the body's %d", doc.Selection.OriginalBytes, len(body))
	}
}

// TestAMarkerTooLargeForTheBudgetIsStillBounded the decode step's own markers can
// have no body after them -- the reason names an unusable Content-Encoding and
// nothing follows. Such a marker still has to be bounded here, because the only
// bound left downstream is capRunes, which head-cuts and would take the closing
// bracket and the explanation with it.
func TestAMarkerTooLargeForTheBudgetIsStillBounded(t *testing.T) {
	huge := markerPrefix + "not captured; the body was content-encoded " + strings.Repeat("z", 90_000) + "]"

	got := rewindowMarkedBody(huge)

	if len(got) > selectionBudget {
		t.Errorf("a %d-byte marker with no payload passed through at %d bytes, over the %d "+
			"budget; capRunes would head-cut it and invalidate its own claim",
			len(huge), len(got), selectionBudget)
	}
	if !strings.HasPrefix(got, markerPrefix) {
		t.Errorf("the bound took the marker prefix off: %q", got[:min(len(got), 80)])
	}
}

func TestSelectionNeverPanics(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("selectModelCallRequest panicked: %v", r)
		}
	}()
	for _, body := range []string{
		"", "{", "}", "[", `{"messages":`, `{"messages":[`, `{"messages":[{`,
		strings.Repeat("{", 10000), strings.Repeat(`"`, 10000),
		`{"messages":[` + strings.Repeat(`{"a":1},`, 50_000) + `{"a":1}]}`,
		"\x00\x00\x00", "\xff\xff\xff",
	} {
		_ = selectModelCallRequest(body)
	}
}
