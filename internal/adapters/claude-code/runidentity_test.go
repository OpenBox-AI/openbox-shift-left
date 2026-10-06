package claudecode

import (
	"bytes"
	"log"
	"strconv"
	"strings"
	"testing"

	obgit "github.com/openbox-ai/openbox-shift-left/internal/adapters/common/git"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
)

func sessionStartPayload(sid, source string) string {
	return `{"hook_event_name":"SessionStart","session_id":"` + sid + `","cwd":"/tmp","source":"` + source + `"}`
}

func preToolUsePayload(sid string, n int) string {
	return `{"hook_event_name":"PreToolUse","session_id":"` + sid + `","cwd":"/tmp","tool_name":"Bash","tool_input":{"command":"echo ` +
		strconv.Itoa(n) + `"}}`
}

func findLastEventType(events []client.DevEvent, et client.EventType) client.DevEvent {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].EventType == et {
			return events[i]
		}
	}
	return client.DevEvent{}
}

// TestSourceBumpTable is the five-source table (only resume bumps,
// live-measured: a `/clear` mints a DIFFERENT session id -- Claude Code
// 2.1.263, ~46ms apart -- so it is never asked to continue a run it never
// saw; there is no prior run for it to bump). It drives isBumpSource, the
// actual function hookrun.go calls, not a reimplementation of it.
func TestSourceBumpTable(t *testing.T) {
	cases := map[string]bool{
		"startup": false,
		"resume":  true,
		"clear":   false, // mints a NEW session id; no prior run exists to continue
		"compact": false, // fires no SessionEnd; there is no sealed run to continue from
		"fork":    false, // carries a fresh id of its own; bumping would invent a generation
		"bogus":   false, // an unrecognized value must never read as a bump
	}
	for source, wantBump := range cases {
		if got := isBumpSource(source); got != wantBump {
			t.Errorf("isBumpSource(%q) = %v, want %v", source, got, wantBump)
		}
	}
}

// TestSessionStartClearWithNoRecordStaysGeneration0 is the table's added sub-case:
// a SessionStart(source=clear) on a session id with NO run record (this is
// what every `/clear` looks like, since it always arrives on a
// session id core has never seen) must NOT be treated as a continuation --
// generation 0, run_id falls back to the session id on the wire, and no
// continued_from_run_id.
func TestSessionStartClearWithNoRecordStaysGeneration0(t *testing.T) {
	isolateConfig(t)
	t.Setenv(envAgentID, testAgentID)
	spoolDir := t.TempDir()
	t.Setenv("OPENBOX_SPOOL_DIR", spoolDir)
	t.Setenv("OPENBOX_SESSION_DIR", t.TempDir())

	sid := "sess-fresh-clear"
	RunHook("SessionStart", strings.NewReader(sessionStartPayload(sid, "clear")), &bytes.Buffer{}, nopLogger())

	events := readSpooledEvents(t, spoolDir, sid)
	ev := findLastEventType(events, client.EventSessionStarted)
	if ev.RunGeneration != 0 {
		t.Errorf("run_generation = %d, want 0 (clear never bumps -- it minted a new session id, so there is no prior run)", ev.RunGeneration)
	}
	if ev.RunID != "" {
		t.Errorf("run_id = %q, want empty (generation 0; the wire falls back to the session id, client.runIDFor)", ev.RunID)
	}
	if ev.ContinuedFromRunID != "" {
		t.Errorf("continued_from_run_id = %q, want empty", ev.ContinuedFromRunID)
	}
}

// TestSessionStartResumeBumpsOnTheSecondInvocation: two
// SessionStart(source=resume) hooks against one temp registry produce
// generations 1 then 2, two DISTINCT run_id UUIDs neither equal to the
// session id, and continued_from_run_id = the session id then = the first
// UUID. Read and write paths are asserted SEPARATELY, and the assertion that
// matters is on the SECOND invocation (a test that runs only once cannot see
// a defect that only the second run exposes). A version of this
// test that passes with the write stubbed out is a defect -- the independent
// re-read below is what rules that out.
func TestSessionStartResumeBumpsOnTheSecondInvocation(t *testing.T) {
	isolateConfig(t)
	t.Setenv(envAgentID, testAgentID)
	spoolDir := t.TempDir()
	t.Setenv("OPENBOX_SPOOL_DIR", spoolDir)
	sessionDir := t.TempDir()
	t.Setenv("OPENBOX_SESSION_DIR", sessionDir)

	sid := "sess-resume-a"
	invoke := func() client.DevEvent {
		RunHook("SessionStart", strings.NewReader(sessionStartPayload(sid, "resume")), &bytes.Buffer{}, nopLogger())
		return findLastEventType(readSpooledEvents(t, spoolDir, sid), client.EventSessionStarted)
	}

	first := invoke()
	if first.RunGeneration != 1 {
		t.Fatalf("first resume: run_generation = %d, want 1", first.RunGeneration)
	}
	if first.RunID == "" || first.RunID == sid {
		t.Fatalf("first resume: run_id = %q, want a fresh UUID distinct from the session id", first.RunID)
	}
	if first.ContinuedFromRunID != sid {
		t.Errorf("first resume: continued_from_run_id = %q, want the session id %q", first.ContinuedFromRunID, sid)
	}

	// The READ path independently: a fresh RunStore value, not anything the
	// write above could have cached in-process.
	readBack, err := (obgit.RunStore{Dir: obgit.RunDir(sessionDir)}).Read(sid)
	if err != nil {
		t.Fatalf("independent read after the first bump: %v", err)
	}
	if readBack.RunID != first.RunID || readBack.Generation != 1 {
		t.Fatalf("registry after the first bump = %+v, want it to agree with the emitted event (run_id=%q, generation=1)",
			readBack, first.RunID)
	}

	// The assertion that matters: the SECOND invocation.
	second := invoke()
	if second.RunGeneration != 2 {
		t.Fatalf("second resume: run_generation = %d, want 2 (a version that resets or fails to advance is a defect)", second.RunGeneration)
	}
	if second.RunID == "" || second.RunID == sid || second.RunID == first.RunID {
		t.Fatalf("second resume: run_id = %q, want a THIRD distinct UUID (first bump minted %q)", second.RunID, first.RunID)
	}
	if second.ContinuedFromRunID != first.RunID {
		t.Errorf("second resume: continued_from_run_id = %q, want the first bump's run id %q", second.ContinuedFromRunID, first.RunID)
	}
}

// TestRunIdentityChainAcrossGenerations: N SessionStart(resume) bumps
// with a PreToolUse between each. The chain is built from the EVENTS across
// generations, not read back from the record (record loss restarts the local counter
// after record loss while the chain still points at the sealed run, so
// asserting run_generation == chain length fleet-wide would be wrong).
func TestRunIdentityChainAcrossGenerations(t *testing.T) {
	isolateConfig(t)
	t.Setenv(envAgentID, testAgentID)
	spoolDir := t.TempDir()
	t.Setenv("OPENBOX_SPOOL_DIR", spoolDir)
	t.Setenv("OPENBOX_SESSION_DIR", t.TempDir())

	const n = 3
	sid := "sess-chain"
	var starts, calls []client.DevEvent
	for k := 0; k < n; k++ {
		RunHook("SessionStart", strings.NewReader(sessionStartPayload(sid, "resume")), &bytes.Buffer{}, nopLogger())
		RunHook("PreToolUse", strings.NewReader(preToolUsePayload(sid, k)), &bytes.Buffer{}, nopLogger())

		events := readSpooledEvents(t, spoolDir, sid)
		starts = append(starts, findLastEventType(events, client.EventSessionStarted))
		calls = append(calls, findLastEventType(events, client.EventToolCall))
	}

	for k := 0; k < n; k++ {
		// (a) PreToolUse(k) reads the SAME record the kth SessionStart just bumped.
		if starts[k].RunID != calls[k].RunID {
			t.Errorf("k=%d: SessionStart run_id=%q, PreToolUse run_id=%q, want equal", k, starts[k].RunID, calls[k].RunID)
		}
		// (c) run_generation increments by exactly 1 per bump, 0 per non-bump hook.
		if starts[k].RunGeneration != k+1 {
			t.Errorf("k=%d: SessionStart run_generation=%d, want %d", k, starts[k].RunGeneration, k+1)
		}
		if calls[k].RunGeneration != k+1 {
			t.Errorf("k=%d: PreToolUse run_generation=%d, want %d (a non-bumping hook must not move it)", k, calls[k].RunGeneration, k+1)
		}
		// (b) continued_from_run_id on WorkflowStarted(k) == run_id on PreToolUse(k-1).
		want := sid
		if k > 0 {
			want = calls[k-1].RunID
		}
		if starts[k].ContinuedFromRunID != want {
			t.Errorf("k=%d: continued_from_run_id=%q, want %q", k, starts[k].ContinuedFromRunID, want)
		}
	}

	// (d) all run ids pairwise distinct, none equal to the session id.
	seen := map[string]bool{}
	for k, ev := range starts {
		if ev.RunID == "" || ev.RunID == sid {
			t.Errorf("k=%d: run_id=%q, want non-empty and distinct from the session id", k, ev.RunID)
		}
		if seen[ev.RunID] {
			t.Errorf("k=%d: run_id=%q reused an earlier generation's id", k, ev.RunID)
		}
		seen[ev.RunID] = true
	}
}

// TestLatchEscapesOnlyABumpedGeneration: a HALT latched at
// generation 0 still replays across a non-bumping SessionStart (b: startup),
// and stops replaying once the run is actually bumped (a: resume) -- because
// the latch is keyed by the run id, which only a bump changes.
func TestLatchEscapesOnlyABumpedGeneration(t *testing.T) {
	isolateConfig(t)
	t.Setenv(envAgentID, testAgentID)
	t.Setenv("OPENBOX_SPOOL_DIR", t.TempDir())
	t.Setenv("OPENBOX_SESSION_DIR", t.TempDir())
	t.Setenv(devconfig.EnvHaltDir, t.TempDir())
	t.Setenv(envEnforce, "1")

	sid := "sess-halt-scope"
	hookflow.WriteSessionHalt(nopLogger(), sid, client.Evaluation{Reason: "org kill switch", PolicyID: "p-1"})

	runPreToolUse := func() (stdout, stderr string) {
		var outBuf, errBuf bytes.Buffer
		logger := log.New(&errBuf, "", 0)
		RunHook("PreToolUse", strings.NewReader(preToolUsePayload(sid, 0)), &outBuf, logger)
		return outBuf.String(), errBuf.String()
	}

	// (b) SessionStart(startup) never bumps: the next gated call is still
	// generation 0 -- the pre-existing latch must still replay.
	RunHook("SessionStart", strings.NewReader(sessionStartPayload(sid, "startup")), &bytes.Buffer{}, nopLogger())
	out, errOut := runPreToolUse()
	if !strings.Contains(out, `"continue":false`) {
		t.Fatalf("generation 0 (after a non-bumping SessionStart) did not replay the latch; stdout=%q stderr=%q", out, errOut)
	}
	if strings.Contains(errOut, "source=evaluate") {
		t.Errorf("a replayed HALT must not also reach the evaluator; stderr=%q", errOut)
	}

	// (a) SessionStart(resume) bumps to generation 1: the next gated call
	// names a run id no latch file exists for, and must reach the evaluator
	// exactly once instead of replaying.
	RunHook("SessionStart", strings.NewReader(sessionStartPayload(sid, "resume")), &bytes.Buffer{}, nopLogger())
	out, errOut = runPreToolUse()
	if strings.Contains(out, `"continue":false`) {
		t.Fatalf("generation 1 replayed the OLD generation's latch; stdout=%q", out)
	}
	if got := strings.Count(errOut, "enforce decision:"); got != 1 {
		t.Fatalf("generation 1 logged %d enforce decision(s), want exactly 1 (the evaluator path); stderr=%q", got, errOut)
	}
	if strings.Contains(errOut, "source=session-halt") {
		t.Errorf("generation 1 replayed instead of reaching the evaluator; stderr=%q", errOut)
	}
}
