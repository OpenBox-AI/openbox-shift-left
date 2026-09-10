package client

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/openbox-ai/openbox-shift-left/internal/conformance"
)

// This file carries the invariants that outlived the span carrier.
//
// Model-call content used to ride `spans[]`, and core parses that array on the
// normal path and then discards it: persistence is gated on `hook_trigger` plus
// a pre-existing event row, which this client deliberately never sets. So every
// body ever sent there was parsed, used in memory, and dropped, and the tests
// asserting it reached the wire were all true and all pointless. The carrier is
// gone; these are the properties that were never about the carrier.
//
// Every assertion here is against a consumer that fails SILENTLY. Core logs and
// returns "" for content it cannot read, so a wrong shape does not error
// anywhere -- it leaves the Goal Alignment widgets empty exactly as they were.

// modelCallEvent is one relayed call as the in-path lanes now build it: the
// bodies on the DevEvent's span (the local carrier, which the content gate
// already clears), the timing on the event.
func modelCallEvent(eventType EventType, requestBody, responseBody string) DevEvent {
	ev := DevEvent{
		SchemaVersion:  SchemaVersion,
		EventID:        "ev-" + string(eventType),
		EventType:      eventType,
		SessionID:      "sess-turn",
		DeveloperDID:   "did:aip:7f3c9b2e-0000-5000-a000-000000000001",
		Timestamp:      "2026-08-13T10:00:12Z",
		StartedAt:      "2026-08-13T10:00:00Z",
		Tool:           Tool{Name: "claude-code", Kind: ToolShell},
		ActivityType:   ActivityTypeLLMCompletion,
		ProxyRequestID: "px-abc123",
		Span: &Span{
			SemanticType:          ActivityTypeLLMCompletion,
			Stage:                 "started",
			HTTPMethod:            "POST",
			HTTPURL:               "https://api.anthropic.com/v1/messages",
			CredentialFingerprint: "a1b2c3d4e5f60718",
			RequestBody:           requestBody,
		},
	}
	if eventType == EventTurnCompleted {
		ev.Span.Stage = "completed"
		ev.Span.HTTPStatus = 200
		ev.Span.ResponseBody = responseBody
		ev.EndedAt = "2026-08-13T10:00:12Z"
	}
	return ev
}

// wireOf marshals the payload the client would POST, which is the only level
// worth asserting: a struct field is not a wire field, and `encoding/json` drops
// a key the receiving type does not recognize without a word.
func wireOf(t *testing.T, ev DevEvent) map[string]any {
	t.Helper()
	raw, err := buildPayload(ev)
	if err != nil {
		t.Fatalf("buildPayload: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("payload is not JSON: %v", err)
	}
	return m
}

func nestedString(t *testing.T, m map[string]any, field, key string) string {
	t.Helper()
	raw, present := m[field]
	if !present {
		return ""
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("%s is not an object: %T", field, raw)
	}
	s, _ := obj[key].(string)
	return s
}

// TestTheRequestBodyRidesActivityInput is acceptance criterion 2, and the field
// NAME is the assertion. Goal Alignment reads its judgeable operation out of
// activity_input, and capOperationInput orders keys by a fixed priority list,
// appends unlisted keys alphabetically, then breaks on the first overflow and
// discards everything after it. Under `request_body` the one field carrying what
// the call did sorts fourth and is the first thing dropped; `content` is in the
// priority list.
func TestTheRequestBodyRidesActivityInput(t *testing.T) {
	const body = `{"model":"claude-opus-4-8","messages":[{"role":"user","content":"hello"}]}`
	m := wireOf(t, modelCallEvent(EventTurnStarted, body, ""))

	if got := nestedString(t, m, "activity_input", "content"); got != body {
		t.Errorf("activity_input.content = %q, want the request body", got)
	}
	if _, present := m["activity_input"].(map[string]any)["request_body"]; present {
		t.Error("the body is under `request_body`, which is unlisted in the judge's key " +
			"priority and is therefore the first key its cap discards")
	}
}

// TestTheResponseBodyRidesActivityOutput is acceptance criterion 1: the field
// core stores as the row's `output`, rather than a span it throws away.
func TestTheResponseBodyRidesActivityOutput(t *testing.T) {
	const reply = `{"type":"message","role":"assistant","content":[{"type":"text","text":"hi"}]}`
	m := wireOf(t, modelCallEvent(EventTurnCompleted, "{}", reply))

	if got := nestedString(t, m, "activity_output", "content"); got != reply {
		t.Errorf("activity_output.content = %q, want the response body", got)
	}
}

// TestNoEventCarriesASpan the carrier is gone, and both producers went with it:
// the observed-exchange span and the synthesized assistant span. omitempty must
// actually elide the keys, or core sees an empty array where it saw none.
func TestNoEventCarriesASpan(t *testing.T) {
	events := []DevEvent{
		modelCallEvent(EventTurnStarted, "{}", ""),
		modelCallEvent(EventTurnCompleted, "{}", "{}"),
	}
	// A hook turn too. Both producers go, not just the in-path one.
	idx := 3
	hook := DevEvent{
		SchemaVersion: SchemaVersion,
		EventID:       "ev-hook",
		EventType:     EventTurnCompleted,
		SessionID:     "sess-hook",
		DeveloperDID:  "did:aip:7f3c9b2e-0000-5000-a000-000000000001",
		Timestamp:     "2026-08-13T10:00:12Z",
		Tool:          Tool{Name: "claude-code", Kind: ToolShell},
		TurnIndex:     &idx,
		Model:         "claude-opus-4-8",
		Content:       &Content{Output: "the assistant's reply", Thinking: "reasoning"},
	}
	events = append(events, hook)

	for _, ev := range events {
		raw, err := buildPayload(ev)
		if err != nil {
			t.Fatalf("buildPayload: %v", err)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		if _, present := m["spans"]; present {
			t.Errorf("%s carries a `spans` key; the activity IS the span for a model call, "+
				"so nesting one inside it represents the call twice", ev.EventID)
		}
		if _, present := m["span_count"]; present {
			t.Errorf("%s carries `span_count`", ev.EventID)
		}
	}
}

// TestNoPayloadCarriesHookTrigger is the rehomed guard, and it is the one
// assertion in this file that has nothing to do with model calls.
//
// It lived in turnspan_test.go, which died with its subject, and it was the ONLY
// thing keeping hook_trigger off the payload. Setting it would unlock span
// persistence in core -- and it would do so by putting a model turn on core's
// approval-bypass fingerprint path, which a turn is not an approvable operation
// for. The invariant outlives the span by a long way, so this is a census over
// every event type rather than a check on one.
func TestNoPayloadCarriesHookTrigger(t *testing.T) {
	idx := 1
	for _, et := range AllEventTypes {
		ev := DevEvent{
			SchemaVersion: SchemaVersion,
			EventID:       "ev-" + string(et),
			EventType:     et,
			SessionID:     "sess-census",
			DeveloperDID:  "did:aip:7f3c9b2e-0000-5000-a000-000000000001",
			Timestamp:     "2026-08-13T10:00:12Z",
			StartedAt:     "2026-08-13T10:00:00Z",
			EndedAt:       "2026-08-13T10:00:12Z",
			Tool:          Tool{Name: "Bash", Kind: ToolShell},
			TurnIndex:     &idx,
			Model:         "claude-opus-4-8",
			Status:        StatusCompleted,
			Span: &Span{
				SemanticType: "internal",
				Stage:        "completed",
				RequestBody:  "in",
				ResponseBody: "out",
			},
			Content: &Content{
				Prompt: "p", Output: "o", FileText: "f", ToolInput: "ti",
				ToolOutput: "to", SignalDetail: "sd", Thinking: "th",
			},
			Metadata: map[string]any{"commit_sha": "abc", "repo": "r", "deploy_id": "d"},
		}
		raw, err := buildPayload(ev)
		if err != nil {
			continue // an event type with no wire mapping cannot carry the key either
		}
		if strings.Contains(string(raw), "hook_trigger") {
			t.Errorf("%s: the payload carries hook_trigger; a model turn must never enter "+
				"core's approval-bypass fingerprint path", et)
		}
	}
}

// TestContentCaptureOffLeavesNoBodyInEitherNewHome is the gate, on the outbound
// bytes, against BOTH new homes. A carrier change that forgot the gate would be
// a content leak with no test failing.
//
// Extended for acceptance criterion 5: activity_output.openbox_capture is
// attached inside the SAME Span.ResponseBody arm that content lives in
// (turnActivityOutput), so stripContent emptying ResponseBody must remove the
// note along with the body it describes -- not just the body, leaving a
// truncated:false note claiming a complete reply that was never stored.
func TestContentCaptureOffLeavesNoBodyInEitherNewHome(t *testing.T) {
	const requestMarker = "MARKER_REQUEST_BODY"
	const responseMarker = "MARKER_RESPONSE_BODY"

	for _, et := range []EventType{EventTurnStarted, EventTurnCompleted} {
		ev := modelCallEvent(et, requestMarker, responseMarker)
		raw, err := buildPayload(stripContent(ev))
		if err != nil {
			t.Fatalf("buildPayload: %v", err)
		}
		got := string(raw)
		if strings.Contains(got, requestMarker) || strings.Contains(got, responseMarker) {
			t.Errorf("%s: a body egressed with content capture OFF: %s", et, got)
		}
		if !strings.Contains(got, "a1b2c3d4e5f60718") {
			t.Errorf("%s: the credential fingerprint disappeared under the content gate. It is "+
				"one-way derived evidence, not content, and a privacy setting must not let an "+
				"org opt out of being identified", et)
		}
		var payload map[string]any
		if err := json.Unmarshal(raw, &payload); err != nil {
			t.Fatalf("%s: payload is not JSON: %v", et, err)
		}
		if ao, ok := payload["activity_output"].(map[string]any); ok {
			if _, present := ao["content"]; present {
				t.Errorf("%s: activity_output.content present with content capture off", et)
			}
			if _, present := ao["openbox_capture"]; present {
				t.Errorf("%s: activity_output.openbox_capture present with content capture off; "+
					"the note must disappear along with the body it describes, not outlive it", et)
			}
		}
	}
}

// TestTheFingerprintRidesMetadataUngated is the rehoming, and the reason it is a
// rehoming rather than a regression: the fingerprint used to travel in a span's
// attributes, and spans never persisted for a developer session, so account
// binding has in fact never had anything to match on. Metadata does persist.
func TestTheFingerprintRidesMetadataUngated(t *testing.T) {
	for _, contentOn := range []bool{true, false} {
		ev := modelCallEvent(EventTurnCompleted, "{}", "{}")
		if !contentOn {
			ev = stripContent(ev)
		}
		m := wireOf(t, ev)
		if got := nestedString(t, m, "metadata", "credential_fingerprint"); got != "a1b2c3d4e5f60718" {
			t.Errorf("content capture %v: metadata.credential_fingerprint = %q", contentOn, got)
		}
	}
}

// TestTheFingerprintIsNotAContentMetadataKey is the structural half of the
// property above: contentMetadataKeys is what strips a metadata key under the
// gate, so membership there would silently re-gate the fingerprint.
func TestTheFingerprintIsNotAContentMetadataKey(t *testing.T) {
	if contentMetadataKeys["credential_fingerprint"] {
		t.Error("credential_fingerprint is in contentMetadataKeys, so the content gate now " +
			"strips derived governance evidence")
	}
}

// TestTheAttributionFourKeysRideMetadataUngated is the rehoming for the four
// correlation fields the in-path lanes parse out of Claude Code's own
// attribution block: like the fingerprint, they are derived evidence rather
// than content, so they must survive content_capture:false.
func TestTheAttributionFourKeysRideMetadataUngated(t *testing.T) {
	for _, contentOn := range []bool{true, false} {
		ev := modelCallEvent(EventTurnCompleted, "{}", "{}")
		ev.Span.PromptID = "018f1a2b-0000-7000-8000-000000000002"
		ev.Span.PreviousRequestID = "req_prior"
		ev.Span.IsSubagent = true
		ev.Span.Entrypoint = "cli"
		if !contentOn {
			ev = stripContent(ev)
		}
		m := wireOf(t, ev)
		meta, ok := m["metadata"].(map[string]any)
		if !ok {
			t.Fatalf("content capture %v: no metadata on a completed relayed call", contentOn)
		}
		if got := meta["prompt_id"]; got != "018f1a2b-0000-7000-8000-000000000002" {
			t.Errorf("content capture %v: metadata.prompt_id = %v", contentOn, got)
		}
		if got := meta["previous_request_id"]; got != "req_prior" {
			t.Errorf("content capture %v: metadata.previous_request_id = %v", contentOn, got)
		}
		if got, _ := meta["is_subagent"].(bool); !got {
			t.Errorf("content capture %v: metadata.is_subagent = %v, want true",
				contentOn, meta["is_subagent"])
		}
		if got := meta["entrypoint"]; got != "cli" {
			t.Errorf("content capture %v: metadata.entrypoint = %v", contentOn, got)
		}
	}
}

// TestTheAttributionKeysAreNotContentMetadataKeys is the structural half:
// contentMetadataKeys is what strips a metadata key under the gate, so
// membership there would silently re-gate derived governance evidence.
func TestTheAttributionKeysAreNotContentMetadataKeys(t *testing.T) {
	for _, k := range []string{"prompt_id", "previous_request_id", "is_subagent", "entrypoint"} {
		if contentMetadataKeys[k] {
			t.Errorf("%s is in contentMetadataKeys, so the content gate would strip it", k)
		}
	}
}

// TestIsSubagentNeverRidesMetadataAsFalse is acceptance criterion 2 at the
// wire boundary: a call whose source carried no cc_is_subagent entry (which
// collapses to Span.IsSubagent's zero value, false) must ship NO
// metadata.is_subagent key at all. A stored `false` would read to a consumer
// as "confirmed not a subagent", which is a claim this lane never has grounds
// to make.
func TestIsSubagentNeverRidesMetadataAsFalse(t *testing.T) {
	ev := modelCallEvent(EventTurnCompleted, "{}", "{}") // Span.IsSubagent left at its zero value
	m := wireOf(t, ev)
	meta, ok := m["metadata"].(map[string]any)
	if !ok {
		t.Fatal("no metadata on a completed relayed call")
	}
	if v, present := meta["is_subagent"]; present {
		t.Errorf("metadata.is_subagent = %v present with no source entry; want the key absent", v)
	}
}

// TestSchemaAcceptsTheAttributionFields is acceptance criterion 5, mirroring
// TestSchemaAcceptsRunIdentityFields (contract_v18_test.go): a client.Span
// carrying all four new fields must pass conformance.ValidateDevEvent, or
// $defs/span's additionalProperties:false rejects every in-path event.
func TestSchemaAcceptsTheAttributionFields(t *testing.T) {
	ev := baseValidEvent()
	ev.EventType = EventToolResult
	ev.Span = &Span{
		SemanticType:      "internal",
		Stage:             "completed",
		PromptID:          "018f1a2b-0000-7000-8000-000000000003",
		PreviousRequestID: "req_prior",
		IsSubagent:        true,
		Entrypoint:        "cli",
	}
	if err := conformance.ValidateDevEvent(devEventJSON(t, ev), false); err != nil {
		t.Errorf("a valid attribution fixture failed schema validation: %v", err)
	}
}

// TestObservedHeadersDoNotEgress they only ever reached core inside spans[],
// which is discarded, so nothing has ever read them -- and they were the highest
// risk class this client carried, because the developer's live provider
// credential is on every model request. Capture still redacts them by key name;
// they simply no longer leave the machine.
func TestObservedHeadersDoNotEgress(t *testing.T) {
	ev := modelCallEvent(EventTurnCompleted, "{}", "{}")
	ev.Span.RequestHeaders = map[string]string{"Anthropic-Version": "2023-06-01", "Authorization": "[redacted]"}
	ev.Span.ResponseHeaders = map[string]string{"Request-Id": "req_upstream_1"}

	raw, err := buildPayload(ev)
	if err != nil {
		t.Fatalf("buildPayload: %v", err)
	}
	for _, marker := range []string{"Anthropic-Version", "request_headers", "response_headers", "req_upstream_1"} {
		if strings.Contains(string(raw), marker) {
			t.Errorf("observed headers reached the wire (%q): %s", marker, raw)
		}
	}
}

// TestRequestBodyTruncationKeepsTheTail is the direction, and it is the opposite
// of every other cap here. A /v1/messages request body is a conversation array
// whose newest turn is at the END; head-truncating one stores the system prompt
// and the boilerplate and discards the actual user turn -- keeping precisely the
// part that does not change between calls.
//
// Read what this test does NOT prove, because for a long time it was read as
// proving it. It exercises capModelCallRequest in ISOLATION, and the wired path
// never reached the branch below: upstream, capturableBody head-cut 256 KiB and
// capRunes head-cut 65,536 RUNES, so what arrived here was already exactly
// 65,536 bytes -- at the cap, not over it -- and the function returned its input
// unchanged. Every stored body was the tail of the head, identical across calls,
// and this test stayed green throughout. The tail direction is still correct and
// still worth pinning; it is now the fallback path's cap rather than the normal
// one, because internal/gateway selects the newest messages before this layer
// sees the body. TestTheWiredPathDoesNotReachTheTruncationBranch below is the
// assertion that was missing.
func TestRequestBodyTruncationKeepsTheTail(t *testing.T) {
	const newest = "THE_NEWEST_TURN"
	body := strings.Repeat("x", maxModelCallBodyBytes) + newest

	got := capModelCallRequest(body)
	if len(got) > maxModelCallBodyBytes {
		t.Errorf("kept %d bytes, want at most %d", len(got), maxModelCallBodyBytes)
	}
	if !strings.HasSuffix(got, newest) {
		t.Error("the newest turn was truncated away; a request body must keep its tail")
	}
}

// TestTheWiredPathDoesNotReachTheTruncationBranch closes the loop the test above
// left open: a selected document passes this cap untouched -- no truncation mark,
// no cut, byte-identical.
//
// What this test does NOT do, deliberately, is police the gateway's budget. An
// earlier version duplicated `48 * 1024` here as a local const and claimed to go
// red if the gateway's budget ever reached this cap. It would not have: it reds
// only if someone edits the copy too, so it passed whether or not the invariant
// held -- the same shape of placebo as the test above it. The real guard lives in
// internal/gateway, asserting `selectionBudget < client.MaxModelCallBodyBytes`
// against the exported constant, in the package that can see both numbers.
//
// So this asserts the local half only: a document sized like a selected one is
// not cut here.
func TestTheWiredPathDoesNotReachTheTruncationBranch(t *testing.T) {
	selected := `{"model":"claude-opus-5","openbox_selection":{"dropped_messages":198,"original_bytes":541631},` +
		`"messages":[{"role":"user","content":"` +
		strings.Repeat("m", 48*1024-160) + `THE_NEWEST_TURN"}]}`

	got := capModelCallRequest(selected)

	if got != selected {
		t.Errorf("a selected document was altered by the cap: %d bytes in, %d out", len(selected), len(got))
	}
	if strings.Contains(got, truncationMark) {
		t.Error("a selected document was marked as truncated; the wired path must not reach that branch")
	}
	if !strings.HasSuffix(got, `THE_NEWEST_TURN"}]}`) {
		t.Error("the newest turn is not at the end of the stored body")
	}
}

// TestResponseBodyTruncationKeepsTheHead the other direction, for the same
// reason in reverse: a reply starts at its beginning.
func TestResponseBodyTruncationKeepsTheHead(t *testing.T) {
	const first = "THE_START_OF_THE_REPLY"
	body := first + strings.Repeat("y", maxModelCallBodyBytes)

	got := capModelCallBody(body)
	if len(got) > maxModelCallBodyBytes {
		t.Errorf("kept %d bytes, want at most %d", len(got), maxModelCallBodyBytes)
	}
	if !strings.HasPrefix(got, first) {
		t.Error("the start of the reply was truncated away")
	}
}

// TestBothCapsAreMeasuredInBytesAndKeepRunesWhole is the unit check, and it found
// a real one: capBody, which every other content field uses, tests a BYTE length
// and then cuts RUNES, so a 65,536-rune CJK body passes its guard on the second
// branch and is not truncated at all -- 192 KB on the wire from a bound that
// reads like 64 KB. Correct for a field whose contract is stated in characters,
// wrong for a bound that exists to manage the cost of synchronous evaluation.
// These two caps therefore bound bytes, and this asserts it in that unit.
func TestBothCapsAreMeasuredInBytesAndKeepRunesWhole(t *testing.T) {
	// Three bytes per rune, so byte length is 3x the rune count.
	wide := strings.Repeat("測", maxModelCallBodyBytes)
	if len(wide) <= maxModelCallBodyBytes {
		t.Fatalf("fixture is not over the byte bound: %d bytes", len(wide))
	}

	for name, got := range map[string]string{
		"request (tail)":  capModelCallRequest(wide),
		"response (head)": capModelCallBody(wide),
	} {
		if len(got) > maxModelCallBodyBytes {
			t.Errorf("%s: kept %d bytes, want at most %d", name, len(got), maxModelCallBodyBytes)
		}
		if !utf8.ValidString(got) {
			t.Errorf("%s: truncation split a multi-byte rune", name)
		}
		if strings.ContainsRune(got, utf8.RuneError) {
			t.Errorf("%s: truncation left a replacement character", name)
		}
	}
}

// TestCapBodyStillCutsRunesForEveryOtherField is the control on the change
// above: the two model-call caps became byte bounds, and nothing else did.
// `thinking` and the tool bodies keep the character-count contract they ship
// under, and maxThinkingBytes must stay larger than this.
func TestCapBodyStillCutsRunesForEveryOtherField(t *testing.T) {
	wide := strings.Repeat("測", maxBodySize)
	if got := capBody(wide); got != wide {
		t.Errorf("capBody truncated a %d-rune value; its contract is stated in characters, "+
			"and changing it here would change what every hook event ships", len([]rune(wide)))
	}
}

// TestTheModelCallDurationReachesTheWire is acceptance criterion 4. The relay
// measured the call and discarded it; every stored in-path row had a null
// duration while api_response_ms -- the CONTROL PLANE's own response time -- sat
// next to it and is easily mistaken for the model's.
func TestTheModelCallDurationReachesTheWire(t *testing.T) {
	m := wireOf(t, modelCallEvent(EventTurnCompleted, "{}", "{}"))
	got, ok := m["duration_ms"].(float64)
	if !ok {
		t.Fatalf("duration_ms = %v (%T), want a number", m["duration_ms"], m["duration_ms"])
	}
	if want := 12000.0; got != want {
		t.Errorf("duration_ms = %v, want %v", got, want)
	}
}

// TestBothHalvesShareOneActivityID is what makes them one timeline row, and what
// core's completion-to-start lookup keys on.
func TestBothHalvesShareOneActivityID(t *testing.T) {
	started := wireOf(t, modelCallEvent(EventTurnStarted, "{}", ""))
	completed := wireOf(t, modelCallEvent(EventTurnCompleted, "{}", "{}"))

	if started["activity_id"] != completed["activity_id"] {
		t.Errorf("activity_id differs across the pair: %v vs %v",
			started["activity_id"], completed["activity_id"])
	}
	if started["activity_id"] == "" || started["activity_id"] == nil {
		t.Error("the pair has no activity_id at all, so nothing pairs it")
	}
	if started["event_type"] != "ActivityStarted" || completed["event_type"] != "ActivityCompleted" {
		t.Errorf("wire types wrong: %v / %v", started["event_type"], completed["event_type"])
	}
}

// TestTheStartedHalfCarriesNoDuration a duration on an opening half would claim
// the call was already over.
func TestTheStartedHalfCarriesNoDuration(t *testing.T) {
	m := wireOf(t, modelCallEvent(EventTurnStarted, "{}", ""))
	if v, present := m["duration_ms"]; present {
		t.Errorf("the ActivityStarted half carries duration_ms = %v", v)
	}
}

// TestAHookTurnsReplyReachesActivityOutput is the positive case for the path this
// work added rather than moved, and the one a review found untested: a HOOK-shaped
// turn has NO span at all, so the `ev.Span != nil` arm cannot cover it.
//
// It matters because deleting the assistant span left `Content.Output` with no
// consumer, and an adapter reads the transcript, redacts the text and caps it. If
// this arm regresses, that work is silently discarded and nothing fails.
func TestAHookTurnsReplyReachesActivityOutput(t *testing.T) {
	const reply = "I refactored the spool; all 11 modules are green."
	idx := 3
	hook := DevEvent{
		SchemaVersion: SchemaVersion,
		EventID:       "ev-hook-reply",
		EventType:     EventTurnCompleted,
		SessionID:     "sess-hook",
		DeveloperDID:  "did:aip:7f3c9b2e-0000-5000-a000-000000000001",
		Timestamp:     "2026-08-13T10:00:12Z",
		Tool:          Tool{Name: "claude-code", Kind: ToolShell},
		TurnIndex:     &idx,
		Model:         "claude-opus-4-8",
		Content:       &Content{Output: reply, Thinking: "reasoning about the spool"},
	}
	if hook.Span != nil {
		t.Fatal("the fixture carries a span, so it cannot exercise the hook arm")
	}

	m := wireOf(t, hook)
	if got := nestedString(t, m, "activity_output", "content"); got != reply {
		t.Errorf("activity_output.content = %q, want the assistant reply", got)
	}
	// Still separate keys: a reader that conflates chain-of-thought with the answer
	// is corrupted silently, because nothing logs when text is merely wrong text.
	if got := nestedString(t, m, "activity_output", "thinking"); got == "" {
		t.Error("thinking did not reach its own key")
	} else if got == reply {
		t.Error("thinking and the reply collapsed into one value")
	}

	// And the gate, on the same path.
	raw, err := buildPayload(stripContent(hook))
	if err != nil {
		t.Fatalf("buildPayload: %v", err)
	}
	if strings.Contains(string(raw), reply) {
		t.Errorf("the hook reply egressed with content capture OFF: %s", raw)
	}
}

// TestATruncatedBodyIsMarkedAsTruncated a clipped body must not read as a
// complete one on a field OPA and Guardrails decide against.
func TestATruncatedBodyIsMarkedAsTruncated(t *testing.T) {
	long := strings.Repeat("y", maxModelCallBodyBytes*2)

	head := capModelCallBody(long)
	if len(head) > maxModelCallBodyBytes {
		t.Errorf("response cap kept %d bytes, over the %d bound", len(head), maxModelCallBodyBytes)
	}
	if !strings.HasSuffix(head, truncationMark) {
		t.Error("a truncated response body carries no marker, so it reads as a complete reply")
	}

	tail := capModelCallRequest(long)
	if len(tail) > maxModelCallBodyBytes {
		t.Errorf("request cap kept %d bytes, over the %d bound", len(tail), maxModelCallBodyBytes)
	}
	// The marker goes on the FRONT here: a request keeps its tail, so what was
	// dropped is the start of the conversation.
	if !strings.HasPrefix(tail, truncationMark) {
		t.Error("a truncated request body carries no marker")
	}

	// An untruncated body must carry no marker at all, or every reader learns to
	// ignore it.
	if got := capModelCallBody("short"); got != "short" {
		t.Errorf("an untruncated body was marked: %q", got)
	}
}

// TestActivityTypeOutsideTheVocabularyIsRefused the column is pass-through, so a
// typo would reach the dashboard unfiltered and break every query that assumes
// the closed set, with nothing to notice it.
func TestActivityTypeOutsideTheVocabularyIsRefused(t *testing.T) {
	ev := modelCallEvent(EventTurnCompleted, "{}", "{}")
	ev.ActivityType = "llm_completionn" // one typo
	if got := activityLabel(ev); got == "llm_completionn" {
		t.Error("a value outside AllActivityTypes reached the wire's activity_type")
	}
	// And it falls back to the derived label rather than to nothing.
	if got := activityLabel(ev); got != ActivityTypeLLMCompletion {
		t.Errorf("fallback label = %q, want the derived %q", got, ActivityTypeLLMCompletion)
	}
}

// TestAProbeShipsNoJudgeableInput is the other half of the path classifier, and
// the half nothing guarded. CarriesContent() keeps a probe's BODIES off the
// wire, but the http_* pair rode activity_input regardless -- and core's primary
// alignment path is an ActivityStarted with a non-empty activity_input, so every
// token-count probe became a judgeable operation describing no work.
func TestAProbeShipsNoJudgeableInput(t *testing.T) {
	probe := modelCallEvent(EventTurnStarted, "", "")
	probe.ActivityType = ActivityTypeTokenCount
	probe.Span.SemanticType = "internal"
	probe.Span.HTTPURL = "https://api.anthropic.com/v1/messages/count_tokens"

	m := wireOf(t, probe)
	if got, present := m["activity_input"]; present {
		t.Errorf("a %s ActivityStarted ships activity_input %v; non-empty is what makes the "+
			"control plane judge it as an operation", ActivityTypeTokenCount, got)
	}
}

// TestASynthesizedCallShipsNoJudgeableInput: the telemetry lane observes no HTTP
// exchange and synthesizes "POST https://api.anthropic.com/v1/messages" to
// classify itself. That pair once carried an `openbox.span_synthetic` marker and
// rode spans[], which core discards; on activity_input it would PERSIST, and a
// fabricated observation stored as an observation is worse than none.
func TestASynthesizedCallShipsNoJudgeableInput(t *testing.T) {
	synthesized := modelCallEvent(EventTurnStarted, "", "")
	synthesized.ProxyRequestID = ""
	synthesized.OtelRequestID = "otel-req-1"

	m := wireOf(t, synthesized)
	if got, present := m["activity_input"]; present {
		t.Errorf("a lane that observed no exchange ships activity_input %v", got)
	}
}

// TestTheRelayedStatusReachesTheWire: the relay measures a response status and
// every other observed field was rehomed when the span carrier went. Without it
// a 5xx, and a call whose transport failed before any response existed, store
// identically to a success whose reply happened not to be captured.
func TestTheRelayedStatusReachesTheWire(t *testing.T) {
	done := modelCallEvent(EventTurnCompleted, "req", "resp")
	done.Span.HTTPStatus = 503

	meta, ok := wireOf(t, done)["metadata"].(map[string]any)
	if !ok {
		t.Fatal("no metadata on a completed relayed call")
	}
	got, present := meta["http_status"]
	if !present {
		t.Fatalf("http_status is absent; the relay observed 503 and nothing stores it. metadata=%v", meta)
	}
	if n, isNum := got.(float64); !isNum || int(n) != 503 {
		t.Errorf("http_status = %v, want 503", got)
	}

	// Structural, so it must survive the content gate: an org opting out of content
	// capture still needs to know which calls failed.
	stripped, ok := wireOf(t, stripContent(done))["metadata"].(map[string]any)
	if !ok {
		t.Fatal("no metadata with content capture off")
	}
	if _, present := stripped["http_status"]; !present {
		t.Error("http_status vanished with content capture off; it is a status code, not content")
	}
}

// --- Phase 04: response completeness (activity_output.openbox_capture) ---

// openboxCapture reads activity_output.openbox_capture off the wire payload,
// or nil when the key is absent. A local helper rather than nestedString
// above: the note is an OBJECT ({truncated, original_bytes}), not a string.
func openboxCapture(t *testing.T, m map[string]any) map[string]any {
	t.Helper()
	ao, ok := m["activity_output"].(map[string]any)
	if !ok {
		return nil
	}
	oc, ok := ao["openbox_capture"].(map[string]any)
	if !ok {
		return nil
	}
	return oc
}

// TestTheResponseCompletenessNoteRidesActivityOutput is acceptance criterion
// 2: a short, whole response reports truncated:false -- emitted, not omitted,
// because that IS the "this reply is complete" signal -- and original_bytes
// equal to the body's own length.
func TestTheResponseCompletenessNoteRidesActivityOutput(t *testing.T) {
	const reply = `{"type":"message","role":"assistant","content":[{"type":"text","text":"hi"}]}`
	m := wireOf(t, modelCallEvent(EventTurnCompleted, "{}", reply))

	oc := openboxCapture(t, m)
	if oc == nil {
		t.Fatal("activity_output.openbox_capture is absent for a completed call carrying a response body")
	}
	if got, ok := oc["truncated"].(bool); !ok || got {
		t.Errorf("truncated = %v, want false present (not omitted)", oc["truncated"])
	}
	if got, ok := oc["original_bytes"].(float64); !ok || int(got) != len(reply) {
		t.Errorf("original_bytes = %v, want %d", oc["original_bytes"], len(reply))
	}
}

// TestTheResponseCompletenessNoteUsesTheDecodedBodyNotTheGatewaysRawByteCount
// pins the fix for the defect this note exists to prevent. Span.ResponseBytesSeen
// counts bytes as the gateway's capture sink saw them off the wire, still
// compressed -- DisableCompression means the gateway decodes capture itself,
// and a corpus measurement puts brotli+gzip at ~98% of real replies
// (internal/gateway/decode.go) -- while ResponseBody is already decoded. A
// compressed reply's wire count is smaller than the text it decodes to, so
// taking ResponseBytesSeen at face value reported original_bytes SMALLER than
// the content it was supposed to describe: exactly backwards for a field a UI
// reads as "how much of the reply is this". original_bytes must track the
// decoded body instead, regardless of what the gateway's raw wire counter says.
func TestTheResponseCompletenessNoteUsesTheDecodedBodyNotTheGatewaysRawByteCount(t *testing.T) {
	const reply = `{"type":"message","role":"assistant","content":[{"type":"text",` +
		`"text":"a realistic decoded reply, larger than its compressed wire form"}]}`
	ev := modelCallEvent(EventTurnCompleted, "{}", reply)
	// The realistic shape for ~98% of real responses: a compressed wire count
	// smaller than the decoded text it expands to, not the exception.
	ev.Span.ResponseBytesSeen = len(reply) / 4
	ev.Span.ResponseTruncated = true

	oc := openboxCapture(t, wireOf(t, ev))
	if oc == nil {
		t.Fatal("activity_output.openbox_capture is absent")
	}
	if got, ok := oc["truncated"].(bool); !ok || !got {
		t.Errorf("truncated = %v, want true (the gateway's own sink reported a cut)", oc["truncated"])
	}
	if got, ok := oc["original_bytes"].(float64); !ok || int(got) != len(reply) {
		t.Errorf("original_bytes = %v, want the DECODED body's own length %d, not the gateway's "+
			"smaller wire-compressed count %d", oc["original_bytes"], len(reply), ev.Span.ResponseBytesSeen)
	}
}

// TestOriginalBytesIsNeverSmallerThanTheContentItDescribes is the general
// invariant the gateway's-raw-count bug violated: whatever produced
// original_bytes, it must never claim the reply was SMALLER than the content
// stored beside it -- a consumer computing "we stored X of Y bytes" cannot
// make sense of Y < X.
func TestOriginalBytesIsNeverSmallerThanTheContentItDescribes(t *testing.T) {
	long := strings.Repeat("y", maxModelCallBodyBytes+10)
	cases := []struct {
		name string
		ev   DevEvent
	}{
		{"short and uncut", modelCallEvent(EventTurnCompleted, "{}", `{"ok":true}`)},
		{"cut by this client's own cap", modelCallEvent(EventTurnCompleted, "{}", long)},
		{"gateway reports a smaller compressed wire count", func() DevEvent {
			ev := modelCallEvent(EventTurnCompleted, "{}", `{"type":"message","content":"decoded text"}`)
			ev.Span.ResponseBytesSeen = 3 // far smaller than the decoded body
			return ev
		}()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := wireOf(t, tc.ev)
			content := nestedString(t, m, "activity_output", "content")
			oc := openboxCapture(t, m)
			if oc == nil {
				t.Fatal("activity_output.openbox_capture is absent")
			}
			originalBytes, ok := oc["original_bytes"].(float64)
			if !ok {
				t.Fatalf("original_bytes is not a number: %v", oc["original_bytes"])
			}
			if int(originalBytes) < len(content) {
				t.Errorf("original_bytes = %d, smaller than the %d bytes of content it describes",
					int(originalBytes), len(content))
			}
		})
	}
}

// TestOriginalBytesForAnUndecodableMarkerIsItsOwnHonestLength documents the
// choice for the case the gateway cannot recover a decoded byte count for at
// all: when a compressed stream failed to decode (corrupt, or cut mid-stream
// by the sink's own bound before decoding ever ran), gateway/decode.go stores
// a marker string in place of the reply, and the true original size is
// genuinely unknowable -- this layer must not invent one. The marker's own
// length is a trivially honest lower bound (it can never be smaller than the
// content it describes, because it always IS that content), and no fabricated
// estimate -- such as the gateway's raw, wrong-unit wire count -- is reported
// instead.
func TestOriginalBytesForAnUndecodableMarkerIsItsOwnHonestLength(t *testing.T) {
	const marker = `[openbox: not captured; the br body could not be decoded ` +
		`(truncated or corrupt), so redaction could not inspect it]`
	ev := modelCallEvent(EventTurnCompleted, "{}", marker)
	ev.Span.ResponseTruncated = true   // the sink hit its own bound receiving the compressed stream
	ev.Span.ResponseBytesSeen = 900000 // a large compressed wire count -- must not be reported

	oc := openboxCapture(t, wireOf(t, ev))
	if oc == nil {
		t.Fatal("activity_output.openbox_capture is absent")
	}
	if got, ok := oc["truncated"].(bool); !ok || !got {
		t.Errorf("truncated = %v, want true", oc["truncated"])
	}
	if got, ok := oc["original_bytes"].(float64); !ok || int(got) != len(marker) {
		t.Errorf("original_bytes = %v, want the marker's own honest length %d, not a fabricated "+
			"figure like the gateway's raw wire count %d", oc["original_bytes"], len(marker), ev.Span.ResponseBytesSeen)
	}
}

// TestTheResponseCompletenessNoteORsInTheClientsOwnCut is acceptance
// criterion 4: a response over maxModelCallBodyBytes is cut by capModelCallBody
// at the client, independent of anything the gateway reported, and that cut
// alone must still flip truncated to true.
func TestTheResponseCompletenessNoteORsInTheClientsOwnCut(t *testing.T) {
	long := strings.Repeat("y", maxModelCallBodyBytes+10)
	ev := modelCallEvent(EventTurnCompleted, "{}", long)
	// Gateway-side signal left at zero value: the client's own cap must be
	// sufficient on its own.

	oc := openboxCapture(t, wireOf(t, ev))
	if oc == nil {
		t.Fatal("activity_output.openbox_capture is absent")
	}
	if got, ok := oc["truncated"].(bool); !ok || !got {
		t.Errorf("truncated = %v, want true: capModelCallBody cut this body regardless of the gateway", oc["truncated"])
	}
	if got, ok := oc["original_bytes"].(float64); !ok || int(got) != len(long) {
		t.Errorf("original_bytes = %v, want the pre-cut length %d (the gateway reported no count)",
			oc["original_bytes"], len(long))
	}
	// TestATruncatedBodyIsMarkedAsTruncated pins that the in-band truncationMark
	// itself is unaffected; this test only asserts the note is additive to it.
}

// TestNoResponseBodyMeansNoCompletenessNote is acceptance criterion 5's other
// half: a completed call that carries no response body at all (never
// observed one, or content capture stripped it) must not carry the note
// either -- there is no stored body for it to describe.
func TestNoResponseBodyMeansNoCompletenessNote(t *testing.T) {
	ev := modelCallEvent(EventTurnCompleted, "{}", "")
	ev.Model = "claude-opus-5" // forces activity_output non-nil via the model key

	m := wireOf(t, ev)
	ao, ok := m["activity_output"].(map[string]any)
	if !ok {
		t.Fatal("activity_output absent; fixture precondition failed")
	}
	if _, present := ao["content"]; present {
		t.Error("activity_output.content present with no response body")
	}
	if _, present := ao["openbox_capture"]; present {
		t.Error("activity_output.openbox_capture present with no response body to describe")
	}
}

// TestSchemaAcceptsTheResponseCompletenessFields is acceptance criterion 7,
// mirroring TestSchemaAcceptsTheAttributionFields: a Span carrying the two new
// fields must still pass conformance.ValidateDevEvent, or $defs/span's
// additionalProperties:false rejects every in-path event.
func TestSchemaAcceptsTheResponseCompletenessFields(t *testing.T) {
	ev := baseValidEvent()
	ev.EventType = EventToolResult
	ev.Span = &Span{
		SemanticType:      "internal",
		Stage:             "completed",
		ResponseBytesSeen: 128,
		ResponseTruncated: true,
	}
	if err := conformance.ValidateDevEvent(devEventJSON(t, ev), false); err != nil {
		t.Errorf("a valid response-completeness fixture failed schema validation: %v", err)
	}
}

// TestModelCallResponseCutRecordsItselfInTheCaptureSummary closes the gap an
// advisory pass found: capModelCallBody's own cut used to bypass cutLog
// entirely, so metadata.openbox_capture.truncated_paths stayed silent about
// exactly the largest thing a model-call row cut. This is the relayed-call
// (Span.ResponseBody) arm, which writes only the `content` key.
func TestModelCallResponseCutRecordsItselfInTheCaptureSummary(t *testing.T) {
	long := strings.Repeat("y", maxModelCallBodyBytes+10)
	ev := modelCallEvent(EventTurnCompleted, "{}", long)

	got := truncated(t, ev)
	want := []string{"activity_output.content"}
	if !slices.Equal(got, want) {
		t.Errorf("truncated_paths = %v, want %v", got, want)
	}
}

// TestHookTurnReplyCutRecordsBothKeysInTheCaptureSummary is the hook-lane
// twin: Content.Output writes BOTH `content` and `reply_text` from the same
// source string, so a cut there must record both paths.
func TestHookTurnReplyCutRecordsBothKeysInTheCaptureSummary(t *testing.T) {
	long := strings.Repeat("y", maxModelCallBodyBytes+10)
	idx := 3
	hook := DevEvent{
		SchemaVersion: SchemaVersion,
		EventID:       "ev-hook-long",
		EventType:     EventTurnCompleted,
		SessionID:     "sess-hook",
		DeveloperDID:  "did:aip:7f3c9b2e-0000-5000-a000-000000000001",
		Timestamp:     "2026-08-13T10:00:12Z",
		Tool:          Tool{Name: "claude-code", Kind: ToolShell},
		TurnIndex:     &idx,
		Model:         "claude-opus-4-8",
		Content:       &Content{Output: long},
	}

	got := truncated(t, hook)
	want := []string{"activity_output.content", "activity_output.reply_text"}
	if !slices.Equal(got, want) {
		t.Errorf("truncated_paths = %v, want %v", got, want)
	}
}

// TestUncutModelCallResponseRecordsNothing is the negative control: a body
// under the cap must not appear in truncated_paths, or every relayed call
// would look cut.
func TestUncutModelCallResponseRecordsNothing(t *testing.T) {
	ev := modelCallEvent(EventTurnCompleted, "{}", `{"type":"message"}`)
	if got := truncated(t, ev); got != nil {
		t.Errorf("truncated_paths = %v, want nil (nothing was cut)", got)
	}
}

// TestAnUnobservedStatusIsAbsentRatherThanZero: proxy.go emits Complete(0, ...)
// when the upstream never answered. A stored 0 would read as a real status.
func TestAnUnobservedStatusIsAbsentRatherThanZero(t *testing.T) {
	failed := modelCallEvent(EventTurnCompleted, "req", "")
	failed.Span.HTTPStatus = 0

	meta, ok := wireOf(t, failed)["metadata"].(map[string]any)
	if !ok {
		t.Fatal("no metadata")
	}
	if got, present := meta["http_status"]; present {
		t.Errorf("http_status = %v for a call that got no response; want the key absent", got)
	}
}
