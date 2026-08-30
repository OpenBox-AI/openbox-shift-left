package client

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/conformance"
)

// Do not regenerate these values.

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

// TestTurnActivityIDIsPinned holds the exact wire id for a fixed turn event,
// and asserts the pair shares it.
func TestTurnActivityIDIsPinned(t *testing.T) {
	ev := pinTurnEvent()

	if got := turnActivityIDFor(ev); got != pinTurnActivityID {
		t.Errorf("turn activity id = %q, want %q", got, pinTurnActivityID)
	}

	started := ev
	started.EventID = "ev-turn-pin-0"
	started.EventType = EventTurnStarted
	started.Timestamp = "2026-08-11T09:00:00Z"
	started.EndedAt = ""
	if got := turnActivityIDFor(started); got != pinTurnActivityID {
		t.Errorf("started half id = %q, want %q (the pair must address one record)", got, pinTurnActivityID)
	}

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

// TestTurnActivityIDAbsentWithoutIndex pins the omitted case. A turn event
// whose index never got set must not mint "<session>:turn:"; an id that would
// collapse every such turn onto one row.
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
// field core runs Guardrails and OPA over.
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

	for k, v := range usage {
		if _, isNum := v.(float64); !isNum {
			t.Errorf("usage[%q] = %v (%T), want a number — only the model id may be a string", k, v, v)
		}
	}
}

// TestTurnActivityOutputOmitsWhatIsUnknown: a turn with no usage and no model
// carries no activity_output at all, rather than an empty or zero-filled
// object.
func TestTurnActivityOutputOmitsWhatIsUnknown(t *testing.T) {
	ev := pinTurnEvent()
	ev.Model = ""
	ev.Tokens = nil
	if raw := turnActivityOutput(ev); raw != nil {
		t.Errorf("activity_output = %s, want omitted", raw)
	}

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

// TestTurnPairRidesAcceptListedWireTypes pins the INV-8 claim for the new
// types: they map onto the same two stock activity types a tool call uses, so
// a stock core accepts them with no patch and no accept-list change.
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

	idx := 3
	ev.TurnIndex = &idx
	if got := turnActivityIDFor(ev); got != want {
		t.Errorf("rollup with an index = %q, want %q", got, want)
	}

	if turnActivityIDFor(pinTurnEvent()) == want {
		t.Error("an indexed turn and a session rollup share an activity_id")
	}
	if strings.HasPrefix(want, "cc-act-") {
		t.Error("the rollup id took the tool-call prefix")
	}
}

// TestTurnAndRollupShareOneWireShape the parity claim, enforced rather than
// asserted in prose: a Claude Code per-turn pair and a Codex session-rollup
// pair produce the same wire envelope (types, activity_type, output shape) and
// differ only in activity_id and the numbers.
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

	if len(a) != len(b) {
		t.Errorf("payload key sets differ: per-turn %v vs rollup %v", keysOf(a), keysOf(b))
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			t.Errorf("key %q present on the per-turn payload but absent on the rollup", k)
		}
	}

	for _, k := range []string{"event_type", "activity_type", "workflow_type", "source"} {
		if a[k] != b[k] {
			t.Errorf("%s differs: per-turn %v vs rollup %v", k, a[k], b[k])
		}
	}
	if a["activity_type"] != "llm_completion" {
		t.Errorf("activity_type = %v, want llm_completion", a["activity_type"])
	}

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

const (
	pinOtelActivityID  = "sess-pin-0001:otel:req_011CSxKq9mNp"
	pinProxyActivityID = "sess-pin-0001:proxy:px-4f2a9c1e7b30"
)

// TestNewLaneActivityIDsArePinned holds the wire bytes for the two lanes that
// decision adds.
func TestNewLaneActivityIDsArePinned(t *testing.T) {
	otel := pinTurnEvent()
	otel.TurnIndex = nil
	otel.OtelRequestID = "req_011CSxKq9mNp"
	if got := turnActivityIDFor(otel); got != pinOtelActivityID {
		t.Errorf("telemetry turn id = %q, want %q", got, pinOtelActivityID)
	}

	proxy := pinTurnEvent()
	proxy.TurnIndex = nil
	proxy.ProxyRequestID = "px-4f2a9c1e7b30"
	if got := turnActivityIDFor(proxy); got != pinProxyActivityID {
		t.Errorf("transport turn id = %q, want %q", got, pinProxyActivityID)
	}
}

// turnLanes is the ONE list of model-call producers these tests share.
var turnLanes = []struct {
	name   string
	field  string
	set    func(*DevEvent)
	clear  func(*DevEvent)
	marker string
	id     string
}{
	{
		name: "transport", field: "proxy_request_id",
		set:    func(e *DevEvent) { e.ProxyRequestID = "px-4f2a9c1e7b30" },
		clear:  func(e *DevEvent) { e.ProxyRequestID = "" },
		marker: ":proxy:", id: "sess-1:proxy:px-4f2a9c1e7b30",
	},
	{
		name: "gateway", field: "gateway_request_id",
		set:    func(e *DevEvent) { e.GatewayRequestID = "req-abc123" },
		clear:  func(e *DevEvent) { e.GatewayRequestID = "" },
		marker: ":gateway:", id: "sess-1:gateway:req-abc123",
	},
	{
		name: "telemetry", field: "otel_request_id",
		set:    func(e *DevEvent) { e.OtelRequestID = "req_011CSxKq9mNp" },
		clear:  func(e *DevEvent) { e.OtelRequestID = "" },
		marker: ":otel:", id: "sess-1:otel:req_011CSxKq9mNp",
	},
	{
		name: "rollup", field: "session_rollup",
		set:    func(e *DevEvent) { e.SessionRollup = true },
		clear:  func(e *DevEvent) { e.SessionRollup = false },
		marker: ":usage:rollup", id: "sess-1:usage:rollup",
	},
	{
		name: "hook", field: "turn_index",
		set:    func(e *DevEvent) { i := 3; e.TurnIndex = &i },
		clear:  func(e *DevEvent) { e.TurnIndex = nil },
		marker: ":turn:", id: "sess-1:turn:3",
	},
}

func laneEvent() DevEvent {
	return DevEvent{SessionID: "sess-1", EventType: EventTurnCompleted}
}

// TestTurnLanesMatchTheContract binds the list above to the schema, the way
// conformance.TestDiscriminatorListMatchesTheSchema binds its own.
func TestTurnLanesMatchTheContract(t *testing.T) {
	schema, err := conformance.LoadSchema()
	if err != nil {
		t.Fatalf("load schema: %v", err)
	}
	defs, _ := schema["$defs"].(map[string]any)
	producer, _ := defs["turnProducer"].(map[string]any)
	branches, _ := producer["oneOf"].([]any)
	if len(branches) == 0 {
		t.Fatal("$defs.turnProducer has no branches — the exactly-one rule is gone")
	}

	inContract := map[string]bool{}
	for _, b := range branches {
		m, _ := b.(map[string]any)
		req, _ := m["required"].([]any)
		for _, r := range req {
			f, _ := r.(string)
			inContract[f] = true
		}
	}
	inTests := map[string]bool{}
	for _, l := range turnLanes {
		inTests[l.field] = true
	}

	for f := range inContract {
		if !inTests[f] {
			t.Errorf("contract declares producer %q but turnLanes omits it — "+
				"its disjointness and precedence are unasserted", f)
		}
	}
	for f := range inTests {
		if !inContract[f] {
			t.Errorf("turnLanes has %q but no contract branch requires it — "+
				"these tests assert a producer the contract does not have", f)
		}
	}
}

// TestEveryTurnProducerNamespaceIsDisjoint is requirement 8, widened from two
// producers to all of them.
func TestEveryTurnProducerNamespaceIsDisjoint(t *testing.T) {
	seen := map[string]string{}
	check := func(name, id, marker string) {
		t.Helper()
		if id == "" {
			t.Errorf("%s lane minted no activity_id; its turns would never correlate onto a row", name)
			return
		}
		if !strings.Contains(id, marker) {
			t.Errorf("%s lane id %q lost its %q namespace marker — disjointness is by separator, not by luck", name, id, marker)
		}
		if prev, dup := seen[id]; dup {
			t.Errorf("%s and %s lanes share activity_id %q; core dedupe would absorb one and half the evidence would vanish", prev, name, id)
		}
		seen[id] = name
	}

	for _, l := range turnLanes {
		ev := laneEvent()
		l.set(&ev)
		got := turnActivityIDFor(ev)
		if got != l.id {
			t.Errorf("%s lane id = %q, want %q", l.name, got, l.id)
		}
		check(l.name, got, l.marker)
	}

	// It belongs here (its ids must stay disjoint too) and not in turnLanes (it
	// is not a contract branch).
	sub := laneEvent()
	idx := 3
	sub.TurnIndex = &idx
	sub.AgentID = "agt-77"
	check("subagent", turnActivityIDFor(sub), ":agent:")

	tool := activityIDFor(pinEvent())
	if strings.ContainsRune(tool, ':') {
		t.Fatalf("tool activity id %q gained a colon; the separation argument above no longer holds", tool)
	}
	if name, dup := seen[tool]; dup {
		t.Errorf("%s lane id collides with a tool call id %q", name, tool)
	}
}

// TestTurnProducerPrecedenceIsPinned the derivation reads exactly one
// discriminator, so an event carrying two is malformed; the contract's
// turnProducer oneOf rejects it before it can be sent. Pinning only the TOP of
// the ladder would let any two rungs beneath it be swapped silently.
func TestTurnProducerPrecedenceIsPinned(t *testing.T) {
	for i, want := range turnLanes {
		t.Run(want.name, func(t *testing.T) {
			ev := laneEvent()
			for _, l := range turnLanes[i:] {
				l.set(&ev)
			}
			if got := turnActivityIDFor(ev); got != want.id {
				t.Errorf("with %s the highest lane set: id = %q, want %q", want.name, got, want.id)
			}
		})
	}

	t.Run("none", func(t *testing.T) {
		if got := turnActivityIDFor(laneEvent()); got != "" {
			t.Errorf("id = %q with no discriminator set, want %q", got, "")
		}
	})
}

// TestTurnWithNoProducerGetsNoSpan a turn with no producer discriminator must
// get NO span.
func TestTurnWithNoProducerGetsNoSpan(t *testing.T) {
	ev := laneEvent()
	ev.Content = &Content{Output: "the assistant's reply"}

	if got := turnActivityIDFor(ev); got != "" {
		t.Fatalf("fixture is wrong: it names a producer (%q), so it cannot exercise the empty-id path", got)
	}
	if span := turnAssistantSpan(ev); span != nil {
		t.Errorf("a turn naming no producer got span_id %q; every such turn in a session "+
			"shares that id and core's dedupe drops all but the first", span.SpanID)
	}

	for _, l := range turnLanes {
		withLane := ev
		l.set(&withLane)
		if turnAssistantSpan(withLane) == nil {
			t.Errorf("%s turn carrying assistant text got no span", l.name)
		}
	}
}
