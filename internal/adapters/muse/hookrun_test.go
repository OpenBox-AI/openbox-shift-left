package muse

import (
	"bytes"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/client/fakecore"
	"github.com/openbox-ai/openbox-shift-left/internal/client/workloadauth"
	"github.com/openbox-ai/openbox-shift-left/internal/trace"
)

const testAgentID = "7f3c9b2e-1111-5000-a000-000000000002"

var testDID = mustTestDID()

func mustTestDID() string {
	did, err := devconfig.AttributionDIDFor(testAgentID)
	if err != nil {
		panic(err)
	}
	return did
}

func testMapper() Mapper {
	m := NewMapper(Identity{DeveloperDID: testDID})
	m.Now = func() time.Time { return time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC) }
	m.NewID = func() string { return "evt-fixed" }
	return m
}

// bindForTest binds for the length of one case: every production caller of this
// adapter runs under `openbox hook muse`, which binds before anything resolves
// a credential, and identity does not resolve at all without it.
func bindForTest(t *testing.T) {
	t.Helper()
	release, err := devconfig.BindProvider("muse")
	if err != nil {
		t.Fatalf("bind muse: %v", err)
	}
	t.Cleanup(release)
}

// setHookEnv isolates one hook environment and returns the spool dir. With no
// core reachable a gated call is denied per call, but its escalation was never
// attempted, so the gate's own observe copy still lands in the local spool.
func setHookEnv(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	spool := filepath.Join(dir, "spool")
	t.Setenv(devconfig.EnvHome, dir)
	// A SessionStart reads Muse's config, and its auth lock is taken under
	// the config home: keep that inside the case's own directory.
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "xdg-config"))
	bindForTest(t)
	t.Setenv(devconfig.EnvAgentID, testAgentID)
	t.Setenv(devconfig.EnvSpoolDir, spool)
	t.Setenv(devconfig.EnvConfigPath, filepath.Join(dir, "none.json"))
	t.Setenv("OPENBOX_SESSION_DIR", filepath.Join(dir, "sessions"))
	t.Setenv("OPENBOX_ADVISORY_FILE", filepath.Join(dir, "advisories.jsonl"))
	t.Setenv("OPENBOX_FINDINGS_CURSOR", filepath.Join(dir, "findings.cursor"))
	t.Setenv(devconfig.EnvEnforcementFile, filepath.Join(dir, "enforcements.jsonl"))
	t.Setenv(devconfig.EnvPendingApprovalDir, filepath.Join(dir, "pending-approvals"))
	t.Setenv(devconfig.EnvHaltDir, filepath.Join(dir, "halts"))
	t.Setenv(devconfig.EnvRealtime, "0")
	t.Setenv(devconfig.EnvContentCapture, "0")
	t.Cleanup(trace.SetDefault(&trace.Writer{Dir: filepath.Join(dir, "trace"), Proc: "test"}))
	return spool
}

// serveCore starts a fake core and points the hook at it.
func serveCore(t *testing.T, s fakecore.Script) *fakecore.Server {
	t.Helper()
	f := fakecore.New(t, s)
	t.Setenv(devconfig.EnvBaseURL, f.URL())
	t.Setenv(devconfig.EnvAPIKeyDirect, fakecore.APIKey())
	key, err := workloadauth.NormalizePrivateKey(fakecore.WorkloadPrivateKey())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(devconfig.EnvWorkloadPrivateKey, key)
	if p, err := devconfig.WorkloadTokenCachePath(); err == nil {
		_ = os.Remove(p)
	}
	// A refusal means the hook put something on the wire that core would have
	// rejected: a spans key, a malformed gate row, a route core does not serve.
	t.Cleanup(func() {
		if refused := f.Rejections(); len(refused) > 0 {
			t.Errorf("the fake refused %d request(s): %s", len(refused), strings.Join(refused, " | "))
		}
	})
	return f
}

func runHook(t *testing.T, sub, payload string) (stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	RunHook(sub, strings.NewReader(payload), &out, log.New(&errb, "openbox hook: ", 0))
	return out.String(), errb.String()
}

// endSession runs a SessionEnd and then the flusher it hands delivery to. In
// production that flusher is a detached process; a test binary never spawns
// one (that would re-run the suite), so the test runs its one step inline.
func endSession(t *testing.T, payload, session string) {
	t.Helper()
	runHook(t, "SessionEnd", payload)
	flushSession(t, session)
}

// flushSession is the detached flusher's own step for one session.
func flushSession(t *testing.T, session string) {
	t.Helper()
	t.Setenv(hookflow.EnvFlushSession, session)
	runHook(t, "flush", "")
}

// fixtureAs is fixture with the subagent session of a capture moved onto the
// same session too, for a test that needs a subagent-shaped payload (a
// PermissionRequest, observed only there) to belong to one latched run.
func fixtureAs(t *testing.T, name, session string) string {
	t.Helper()
	return strings.ReplaceAll(fixture(t, name, session), "sess-0002", session)
}

// fixture reads a captured payload (scrubbed, see testdata/README.md), moved
// onto a session of the caller's.
func fixture(t *testing.T, name, session string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name+".json"))
	if err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	if session == "" {
		return string(raw)
	}
	return strings.ReplaceAll(string(raw), "sess-0001", session)
}

const allowJSON = `{"verdict":"allow"}`

func verdictJSON(verdict, reason string) string {
	return `{"verdict":"` + verdict + `","reason":"` + reason + `","policy_id":"p-1"}`
}

// gateRows are the wire rows of the model-call gate, in arrival order.
func gateRows(f *fakecore.Server) []fakecore.Received {
	var out []fakecore.Received
	for _, r := range f.Inbox() {
		if r.ActivityType() == client.ActivityTypeModelCallGate {
			out = append(out, r)
		}
	}
	return out
}

func rowCount(f *fakecore.Server, eventType string) int {
	n := 0
	for _, r := range f.Inbox() {
		if r.EventType() == eventType {
			n++
		}
	}
	return n
}

func haltLatches(t *testing.T) []string {
	t.Helper()
	dir, _ := os.LookupEnv(devconfig.EnvHaltDir)
	latches, _ := filepath.Glob(filepath.Join(dir, "*"))
	return latches
}

func decodeStdout(t *testing.T, stdout string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(stdout), &m); err != nil {
		t.Fatalf("stdout is not one JSON document: %v (%q)", err, stdout)
	}
	return m
}

func TestNoIdentityIsSilentEvenOnAGatedEvent(t *testing.T) {
	setHookEnv(t)
	t.Setenv(devconfig.EnvAgentID, "")
	stdout, stderr := runHook(t, "PreLLMCall", fixture(t, "pre-llm-call", "s-noid"))
	if stdout != "" || !strings.Contains(stderr, "no identity") {
		t.Fatalf("an unconfigured machine is governance inactive: stdout=%q stderr=%q", stdout, stderr)
	}
}

// A payload that cannot be parsed on a gated event is refused with the event's
// own answer, never a silent exit 0, which Muse reads as an allow.
func TestUnreadablePayloadOnAGatedEventIsRefused(t *testing.T) {
	setHookEnv(t)
	for _, hook := range []string{"UserPromptSubmit", "PreToolUse", "PermissionRequest", "PreLLMCall"} {
		for _, payload := range []string{"", "{not json", "[1,2]", "null"} {
			stdout, _ := runHook(t, hook, payload)
			if strings.TrimSpace(stdout) == "" {
				t.Errorf("%s with payload %q wrote nothing; that is an allow", hook, payload)
				continue
			}
			decodeStdout(t, stdout)
		}
	}
	if latches := haltLatches(t); len(latches) != 0 {
		t.Errorf("an unreadable payload latched the run: %v", latches)
	}
	// A non-gated event stays silent.
	if stdout, _ := runHook(t, "PostToolUse", "{not json"); stdout != "" {
		t.Errorf("PostToolUse wrote %q", stdout)
	}
}

func TestUnmappedEventsAreSilentNoOps(t *testing.T) {
	spool := setHookEnv(t)
	for _, hook := range []string{"PreCompact", "PostCompact", "Notification", "PostToolBatch", "Interrupt"} {
		stdout, stderr := runHook(t, hook, fixture(t, "stop", "s-noop"))
		if stdout != "" || strings.Contains(stderr, "unknown") {
			t.Errorf("%s: stdout=%q stderr=%q", hook, stdout, stderr)
		}
	}
	if entries, _ := os.ReadDir(spool); len(entries) != 0 {
		t.Errorf("an unmapped event spooled something: %v", entries)
	}
	if _, stderr := runHook(t, "NotAnEvent", "{}"); !strings.Contains(stderr, "unknown Muse hook") {
		t.Errorf("an unknown event name was not reported: %q", stderr)
	}
}

// No core reachable: every gated event denies its own call with its own answer
// and spools the observe copy, and nothing latches.
func TestGatedEventsDenyWhenCoreIsUnreachable(t *testing.T) {
	spool := setHookEnv(t)
	cases := []struct{ hook, file, key string }{
		{"UserPromptSubmit", "user-prompt-submit", "decision"},
		{"PreToolUse", "pre-tool-use-bash", "hookSpecificOutput"},
		{"PermissionRequest", "permission-request", "hookSpecificOutput"},
		{"PreLLMCall", "pre-llm-call", "decision"},
	}
	for _, tc := range cases {
		stdout, _ := runHook(t, tc.hook, fixture(t, tc.file, "s-down"))
		m := decodeStdout(t, stdout)
		if _, ok := m[tc.key]; !ok {
			t.Errorf("%s: answer %q has no %q", tc.hook, stdout, tc.key)
		}
	}
	if l := haltLatches(t); len(l) != 0 {
		t.Errorf("an outage latched the run: %v", l)
	}
	if entries, _ := os.ReadDir(spool); len(entries) == 0 {
		t.Error("the observe copies were not spooled")
	}
}

// A DENY on PreLLMCall blocks the call and leaves the started row only: the
// call never ran, so no completion is reported, and none is fabricated.
func TestModelCallDenyBlocksAndLeavesTheStartedRowOnly(t *testing.T) {
	setHookEnv(t)
	f := serveCore(t, fakecore.Script{VerdictsByActivityType: map[string]string{
		client.ActivityTypeModelCallGate: verdictJSON("block", "model egress not permitted"),
	}})

	stdout, _ := runHook(t, "PreLLMCall", fixture(t, "pre-llm-call", "s-deny"))
	m := decodeStdout(t, stdout)
	if m["decision"] != "block" || !strings.Contains(m["reason"].(string), "model egress not permitted") {
		t.Fatalf("answer = %q, want the documented block with the policy reason", stdout)
	}
	if strings.Contains(stdout, `"continue"`) {
		t.Fatalf("answer carries continue: %q", stdout)
	}
	// A deny is a per-call verdict, never a latch.
	if l := haltLatches(t); len(l) != 0 {
		t.Fatalf("a BLOCK latched the run: %v", l)
	}

	// The session end drains whatever is still queued; the deny leaves no
	// completion behind it.
	endSession(t, fixture(t, "session-end", "s-deny"), "s-deny")
	rows := gateRows(f)
	if len(rows) != 1 {
		t.Fatalf("gate rows = %d, want exactly the started row", len(rows))
	}
	if rows[0].EventType() != fakecore.WireActivityStarted {
		t.Fatalf("the one gate row is %q, want %q", rows[0].EventType(), fakecore.WireActivityStarted)
	}
	if !strings.Contains(rows[0].ActivityID(), ":llmgate:turn-0001:0:1.1") {
		t.Errorf("activity id = %q, want the request id and attempt in the llmgate namespace", rows[0].ActivityID())
	}
}

// An allowed PreLLMCall followed by its PostLLMCall is exactly two rows under
// one activity id, both model_call_gate, neither carrying usage or a turn.
func TestModelCallAllowThenFinishedIsOnePair(t *testing.T) {
	setHookEnv(t)
	f := serveCore(t, fakecore.Script{Default: allowJSON})

	if stdout, _ := runHook(t, "PreLLMCall", fixture(t, "pre-llm-call", "s-pair")); stdout != "" {
		t.Fatalf("an allowed model call must write nothing, got %q", stdout)
	}
	if stdout, _ := runHook(t, "PostLLMCall", fixture(t, "post-llm-call", "s-pair")); stdout != "" {
		t.Fatalf("PostLLMCall must write nothing, got %q", stdout)
	}
	endSession(t, fixture(t, "session-end", "s-pair"), "s-pair")

	rows := gateRows(f)
	if len(rows) != 2 {
		t.Fatalf("gate rows = %d, want a started and a completed", len(rows))
	}
	if rows[0].ActivityID() != rows[1].ActivityID() || rows[0].ActivityID() == "" {
		t.Fatalf("the halves do not share one activity id: %q vs %q", rows[0].ActivityID(), rows[1].ActivityID())
	}
	if rows[0].EventType() != fakecore.WireActivityStarted || rows[1].EventType() != fakecore.WireActivityCompleted {
		t.Fatalf("halves = %q, %q", rows[0].EventType(), rows[1].EventType())
	}
	for _, r := range rows {
		raw := string(r.Raw)
		for _, forbidden := range []string{`"tokens"`, `"usage"`, `"turn_index"`, `input_tokens`, `output_tokens`, `"spans"`} {
			if strings.Contains(raw, forbidden) {
				t.Errorf("a gate row carries %s: %s", forbidden, raw)
			}
		}
		if r.ActivityType() == "llm_completion" {
			t.Errorf("a gate row is labelled llm_completion: %s", raw)
		}
	}
	out, _ := rows[1].Body["activity_output"].(map[string]any)
	// A success with a null finish_reason carries no finish_reason at all.
	if _, present := out["finish_reason"]; out["status"] != "completed" || present || out["response_id"] != "resp_0001" {
		t.Errorf("completed half output = %v", out)
	}
}

// A PostLLMCall that never comes (a crash, a cancel) leaves the started row only.
func TestMissingModelCallFinishedLeavesTheStartedRowOnly(t *testing.T) {
	setHookEnv(t)
	f := serveCore(t, fakecore.Script{Default: allowJSON})
	runHook(t, "PreLLMCall", fixture(t, "pre-llm-call", "s-nopost"))
	endSession(t, fixture(t, "session-end", "s-nopost"), "s-nopost")
	rows := gateRows(f)
	if len(rows) != 1 || rows[0].EventType() != fakecore.WireActivityStarted {
		t.Fatalf("gate rows = %d, want the started row only", len(rows))
	}
}

// Core down: this call is denied with the delivery reason, the next attempt is
// a fresh one (it reaches core once core is back), and nothing latches.
func TestOutageDeniesOnlyItsOwnModelCall(t *testing.T) {
	setHookEnv(t)
	f := serveCore(t, fakecore.Script{Default: allowJSON})

	f.SetOutage(true)
	stdout, _ := runHook(t, "PreLLMCall", fixture(t, "pre-llm-call", "s-outage"))
	if m := decodeStdout(t, stdout); m["decision"] != "block" {
		t.Fatalf("an outage must deny this call, got %q", stdout)
	}
	if l := haltLatches(t); len(l) != 0 {
		t.Fatalf("a delivery failure latched the run: %v", l)
	}

	f.SetOutage(false)
	next := strings.ReplaceAll(fixture(t, "pre-llm-call", "s-outage"), `"request_id": "turn-0001:0:1"`, `"request_id": "turn-0001:0:2"`)
	before := f.Hits()
	stdout, _ = runHook(t, "PreLLMCall", next)
	if strings.TrimSpace(stdout) != "" {
		t.Fatalf("the next call gets a fresh attempt and core allows it, got %q", stdout)
	}
	if f.Hits() == before {
		t.Fatal("the next call never reached core: the failure carried over")
	}
}

// A HALT latches the run, and every later gated call of that run, tools and
// model calls alike, is denied with zero round trips.
func TestHaltLatchesTheRunAndDeniesLaterCallsWithoutTheNetwork(t *testing.T) {
	setHookEnv(t)
	f := serveCore(t, fakecore.Script{Default: verdictJSON("halt", "policy violation")})

	stdout, _ := runHook(t, "PreToolUse", fixture(t, "pre-tool-use-bash", "s-halt"))
	m := decodeStdout(t, stdout)
	hso := m["hookSpecificOutput"].(map[string]any)
	if hso["permissionDecision"] != "deny" {
		t.Fatalf("a HALT renders a plain deny, got %q", stdout)
	}
	if _, ok := m["continue"]; ok {
		t.Fatalf("a HALT on PreToolUse must not carry continue: %q", stdout)
	}
	if len(haltLatches(t)) != 1 {
		t.Fatalf("a real HALT must write exactly one latch, got %v", haltLatches(t))
	}

	before := f.Hits()
	for _, tc := range []struct{ hook, file string }{
		{"PreToolUse", "pre-tool-use-read"},
		{"PreLLMCall", "pre-llm-call"},
		{"UserPromptSubmit", "user-prompt-submit"},
		{"PermissionRequest", "permission-request"},
	} {
		out, _ := runHook(t, tc.hook, fixtureAs(t, tc.file, "s-halt"))
		if strings.TrimSpace(out) == "" {
			t.Errorf("%s after a HALT wrote nothing; the latch must still refuse", tc.hook)
			continue
		}
		decodeStdout(t, out)
	}
	if got := f.Hits() - before; got != 0 {
		t.Errorf("latched calls made %d round trips, want 0", got)
	}
}

// A latch is keyed on the run, so a SessionStart with source=resume, which
// continues the session as a new run (continue-as-new), is unlatched.
func TestResumeSessionStartOpensAnUnlatchedRun(t *testing.T) {
	setHookEnv(t)
	serveCore(t, fakecore.Script{Default: allowJSON})
	const sess = "s-run-resume"

	// A latch on the session's first run.
	hookflow.WriteSessionHalt(log.New(&bytes.Buffer{}, "", 0), sess,
		client.Evaluation{Verdict: client.VerdictHalt, Reason: "seeded", PolicyID: "p"})
	if out, _ := runHook(t, "PreLLMCall", fixture(t, "pre-llm-call", sess)); strings.TrimSpace(out) == "" {
		t.Fatal("the seeded latch did not refuse")
	}

	start := strings.ReplaceAll(fixture(t, "session-start-startup", sess), `"source": "startup"`, `"source": "resume"`)
	runHook(t, "SessionStart", start)

	if out, _ := runHook(t, "PreLLMCall", fixture(t, "pre-llm-call", sess)); strings.TrimSpace(out) != "" {
		t.Fatalf("source=resume must open a new, unlatched run, got %q", out)
	}
}

// A clear is a new session id, not a continuation: it never bumps a run, so a
// latch on the same id (which a clear never reuses) would still hold.
func TestClearSessionStartDoesNotOpenANewRun(t *testing.T) {
	setHookEnv(t)
	serveCore(t, fakecore.Script{Default: allowJSON})
	const sess = "s-run-clear"
	hookflow.WriteSessionHalt(log.New(&bytes.Buffer{}, "", 0), sess,
		client.Evaluation{Verdict: client.VerdictHalt, Reason: "seeded", PolicyID: "p"})
	start := strings.ReplaceAll(fixture(t, "session-start-startup", sess), `"source": "startup"`, `"source": "clear"`)
	runHook(t, "SessionStart", start)
	if out, _ := runHook(t, "PreLLMCall", fixture(t, "pre-llm-call", sess)); strings.TrimSpace(out) == "" {
		t.Fatal("a clear bumped the run of an id it would never reuse")
	}
}

// A startup does not open a new run: the latch still holds.
func TestStartupDoesNotClearALatch(t *testing.T) {
	setHookEnv(t)
	serveCore(t, fakecore.Script{Default: allowJSON})
	hookflow.WriteSessionHalt(log.New(&bytes.Buffer{}, "", 0), "s-startup",
		client.Evaluation{Verdict: client.VerdictHalt, Reason: "seeded", PolicyID: "p"})
	runHook(t, "SessionStart", fixture(t, "session-start-startup", "s-startup"))
	if out, _ := runHook(t, "PreLLMCall", fixture(t, "pre-llm-call", "s-startup")); strings.TrimSpace(out) == "" {
		t.Fatal("a startup un-halted a latched run")
	}
}

// One spurious 401 is absorbed by the client's forced-refresh retry and the
// gate still gets a real verdict.
func TestSpurious401IsAbsorbedByAForcedRefresh(t *testing.T) {
	setHookEnv(t)
	f := serveCore(t, fakecore.Script{VerdictsByActivityType: map[string]string{
		client.ActivityTypeModelCallGate: verdictJSON("block", "denied after refresh"),
	}})
	f.SpuriousUnauthorizedOnce()
	stdout, _ := runHook(t, "PreLLMCall", fixture(t, "pre-llm-call", "s-401"))
	m := decodeStdout(t, stdout)
	if !strings.Contains(m["reason"].(string), "denied after refresh") {
		t.Fatalf("the verdict after the retry was not applied: %q", stdout)
	}
}

// REQUIRE_APPROVAL on a model call renders a refusal, never an ask.
func TestRequireApprovalOnAModelCallDenies(t *testing.T) {
	setHookEnv(t)
	t.Setenv(devconfig.EnvApprovalHold, "50")
	f := serveCore(t, fakecore.Script{VerdictsByActivityType: map[string]string{
		client.ActivityTypeModelCallGate: verdictJSON("require_approval", "needs a human"),
	}})
	f.Approval(func(fakecore.Received) (int, string) { return 404, "" })
	stdout, _ := runHook(t, "PreLLMCall", fixture(t, "pre-llm-call", "s-approval"))
	m := decodeStdout(t, stdout)
	if m["decision"] != "block" {
		t.Fatalf("REQUIRE_APPROVAL must refuse, got %q", stdout)
	}
	if strings.Contains(stdout, `"ask"`) {
		t.Fatalf("an ask was rendered: %q", stdout)
	}
}

// PermissionRequest is deny-or-nothing whatever core says.
func TestPermissionRequestNeverAllows(t *testing.T) {
	setHookEnv(t)
	serveCore(t, fakecore.Script{Default: allowJSON})
	if out, _ := runHook(t, "PermissionRequest", fixtureAs(t, "permission-request", "s-perm")); strings.TrimSpace(out) != "" {
		t.Fatalf("an allowed PermissionRequest must write nothing so the human is still asked, got %q", out)
	}
	serveCore(t, fakecore.Script{Default: verdictJSON("block", "no")})
	out, _ := runHook(t, "PermissionRequest", fixtureAs(t, "permission-request", "s-perm"))
	if strings.Contains(out, `"allow"`) || !strings.Contains(out, `"behavior":"deny"`) {
		t.Fatalf("PermissionRequest answer = %q", out)
	}
}

// A blocked tool leaves the started row only; a tool that ran leaves the pair.
func TestToolPairing(t *testing.T) {
	setHookEnv(t)
	f := serveCore(t, fakecore.Script{Default: allowJSON})
	runHook(t, "PreToolUse", fixture(t, "pre-tool-use-bash", "s-tool"))
	runHook(t, "PostToolUse", fixture(t, "post-tool-use", "s-tool"))
	endSession(t, fixture(t, "session-end", "s-tool"), "s-tool")

	byActivity := map[string][]string{}
	for _, r := range f.Inbox() {
		if r.ToolUseID() == "call_0001" {
			byActivity[r.ActivityID()] = append(byActivity[r.ActivityID()], r.EventType())
		}
	}
	if len(byActivity) != 1 {
		t.Fatalf("tool rows spread over %d activities: %v", len(byActivity), byActivity)
	}
	for id, types := range byActivity {
		if len(types) != 2 || types[0] != fakecore.WireActivityStarted || types[1] != fakecore.WireActivityCompleted {
			t.Errorf("activity %s = %v, want one started and one completed", id, types)
		}
	}
}

// The trace holds usage and the trace context; the wire never does.
func TestUsageAndTraceparentStayLocal(t *testing.T) {
	setHookEnv(t)
	f := serveCore(t, fakecore.Script{Default: allowJSON})
	t.Setenv(devconfig.EnvContentCapture, "1")
	runHook(t, "PreLLMCall", fixture(t, "pre-llm-call", "s-usage"))
	runHook(t, "PostLLMCall", fixture(t, "post-llm-call", "s-usage"))
	endSession(t, fixture(t, "session-end", "s-usage"), "s-usage")

	for _, r := range f.Inbox() {
		raw := string(r.Raw)
		for _, leaked := range []string{"traceparent", "00-00000000000000000000000000000001", "cache_read_tokens", "reasoning_tokens", "input_tokens", "1200"} {
			if strings.Contains(raw, leaked) {
				t.Fatalf("local-only fact %q reached the wire: %s", leaked, raw)
			}
		}
	}
	var local strings.Builder
	files, _ := filepath.Glob(filepath.Join(trace.Dir(), "*"))
	for _, p := range files {
		raw, _ := os.ReadFile(p)
		local.Write(raw)
	}
	for _, want := range []string{outcomeTraceparent, outcomeUsage, "00-00000000000000000000000000000001-0000000000000001-01", `"input_tokens":1200`, `"cache_read_tokens":800`, `"cached_tokens":800`} {
		if !strings.Contains(local.String(), want) {
			t.Errorf("the local trace lacks %q", want)
		}
	}
}

// The completed half of a model-call gate carries the time since its started
// half, recovered across the two hook processes; a completed half with no
// recorded start carries none.
func TestModelCallFinishedCarriesTheGateDuration(t *testing.T) {
	setHookEnv(t)
	f := serveCore(t, fakecore.Script{Default: allowJSON})
	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	defer func(prev func() time.Time) { nowFn = prev }(nowFn)

	nowFn = func() time.Time { return base }
	runHook(t, "PreLLMCall", fixture(t, "pre-llm-call", "s-dur"))
	nowFn = func() time.Time { return base.Add(250 * time.Millisecond) }
	runHook(t, "PostLLMCall", fixture(t, "post-llm-call", "s-dur"))
	// A finished half whose gate has no recorded start.
	runHook(t, "PostLLMCall", fixture(t, "post-llm-call", "s-dur"))
	endSession(t, fixture(t, "session-end", "s-dur"), "s-dur")

	var completed []fakecore.Received
	for _, r := range gateRows(f) {
		if r.EventType() == fakecore.WireActivityCompleted {
			completed = append(completed, r)
		}
	}
	if len(completed) == 0 {
		t.Fatal("no completed gate row")
	}
	if got, ok := completed[0].Body["duration_ms"].(float64); !ok || got != 250 {
		t.Errorf("completed gate row duration_ms = %v, want 250", completed[0].Body["duration_ms"])
	}
	if len(completed) > 1 {
		if d, has := completed[1].Body["duration_ms"]; has {
			t.Errorf("a completed half with no recorded start carries duration_ms %v", d)
		}
	}
}
