package client

import (
	"slices"
	"strings"
	"testing"
)

// truncated decodes metadata.openbox_capture.truncated_paths for ev, or nil when
// the key is absent (nothing was cut). Shared with the two existing-test
// extensions in payload_enrich_test.go, so there is one decode path for the
// summary rather than one per call site.
func truncated(t *testing.T, ev DevEvent) []string {
	t.Helper()
	meta := rawMeta(t, decodePayload(t, ev))
	raw, ok := meta["openbox_capture"]
	if !ok {
		return nil
	}
	oc, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("metadata.openbox_capture is not an object: %v (%T)", raw, raw)
	}
	list, ok := oc["truncated_paths"].([]any)
	if !ok {
		t.Fatalf("metadata.openbox_capture.truncated_paths is not an array: %v", oc["truncated_paths"])
	}
	out := make([]string, len(list))
	for i, v := range list {
		s, ok := v.(string)
		if !ok {
			t.Fatalf("metadata.openbox_capture.truncated_paths[%d] = %v, want a string", i, v)
		}
		out[i] = s
	}
	return out
}

// TestOpenboxCapture_ToolOutputOverCap is measurable outcome 1's cut half: an
// over-cap tool output names exactly the one path it landed on.
func TestOpenboxCapture_ToolOutputOverCap(t *testing.T) {
	over := strings.Repeat("x", maxBodySize+1)
	ev := DevEvent{
		EventID: "e1", EventType: EventToolResult, SessionID: "s", DeveloperDID: "did:aip:x",
		Timestamp: "2026-09-10T00:00:00Z", Tool: Tool{Name: "Bash", Kind: ToolShell},
		Content: &Content{ToolOutput: over},
	}
	got, want := truncated(t, ev), []string{"activity_output.output"}
	if !slices.Equal(got, want) {
		t.Fatalf("truncated = %v, want %v", got, want)
	}
}

// TestOpenboxCapture_ExactCapMeansNoKeyAtAll is measurable outcome 1's other
// half: a value that lands exactly on the cap is not cut, so the summary must
// not appear at all -- not an empty object, not an empty array.
func TestOpenboxCapture_ExactCapMeansNoKeyAtAll(t *testing.T) {
	exact := strings.Repeat("x", maxBodySize)
	ev := DevEvent{
		EventID: "e1", EventType: EventToolResult, SessionID: "s", DeveloperDID: "did:aip:x",
		Timestamp: "2026-09-10T00:00:00Z", Tool: Tool{Name: "Bash", Kind: ToolShell},
		Content: &Content{ToolOutput: exact},
	}
	meta := rawMeta(t, decodePayload(t, ev))
	if v, present := meta["openbox_capture"]; present {
		t.Fatalf("an exactly-at-cap value must not produce openbox_capture at all; got %v", v)
	}
}

// TestOpenboxCapture_DenialReasonCutTwice is measurable outcome 2 (Key
// Insight 8): denial_reason arrives via ev.Metadata (not Content), so the
// dynamic backstop (site 7, eventMetadataForEgress) is the one that fires --
// once from buildMetadata's call and once from buildSignalArgs's, because
// PermissionDenied is a signal class and both builders run for it. The same
// value is genuinely cut into two different wire objects: two entries, no
// dedupe.
func TestOpenboxCapture_DenialReasonCutTwice(t *testing.T) {
	huge := strings.Repeat("z", maxBodySize+10)
	ev := DevEvent{
		EventID: "e1", EventType: EventPermissionDenied, SessionID: "s", DeveloperDID: "did:aip:x",
		Timestamp: "2026-09-10T00:00:00Z", Tool: Tool{Name: "Bash", Kind: ToolShell},
		Metadata: map[string]any{"denial_reason": huge},
	}
	got, want := truncated(t, ev), []string{"metadata.denial_reason", "signal_args.denial_reason"}
	if !slices.Equal(got, want) {
		t.Fatalf("truncated = %v, want %v (two entries, one per destination, no dedupe)", got, want)
	}
}

// TestOpenboxCapture_MetadataCommandBackstop is measurable outcome 3: an
// over-cap command arrives via ev.Metadata rather than Content, so only the
// backstop can see it. EventSessionStarted is not a signal class, so
// buildSignalArgs never runs and only metadata.command is recorded.
func TestOpenboxCapture_MetadataCommandBackstop(t *testing.T) {
	huge := strings.Repeat("c", maxBodySize+10)
	ev := DevEvent{
		EventID: "e1", EventType: EventSessionStarted, SessionID: "s", DeveloperDID: "did:aip:x",
		Timestamp: "2026-09-10T00:00:00Z", Tool: Tool{Name: "claude-code", Kind: ToolShell},
		Metadata: map[string]any{"command": huge},
	}
	got, want := truncated(t, ev), []string{"metadata.command"}
	if !slices.Equal(got, want) {
		t.Fatalf("truncated = %v, want %v (backstop, single destination)", got, want)
	}
}

// TestOpenboxCapture_AbsentWhenContentGateStrips is measurable outcome 4 and
// the Requirements' INV-2 clause: with content capture off, contentStripped
// makes eventMetadataForEgress `continue` before capBodyInto ever runs, so
// nothing is cut and the summary cannot exist to reveal that a value was
// once there.
func TestOpenboxCapture_AbsentWhenContentGateStrips(t *testing.T) {
	huge := strings.Repeat("x", maxBodySize+10)
	ev := DevEvent{
		EventID: "e1", EventType: EventPermissionDenied, SessionID: "s", DeveloperDID: "did:aip:x",
		Timestamp: "2026-09-10T00:00:00Z", Tool: Tool{Name: "Bash", Kind: ToolShell},
		Metadata: map[string]any{"denial_reason": huge},
	}
	meta := rawMeta(t, decodePayload(t, stripContent(ev)))
	if v, present := meta["denial_reason"]; present {
		t.Fatalf("content gate should have dropped denial_reason entirely; got %v", v)
	}
	if v, present := meta["openbox_capture"]; present {
		t.Fatalf("openbox_capture must be absent when the gate stripped the only cut candidate; got %v", v)
	}
}

// TestOpenboxCapture_PromptSubmittedStaysOneKeyAndRecordsCut is measurable
// outcome 5: prompt_submitted's signal_args is the goal (R2) and must stay a
// one-key map even when its value is capped, while the cut still surfaces in
// metadata.openbox_capture (a different destination) rather than being lost.
func TestOpenboxCapture_PromptSubmittedStaysOneKeyAndRecordsCut(t *testing.T) {
	huge := strings.Repeat("y", maxBodySize+10)
	ev := DevEvent{
		EventID: "e1", EventType: EventPromptSubmitted, SessionID: "s", DeveloperDID: "did:aip:x",
		Timestamp: "2026-09-10T00:00:00Z", Tool: Tool{Name: "claude-code", Kind: ToolShell},
		Content: &Content{Prompt: huge},
	}
	if args := signalArgs(t, ev); len(args) != 1 {
		t.Fatalf("prompt_submitted signal_args must stay a one-key map even when capped, got %v", args)
	}
	got, want := truncated(t, ev), []string{"signal_args.prompt"}
	if !slices.Equal(got, want) {
		t.Fatalf("truncated = %v, want %v", got, want)
	}
}

// TestOpenboxCapture_TruncatedIsSortedAcrossMapIteration is measurable
// outcome 6. eventMetadataForEgress iterates ev.Metadata, a Go map with
// randomized iteration order; two over-cap keys in the same map, read by both
// buildMetadata and buildSignalArgs (PermissionDenied is a signal class),
// give four cuts whose recording order would otherwise vary run to run.
// -count=100 is the guard against that flapping.
func TestOpenboxCapture_TruncatedIsSortedAcrossMapIteration(t *testing.T) {
	hugeA := strings.Repeat("a", maxBodySize+10)
	hugeB := strings.Repeat("b", maxBodySize+10)
	ev := DevEvent{
		EventID: "e1", EventType: EventPermissionDenied, SessionID: "s", DeveloperDID: "did:aip:x",
		Timestamp: "2026-09-10T00:00:00Z", Tool: Tool{Name: "Bash", Kind: ToolShell},
		Metadata: map[string]any{"command": hugeA, "output": hugeB},
	}
	got := truncated(t, ev)
	want := []string{"metadata.command", "metadata.output", "signal_args.command", "signal_args.output"}
	if !slices.Equal(got, want) {
		t.Fatalf("truncated = %v, want %v sorted regardless of map iteration order", got, want)
	}
}

// TestOpenboxCaptureKeyIsNotAContentMetadataKey is measurable outcome 7,
// asserted directly. contentMetadataKeys entries are dropped entirely by
// payload.go's egress loop when content is stripped -- exactly when a reader
// needs the summary -- so the summary must never join that map.
func TestOpenboxCaptureKeyIsNotAContentMetadataKey(t *testing.T) {
	if contentMetadataKeys["openbox_capture"] {
		t.Fatal("openbox_capture must not be in contentMetadataKeys: a member there is dropped " +
			"when content is stripped, exactly when a reader needs the summary")
	}
}

// TestOpenboxCaptureSurvivesAsNestedObject is measurable outcome 9. The
// backstop (site 7) only ever sees ev.Metadata's own keys, and openbox_capture
// is written into the output map afterward -- so it can never be re-capped as
// a string. Proven observably: it must decode as an object, not a string.
func TestOpenboxCaptureSurvivesAsNestedObject(t *testing.T) {
	huge := strings.Repeat("d", maxBodySize+10)
	ev := DevEvent{
		EventID: "e1", EventType: EventToolResult, SessionID: "s", DeveloperDID: "did:aip:x",
		Timestamp: "2026-09-10T00:00:00Z", Tool: Tool{Name: "Bash", Kind: ToolShell},
		Metadata: map[string]any{"command": huge},
		Content:  &Content{ToolOutput: huge},
	}
	meta := rawMeta(t, decodePayload(t, ev))
	raw, ok := meta["openbox_capture"]
	if !ok {
		t.Fatal("openbox_capture missing even though command and output were both over cap")
	}
	oc, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("openbox_capture is not a nested object: %v (%T)", raw, raw)
	}
	list, ok := oc["truncated_paths"].([]any)
	if !ok {
		t.Fatalf("openbox_capture.truncated_paths is not an array: %v", oc["truncated_paths"])
	}
	want := []string{"activity_output.output", "metadata.command"}
	if len(list) != len(want) {
		t.Fatalf("truncated = %v, want %v", list, want)
	}
	for i, w := range want {
		if list[i] != w {
			t.Fatalf("truncated[%d] = %v, want %q", i, list[i], w)
		}
	}
	cmd, _ := meta["command"].(string)
	if n := len([]rune(cmd)); n != maxBodySize {
		t.Errorf("metadata.command = %d runes, want capped to %d; the backstop must still cap the string "+
			"even though it must not touch the nested object", n, maxBodySize)
	}
}

// TestTheTruncationSummaryNeverReachesSignalArgs pins the one placement fact
// that keeps this object off the enforcement path.
//
// metadata has no governance-engine reader: not OPA policy input, not
// Guardrails, not the alignment judge. signal_args does -- OPA and Guardrails
// both read it, and buildSignalArgs projects ev.Metadata onto every signal
// class through eventMetadataForEgress. So the summary is safe only because
// buildPayload assembles it inside buildMetadata, which runs AFTER
// buildSignalArgs; it is never a member of ev.Metadata that the projection
// could pick up.
//
// That ordering is load-bearing and nothing else pins it. An implementer who
// "simplifies" by writing openbox_capture into ev.Metadata upstream would ship
// it into policy input as input.openbox_capture on ~25 signal classes. This
// test reds on that change.
//
// The prompt class cannot catch it: prompt_submitted's signal_args is the goal
// and is a one-key map by construction, so the leak would be invisible there.
// This uses a non-prompt signal whose signal_args is a full projection.
func TestTheTruncationSummaryNeverReachesSignalArgs(t *testing.T) {
	over := strings.Repeat("z", maxBodySize+10)
	ev := DevEvent{
		EventID: "e1", EventType: EventPermissionDenied, SessionID: "s", DeveloperDID: "did:aip:x",
		Timestamp: "2026-09-10T00:00:00Z", Tool: Tool{Name: "Bash", Kind: ToolShell},
		Metadata: map[string]any{"command": over},
	}

	// The cut is real and the summary did land in metadata, so a green result
	// below means "absent from signal_args", not "nothing was truncated".
	if got := truncated(t, ev); !slices.Equal(got, []string{"metadata.command", "signal_args.command"}) {
		t.Fatalf("precondition: truncated = %v, want the cut recorded in metadata for both destinations", got)
	}

	if _, present := signalArgs(t, ev)["openbox_capture"]; present {
		t.Fatal("openbox_capture reached signal_args, which OPA and Guardrails read; " +
			"the summary must be assembled in buildMetadata only, never as a member of ev.Metadata")
	}
}

// TestAnAdapterWrittenCaptureSummaryIsDropped pins openbox_capture as
// client-reserved: it is synthesized from cutLog and may never be carried in
// from a caller.
//
// Two distinct holes are closed by the one bar in eventMetadataForEgress.
// (a) signal_args reaches OPA and Guardrails, so a caller-supplied value on a
// signal class would land in policy input.
// (b) buildMetadata writes the key only when something was actually cut, so on
// an event with NO cuts a caller-supplied value would otherwise survive as a
// forged summary claiming truncation that never happened.
//
// This is the stronger form of TestTheTruncationSummaryNeverReachesSignalArgs:
// that test pins the builder ORDERING, this one pins the RULE, so the ordering
// stops being load-bearing.
func TestAnAdapterWrittenCaptureSummaryIsDropped(t *testing.T) {
	// A signal class, nothing over cap: the summary must be absent everywhere,
	// so anything that appears was carried in rather than synthesized.
	ev := DevEvent{
		EventID: "e1", EventType: EventPermissionDenied, SessionID: "s", DeveloperDID: "did:aip:x",
		Timestamp: "2026-09-10T00:00:00Z", Tool: Tool{Name: "Bash", Kind: ToolShell},
		Metadata: map[string]any{
			"command":         "echo hi",
			captureSummaryKey: map[string]any{"truncated_paths": []string{"activity_output.output"}},
		},
	}

	if got := truncated(t, ev); got != nil {
		t.Fatalf("a forged summary survived into metadata: %v; nothing was cut, so the key must be absent", got)
	}
	if _, present := signalArgs(t, ev)[captureSummaryKey]; present {
		t.Fatal("a forged summary reached signal_args, which OPA and Guardrails read")
	}
}
