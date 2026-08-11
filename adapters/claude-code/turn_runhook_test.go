package claudecode

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/client"
)

// End-to-end tests for the turn boundary through the real RunHook entry point:
// the hook the provider actually invokes, over a real transcript file, writing to
// a real spool. The unit tests in usage_test.go prove the window arithmetic; these
// prove the wiring — that Stop fires the pair, that the cursor makes repeated
// firings disjoint, and that neither hook can ever block a session.

// turnEnv isolates the spool, config, session registry and identity, turns finops
// on, and returns the spool dir.
func turnEnv(t *testing.T, finopsOn bool) string {
	t.Helper()
	spool := t.TempDir()
	isolateConfig(t)
	t.Setenv(envDID, testDID)
	t.Setenv("OPENBOX_SPOOL_DIR", spool)
	t.Setenv("OPENBOX_SESSION_DIR", t.TempDir())
	// Realtime delivery forks a detached flusher; off here so the test observes
	// the spool rather than racing a drain.
	t.Setenv(devconfig.EnvRealtime, "0")
	if finopsOn {
		t.Setenv(devconfig.EnvFinops, "1")
	} else {
		t.Setenv(devconfig.EnvFinops, "0")
	}
	return spool
}

// usageLine renders one assistant transcript line.
func usageLine(model string, in, out int, sidechain bool) string {
	return `{"type":"assistant","isSidechain":` + strconv.FormatBool(sidechain) +
		`,"timestamp":"2026-08-11T09:00:00.000Z","message":{"model":"` + model +
		`","usage":{"input_tokens":` + strconv.Itoa(in) +
		`,"output_tokens":` + strconv.Itoa(out) + `}}}` + "\n"
}

// spooledEvents reads every DevEvent the spool holds, in file order.
func spooledEvents(t *testing.T, spool string) []client.DevEvent {
	t.Helper()
	var out []client.DevEvent
	entries, err := os.ReadDir(spool)
	if err != nil {
		t.Fatalf("read spool: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	for _, name := range names {
		raw, err := os.ReadFile(filepath.Join(spool, name))
		if err != nil {
			t.Fatalf("read spool file: %v", err)
		}
		for _, line := range bytes.Split(raw, []byte{'\n'}) {
			if len(bytes.TrimSpace(line)) == 0 {
				continue
			}
			var ev client.DevEvent
			if err := json.Unmarshal(line, &ev); err != nil {
				continue // spool records other shapes too; skip what does not parse
			}
			if ev.EventType != "" {
				out = append(out, ev)
			}
		}
	}
	return out
}

func turnEventsOnly(evs []client.DevEvent) []client.DevEvent {
	var out []client.DevEvent
	for _, ev := range evs {
		if ev.EventType == client.EventTurnStarted || ev.EventType == client.EventTurnCompleted {
			out = append(out, ev)
		}
	}
	return out
}

func stopPayload(session, transcript, agentID string) string {
	name := "Stop"
	agent := ""
	if agentID != "" {
		name = "SubagentStop"
		agent = `,"agent_id":"` + agentID + `","agent_type":"code-reviewer"`
	}
	return `{"hook_event_name":"` + name + `","session_id":"` + session +
		`","cwd":"/tmp","transcript_path":"` + transcript + `"` + agent +
		`,"last_assistant_message":"SENTINEL_LASTMSG do not bind me","stop_reason":"SENTINEL_STOPREASON"}`
}

// Three Stop firings over a growing transcript produce three pairs with disjoint
// numbers and contiguous, strictly-increasing indexes. This is the assertion that
// catches the double-count, the off-by-one cursor, and the missed turn — the
// counting assertion the Tier-2 duplicate-ActivityStarted bug taught us to write.
func TestRunHook_StopEmitsOneDisjointPairPerFiring(t *testing.T) {
	spool := turnEnv(t, true)
	transcript := filepath.Join(t.TempDir(), "transcript.jsonl")

	appendLines := func(body string) {
		f, err := os.OpenFile(transcript, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteString(body); err != nil {
			t.Fatal(err)
		}
		f.Close()
	}

	wantInputs := []int{100, 200, 300}
	for i, in := range wantInputs {
		appendLines(usageLine("claude-opus-4-8", in, i+1, false))
		var out bytes.Buffer
		RunHook("Stop", strings.NewReader(stopPayload("sess-turn", transcript, "")), &out, nopLogger())
		if out.Len() != 0 {
			t.Fatalf("Stop wrote to stdout (it can block a session): %q", out.String())
		}
	}

	turns := turnEventsOnly(spooledEvents(t, spool))
	if len(turns) != 6 {
		t.Fatalf("got %d turn events, want 6 (3 firings x 2 halves): %+v", len(turns), turns)
	}

	// Pairing and contiguity, per index.
	byIndex := map[int][]client.DevEvent{}
	for _, ev := range turns {
		if ev.TurnIndex == nil {
			t.Fatalf("turn event with no index: %+v", ev)
		}
		byIndex[*ev.TurnIndex] = append(byIndex[*ev.TurnIndex], ev)
	}
	if len(byIndex) != 3 {
		t.Errorf("got indexes %v, want exactly 3 contiguous from 0", keysOfIndex(byIndex))
	}
	for i, wantIn := range wantInputs {
		pair := byIndex[i]
		if len(pair) != 2 {
			t.Errorf("index %d has %d events, want exactly one Started and one Completed", i, len(pair))
			continue
		}
		var started, completed *client.DevEvent
		for j := range pair {
			switch pair[j].EventType {
			case client.EventTurnStarted:
				started = &pair[j]
			case client.EventTurnCompleted:
				completed = &pair[j]
			}
		}
		if started == nil || completed == nil {
			t.Errorf("index %d is not a Started+Completed pair: %+v", i, pair)
			continue
		}
		if completed.Tokens == nil || *completed.Tokens.Input != wantIn {
			t.Errorf("index %d input = %v, want %d — the windows overlap or the cursor did not advance",
				i, completed.Tokens, wantIn)
		}
		if *completed.Tokens.Output != i+1 {
			t.Errorf("index %d output = %d, want %d", i, *completed.Tokens.Output, i+1)
		}
		if completed.Model != "claude-opus-4-8" {
			t.Errorf("index %d model = %q", i, completed.Model)
		}
		if started.Tokens != nil {
			t.Errorf("index %d Started half carried tokens; usage belongs on Completed only", i)
		}
	}
}

// A Stop firing with nothing new in the transcript emits nothing: idempotent
// locally, so a duplicate firing cannot inflate the pair count.
func TestRunHook_StopWithNoNewUsageEmitsNothing(t *testing.T) {
	spool := turnEnv(t, true)
	transcript := filepath.Join(t.TempDir(), "transcript.jsonl")
	if err := os.WriteFile(transcript, []byte(usageLine("m1", 10, 2, false)), 0o600); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 3; i++ {
		var out bytes.Buffer
		RunHook("Stop", strings.NewReader(stopPayload("sess-idem", transcript, "")), &out, nopLogger())
		if out.Len() != 0 {
			t.Fatalf("Stop wrote to stdout: %q", out.String())
		}
	}

	turns := turnEventsOnly(spooledEvents(t, spool))
	if len(turns) != 2 {
		t.Errorf("got %d turn events across 3 firings, want 2 (one pair — the later firings had no new usage)", len(turns))
	}
}

// An empty transcript is not a turn. A pair emitted for it would inflate the
// count Phase 06 asserts against the real turn count.
func TestRunHook_StopWithEmptyTranscriptEmitsNothing(t *testing.T) {
	spool := turnEnv(t, true)
	transcript := filepath.Join(t.TempDir(), "transcript.jsonl")
	if err := os.WriteFile(transcript, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	RunHook("Stop", strings.NewReader(stopPayload("sess-empty", transcript, "")), &out, nopLogger())
	if out.Len() != 0 {
		t.Fatalf("Stop wrote to stdout: %q", out.String())
	}
	if turns := turnEventsOnly(spooledEvents(t, spool)); len(turns) != 0 {
		t.Errorf("got %d turn events for an empty transcript, want 0: %+v", len(turns), turns)
	}
}

// A missing transcript must degrade quietly: no events, no stdout, no failure.
func TestRunHook_StopWithMissingTranscriptIsQuiet(t *testing.T) {
	spool := turnEnv(t, true)
	var out bytes.Buffer
	RunHook("Stop", strings.NewReader(stopPayload("sess-missing", filepath.Join(t.TempDir(), "gone.jsonl"), "")), &out, nopLogger())
	if out.Len() != 0 {
		t.Fatalf("Stop wrote to stdout: %q", out.String())
	}
	if turns := turnEventsOnly(spooledEvents(t, spool)); len(turns) != 0 {
		t.Errorf("got %d turn events, want 0", len(turns))
	}
}

// A subagent's window and the main thread's must not consume each other's bytes,
// and the subagent's tokens must be attributed to it — the no-double-count case.
func TestRunHook_SubagentStopIsPartitionedFromMainThread(t *testing.T) {
	spool := turnEnv(t, true)
	transcript := filepath.Join(t.TempDir(), "transcript.jsonl")
	body := usageLine("claude-opus-4-8", 100, 10, false) +
		usageLine("claude-sonnet-4-8", 7000, 700, true) +
		usageLine("claude-opus-4-8", 50, 5, false)
	if err := os.WriteFile(transcript, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	RunHook("SubagentStop", strings.NewReader(stopPayload("sess-sub", transcript, "agt-1")), &out, nopLogger())
	if out.Len() != 0 {
		t.Fatalf("SubagentStop wrote to stdout: %q", out.String())
	}
	RunHook("Stop", strings.NewReader(stopPayload("sess-sub", transcript, "")), &out, nopLogger())
	if out.Len() != 0 {
		t.Fatalf("Stop wrote to stdout: %q", out.String())
	}

	turns := turnEventsOnly(spooledEvents(t, spool))
	if len(turns) != 4 {
		t.Fatalf("got %d turn events, want 4 (one subagent pair + one main pair): %+v", len(turns), turns)
	}

	var mainTokens, subTokens *client.Tokens
	for _, ev := range turns {
		if ev.EventType != client.EventTurnCompleted {
			continue
		}
		if ev.AgentID == "agt-1" {
			subTokens = ev.Tokens
			if ev.Metadata["agent_type"] != "code-reviewer" {
				t.Errorf("subagent turn not attributed: %+v", ev.Metadata)
			}
		} else {
			mainTokens = ev.Tokens
		}
	}
	if mainTokens == nil || subTokens == nil {
		t.Fatalf("missing a Completed half: main=%v sub=%v", mainTokens, subTokens)
	}
	// The partition: main sums only its own lines, the subagent only the sidechain
	// line, and together they equal the whole transcript exactly once.
	if *mainTokens.Input != 150 || *mainTokens.Output != 15 {
		t.Errorf("main turn = %d/%d, want 150/15 (sidechain excluded)", *mainTokens.Input, *mainTokens.Output)
	}
	if *subTokens.Input != 7000 || *subTokens.Output != 700 {
		t.Errorf("subagent turn = %d/%d, want 7000/700", *subTokens.Input, *subTokens.Output)
	}
	if got, want := *mainTokens.Input+*subTokens.Input, 7150; got != want {
		t.Errorf("Σ input = %d, want %d — tokens counted twice or dropped", got, want)
	}
}

// A SubagentStop with no agent_id would share the main thread's cursor and eat
// its window. Skipping is the safe answer: the main thread's accounting stays
// correct, and the subagent's tokens still reach the session rollup.
func TestRunHook_SubagentStopWithoutAgentIDIsSkipped(t *testing.T) {
	spool := turnEnv(t, true)
	transcript := filepath.Join(t.TempDir(), "transcript.jsonl")
	if err := os.WriteFile(transcript, []byte(usageLine("m1", 10, 2, true)), 0o600); err != nil {
		t.Fatal(err)
	}
	payload := `{"hook_event_name":"SubagentStop","session_id":"sess-noagent","cwd":"/tmp","transcript_path":"` + transcript + `"}`

	var out bytes.Buffer
	RunHook("SubagentStop", strings.NewReader(payload), &out, nopLogger())
	if out.Len() != 0 {
		t.Fatalf("SubagentStop wrote to stdout: %q", out.String())
	}
	if turns := turnEventsOnly(spooledEvents(t, spool)); len(turns) != 0 {
		t.Errorf("got %d turn events, want 0 (skipped rather than corrupting the main cursor)", len(turns))
	}

	// The main thread's cursor is untouched, so its own window is still intact.
	var out2 bytes.Buffer
	RunHook("Stop", strings.NewReader(stopPayload("sess-noagent", transcript, "")), &out2, nopLogger())
	if turns := turnEventsOnly(spooledEvents(t, spool)); len(turns) != 0 {
		t.Errorf("main Stop emitted %d turn events; the only line was sidechain so it should see none", len(turns))
	}
}

// With finops off the turn hooks are completely inert: no events, no stdout, no
// transcript read. This is also the opt-out assertion — nothing leaks when the
// posture says no.
func TestRunHook_StopIsInertWhenFinopsOff(t *testing.T) {
	spool := turnEnv(t, false)
	transcript := filepath.Join(t.TempDir(), "transcript.jsonl")
	if err := os.WriteFile(transcript, []byte(usageLine("claude-opus-4-8", 100, 10, false)), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, hook := range []string{"Stop", "SubagentStop"} {
		var out bytes.Buffer
		RunHook(hook, strings.NewReader(stopPayload("sess-off", transcript, map[string]string{"Stop": "", "SubagentStop": "agt-1"}[hook])), &out, nopLogger())
		if out.Len() != 0 {
			t.Fatalf("%s with finops off wrote to stdout: %q", hook, out.String())
		}
	}
	evs := spooledEvents(t, spool)
	if turns := turnEventsOnly(evs); len(turns) != 0 {
		t.Errorf("finops off emitted %d turn events, want 0", len(turns))
	}
	// And nothing else either: a turn hook has no lifecycle event of its own.
	for _, ev := range evs {
		t.Errorf("finops off spooled an event from a turn hook: %s", ev.EventType)
	}
}

// A turn hook must never spool a lifecycle event of its own — Map returns false
// for these hooks by design, and RunHook must not fall through to Observe.
func TestRunHook_StopSpoolsOnlyTurnEvents(t *testing.T) {
	spool := turnEnv(t, true)
	transcript := filepath.Join(t.TempDir(), "transcript.jsonl")
	if err := os.WriteFile(transcript, []byte(usageLine("claude-opus-4-8", 100, 10, false)), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	RunHook("Stop", strings.NewReader(stopPayload("sess-only", transcript, "")), &out, nopLogger())

	for _, ev := range spooledEvents(t, spool) {
		if ev.EventType != client.EventTurnStarted && ev.EventType != client.EventTurnCompleted {
			t.Errorf("Stop spooled a %s event; it should emit the turn pair and nothing else", ev.EventType)
		}
	}
}

// The Stop payload's content fields must not reach the spool. They are not bound
// on HookEvent at all, so this is a regression guard on that absence: a future
// contributor who adds last_assistant_message to the struct fails here.
func TestRunHook_StopPayloadContentNeverSpooled(t *testing.T) {
	spool := turnEnv(t, true)
	transcript := filepath.Join(t.TempDir(), "transcript.jsonl")
	if err := os.WriteFile(transcript, []byte(usageLine("claude-opus-4-8", 100, 10, false)), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	RunHook("Stop", strings.NewReader(stopPayload("sess-content", transcript, "")), &out, nopLogger())

	for _, ev := range spooledEvents(t, spool) {
		raw, _ := json.Marshal(ev)
		for _, banned := range []string{"SENTINEL_LASTMSG", "SENTINEL_STOPREASON"} {
			if strings.Contains(string(raw), banned) {
				t.Errorf("INV-2 breach: Stop payload content %q reached the spool: %s", banned, raw)
			}
		}
	}
}

func keysOfIndex(m map[int][]client.DevEvent) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
