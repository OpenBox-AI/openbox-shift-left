package client

import (
	"encoding/json"
	"strings"
	"testing"
)

// The turn activity_id is the second identity in this package a refactor must not
// move, for the same class of reason as the approval key (approval_key_pin_test.go):
// both halves of a turn pair onto one row by sharing it, and core's dedupe key
// includes it — so a derivation that changes between the Started and Completed
// emission, or between an emission and a post-crash re-emission, either splits one
// turn across two rows or stops the server from absorbing the duplicate.
//
// The failure is silent under tests that build their expectation from the same
// function they exercise. These cases pin literal bytes instead.
//
// Do not regenerate these values. If one fails, the derivation moved and the
// change is wrong, not the fixture.

func pinTurnEvent() DevEvent {
	idx := 3
	return DevEvent{
		SchemaVersion: SchemaVersion,
		EventID:       "ev-turn-pin-1",
		EventType:     EventTurnCompleted,
		SessionID:     "sess-pin-0001",
		DeveloperDID:  "did:aip:7f3c9b2e-0000-5000-a000-000000000001",
		WorkspaceID:   "openbox-ai/openbox-shift-left",
		Timestamp:     "2026-08-11T09:00:05Z",
		StartedAt:     "2026-08-11T09:00:00Z",
		EndedAt:       "2026-08-11T09:00:05Z",
		Tool:          Tool{Name: "claude-code", Kind: ToolShell},
		Model:         "claude-opus-4-8",
		TurnIndex:     &idx,
		Tokens: &Tokens{
			Input:              intp(120),
			Output:             intp(45),
			CacheCreationInput: intp(2048),
			CacheRead:          intp(16384),
		},
	}
}

func intp(v int) *int { return &v }

const (
	pinTurnActivityID     = "sess-pin-0001:turn:3"
	pinSubagentActivityID = "sess-pin-0001:agent:agt-77:turn:3"
)

// TestTurnActivityIDIsPinned holds the exact wire id for a fixed turn event, and
// asserts the pair shares it.
func TestTurnActivityIDIsPinned(t *testing.T) {
	ev := pinTurnEvent()

	if got := turnActivityIDFor(ev); got != pinTurnActivityID {
		t.Errorf("turn activity id = %q, want %q", got, pinTurnActivityID)
	}

	// The Started half of the same turn addresses the same record. Everything
	// that differs between the halves (event type, timestamps, output) is
	// excluded from the derivation by construction.
	started := ev
	started.EventID = "ev-turn-pin-0"
	started.EventType = EventTurnStarted
	started.Timestamp = "2026-08-11T09:00:00Z"
	started.EndedAt = ""
	if got := turnActivityIDFor(started); got != pinTurnActivityID {
		t.Errorf("started half id = %q, want %q (the pair must address one record)", got, pinTurnActivityID)
	}

	// A subagent's turn is partitioned, so subagent turn 3 and main-thread turn 3
	// are different records. Without this, a subagent's tokens would land on the
	// parent's row and one of the two would be deduped away.
	sub := ev
	sub.AgentID = "agt-77"
	if got := turnActivityIDFor(sub); got != pinSubagentActivityID {
		t.Errorf("subagent turn id = %q, want %q", got, pinSubagentActivityID)
	}
	if turnActivityIDFor(sub) == turnActivityIDFor(ev) {
		t.Error("subagent and main-thread turn share an activity_id; one would be deduped away")
	}
}

// TestTurnActivityIDCannotCollideWithToolCallID pins the two id shapes apart.
// They live in one column and core's dedupe treats the value as opaque, so a
// collision would silently merge a turn with a tool call — and, because the
// tool-call id is the approval key, could make a turn event address an approval.
func TestTurnActivityIDCannotCollideWithToolCallID(t *testing.T) {
	turn := turnActivityIDFor(pinTurnEvent())
	tool := activityIDFor(pinEvent())

	if !strings.HasPrefix(tool, "cc-act-") {
		t.Fatalf("tool activity id %q lost its cc-act- prefix; the separation argument below no longer holds", tool)
	}
	if strings.HasPrefix(turn, "cc-act-") {
		t.Errorf("turn activity id %q took the tool-call prefix", turn)
	}
	if strings.ContainsRune(tool, ':') {
		t.Errorf("tool activity id %q gained a colon; it can now be shaped like a turn id", tool)
	}
	if !strings.ContainsRune(turn, ':') {
		t.Errorf("turn activity id %q lost its colon separator", turn)
	}
}

// TestTurnActivityIDAbsentWithoutIndex pins the omitted case. A turn event whose
// index never got set must not mint "<session>:turn:" — an id that would collapse
// every such turn onto one row.
func TestTurnActivityIDAbsentWithoutIndex(t *testing.T) {
	ev := pinTurnEvent()
	ev.TurnIndex = nil
	if got := turnActivityIDFor(ev); got != "" {
		t.Errorf("turn activity id = %q with no index, want %q", got, "")
	}

	body, err := buildPayload(ev)
	if err != nil {
		t.Fatalf("buildPayload: %v", err)
	}
	var p map[string]any
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if _, ok := p["activity_id"]; ok {
		t.Errorf("activity_id present on an indexless turn: %s", body)
	}
}

// TestTurnActivityOutputCarriesNumbersAndOneString is the schema gate on the
// field core runs Guardrails and OPA over. It asserts the exact key set: four
// counts under usage, one model string, and nothing else. A field added "while
// we're here" fails here.
func TestTurnActivityOutputCarriesNumbersAndOneString(t *testing.T) {
	raw := turnActivityOutput(pinTurnEvent())
	if raw == nil {
		t.Fatal("turnActivityOutput returned nil for an event carrying model and usage")
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal activity_output: %v", err)
	}

	if len(out) != 2 {
		t.Errorf("activity_output has %d top-level keys %v, want exactly {model, usage}", len(out), keysOf(out))
	}
	if got := out["model"]; got != "claude-opus-4-8" {
		t.Errorf("model = %v, want claude-opus-4-8", got)
	}
	usage, ok := out["usage"].(map[string]any)
	if !ok {
		t.Fatalf("usage is not an object: %v", out["usage"])
	}
	want := map[string]float64{
		"input_tokens":                120,
		"output_tokens":               45,
		"cache_creation_input_tokens": 2048,
		"cache_read_input_tokens":     16384,
	}
	if len(usage) != len(want) {
		t.Errorf("usage has keys %v, want exactly %d counts", keysOf(usage), len(want))
	}
	for k, v := range want {
		got, ok := usage[k].(float64)
		if !ok {
			t.Errorf("usage[%q] missing or not a number: %v", k, usage[k])
			continue
		}
		if got != v {
			t.Errorf("usage[%q] = %v, want %v", k, got, v)
		}
	}

	// Every value under usage must be a number. A string reaching this object is
	// the exact regression the allowlist exists to prevent.
	for k, v := range usage {
		if _, isNum := v.(float64); !isNum {
			t.Errorf("usage[%q] = %v (%T), want a number — only the model id may be a string", k, v, v)
		}
	}
}

// TestTurnActivityOutputOmitsWhatIsUnknown: a turn with no usage and no model
// carries no activity_output at all, rather than an empty or zero-filled object.
// Zeroes would claim a turn spent nothing, which is a different statement from
// "not measured".
func TestTurnActivityOutputOmitsWhatIsUnknown(t *testing.T) {
	ev := pinTurnEvent()
	ev.Model = ""
	ev.Tokens = nil
	if raw := turnActivityOutput(ev); raw != nil {
		t.Errorf("activity_output = %s, want omitted", raw)
	}

	// A model-less turn still reports its usage: the core-side extractor buckets
	// it under an unknown model rather than dropping the numbers.
	ev.Tokens = &Tokens{Input: intp(7)}
	raw := turnActivityOutput(ev)
	if raw == nil {
		t.Fatal("activity_output omitted for a turn with usage but no model")
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := out["model"]; ok {
		t.Errorf("model present when unknown: %s", raw)
	}
	usage, _ := out["usage"].(map[string]any)
	if len(usage) != 1 {
		t.Errorf("usage = %v, want only the one known count", usage)
	}
}

// TestTurnPairRidesAcceptListedWireTypes pins the INV-8 claim for the new types:
// they map onto the same two stock activity types a tool call uses, so a stock
// core accepts them with no patch and no accept-list change.
func TestTurnPairRidesAcceptListedWireTypes(t *testing.T) {
	cases := []struct {
		et   EventType
		want string
	}{
		{EventTurnStarted, wireActivityStarted},
		{EventTurnCompleted, wireActivityCompleted},
	}
	for _, tc := range cases {
		got, signal, err := wireTypeFor(tc.et)
		if err != nil {
			t.Fatalf("wireTypeFor(%s): %v", tc.et, err)
		}
		if got != tc.want {
			t.Errorf("wireTypeFor(%s) = %q, want %q", tc.et, got, tc.want)
		}
		if signal != "" {
			t.Errorf("wireTypeFor(%s) returned signal_name %q; a turn is an activity, not a signal", tc.et, signal)
		}
		ev := pinTurnEvent()
		ev.EventType = tc.et
		if label := activityLabel(ev); label != "llm_completion" {
			t.Errorf("activityLabel(%s) = %q, want llm_completion", tc.et, label)
		}
	}
}

// TestUsageRollupIDIsPinned holds Codex's session-rollup id, and pins it apart
// from both other id shapes in the column.
func TestUsageRollupIDIsPinned(t *testing.T) {
	ev := pinTurnEvent()
	ev.TurnIndex = nil
	ev.SessionRollup = true

	const want = "sess-pin-0001:usage:rollup"
	if got := turnActivityIDFor(ev); got != want {
		t.Errorf("rollup activity id = %q, want %q", got, want)
	}

	// The flag wins over an index, so a rollup that also happens to carry one is
	// still a rollup rather than silently becoming turn N.
	idx := 3
	ev.TurnIndex = &idx
	if got := turnActivityIDFor(ev); got != want {
		t.Errorf("rollup with an index = %q, want %q", got, want)
	}

	// Distinct from an indexed turn and from a tool call: all three shapes share
	// one column, and core's dedupe treats the value as opaque.
	if turnActivityIDFor(pinTurnEvent()) == want {
		t.Error("an indexed turn and a session rollup share an activity_id")
	}
	if strings.HasPrefix(want, "cc-act-") {
		t.Error("the rollup id took the tool-call prefix")
	}
}

// The parity claim, enforced rather than asserted in prose: a Claude Code
// per-turn pair and a Codex session-rollup pair produce the SAME wire envelope
// (types, activity_type, output shape) and differ only in activity_id and the
// numbers. The two adapters are separate Go modules and cannot import each other,
// so this — plus the golden fixtures — is where the shapes can be compared.
func TestTurnAndRollupShareOneWireShape(t *testing.T) {
	perTurn := pinTurnEvent()

	rollup := pinTurnEvent()
	rollup.EventID = "ev-rollup-pin"
	rollup.TurnIndex = nil
	rollup.SessionRollup = true
	rollup.Model = "gpt-5.6-sol"

	shapeOf := func(t *testing.T, ev DevEvent) map[string]any {
		t.Helper()
		body, err := buildPayload(ev)
		if err != nil {
			t.Fatalf("buildPayload: %v", err)
		}
		var p map[string]any
		if err := json.Unmarshal(body, &p); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		return p
	}
	a, b := shapeOf(t, perTurn), shapeOf(t, rollup)

	// Same key set: a field present on one carrier and absent on the other would
	// make a single core-side extractor read one shape and miss the other.
	if len(a) != len(b) {
		t.Errorf("payload key sets differ: per-turn %v vs rollup %v", keysOf(a), keysOf(b))
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			t.Errorf("key %q present on the per-turn payload but absent on the rollup", k)
		}
	}

	// Same discriminators.
	for _, k := range []string{"event_type", "activity_type", "workflow_type", "source"} {
		if a[k] != b[k] {
			t.Errorf("%s differs: per-turn %v vs rollup %v", k, a[k], b[k])
		}
	}
	if a["activity_type"] != "llm_completion" {
		t.Errorf("activity_type = %v, want llm_completion", a["activity_type"])
	}

	// Same activity_output shape, different values.
	outA, _ := a["activity_output"].(map[string]any)
	outB, _ := b["activity_output"].(map[string]any)
	if len(outA) == 0 || len(outB) == 0 {
		t.Fatalf("activity_output missing: per-turn %v, rollup %v", a["activity_output"], b["activity_output"])
	}
	for k := range outA {
		if _, ok := outB[k]; !ok {
			t.Errorf("activity_output key %q on the per-turn payload but not the rollup", k)
		}
	}

	// And the one thing that MUST differ, or the two would collide in one column.
	if a["activity_id"] == b["activity_id"] {
		t.Errorf("both carriers minted activity_id %v", a["activity_id"])
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
