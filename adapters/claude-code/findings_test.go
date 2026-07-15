package claudecode

import (
	"bytes"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/client"
)

// mkReasons builds guardrail reasons carrying only a category TYPE (the content-free
// shape). mkReasonsFull builds one reason with type/field/reason free text (to prove
// the free text never reaches the surface — INV-2).
func mkReasons(cats ...string) []client.GuardrailReason {
	out := make([]client.GuardrailReason, 0, len(cats))
	for _, c := range cats {
		out = append(out, client.GuardrailReason{Type: c})
	}
	return out
}

func mkReasonsFull(typ, field, reason string) []client.GuardrailReason {
	return []client.GuardrailReason{{Type: typ, Field: field, Reason: reason}}
}

// findingsEnv isolates the advisory sink + cursor at temp paths and sets the
// findings flag. Returns (advPath, cursorPath).
func findingsEnv(t *testing.T, on bool) (string, string) {
	t.Helper()
	dir := t.TempDir()
	adv := filepath.Join(dir, "advisories.jsonl")
	cur := filepath.Join(dir, "findings.cursor")
	t.Setenv("OPENBOX_ADVISORY_FILE", adv)
	t.Setenv(envFindingsCursor, cur)
	if on {
		t.Setenv(envFindings, "1")
	} else {
		t.Setenv(envFindings, "0")
	}
	return adv, cur
}

// seedAdvisories appends raw JSONL advisory records (as core+Advisory.Record would
// have written them) so the consumer parses realistic input.
func seedAdvisories(t *testing.T, path string, recs ...advisoryRecord) {
	t.Helper()
	var buf bytes.Buffer
	for _, r := range recs {
		line, err := json.Marshal(r)
		if err != nil {
			t.Fatalf("marshal advisory: %v", err)
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write(buf.Bytes()); err != nil {
		t.Fatal(err)
	}
}

func nopLogger() *log.Logger { return log.New(&bytes.Buffer{}, "", 0) }

// ── summarizeFindings: content-free rendering ────────────────────────────────

func TestSummarizeFindings_ContentFreeAggregate(t *testing.T) {
	recs := []advisoryRecord{
		{EventType: "ToolCall", Verdict: "BLOCK", WouldBlock: true, RiskScore: 0.9,
			GuardrailReasons: mkReasonsFull("pii", "email", "Contains PII john@example.com")},
		{EventType: "ToolResult", Verdict: "ALLOW", DriftDetected: true, DriftViolations: 2, RiskScore: 0.3},
	}
	// Second guardrail category so the summary lists a sorted set.
	recs[0].GuardrailReasons = append(recs[0].GuardrailReasons, mkReasons("secret")...)
	var buf bytes.Buffer
	for _, r := range recs {
		line, _ := json.Marshal(r)
		buf.Write(line)
		buf.WriteByte('\n')
	}
	sum := summarizeFindings(buf.Bytes())

	if sum == "" {
		t.Fatal("expected a summary for 2 notable records")
	}
	// Content-free: the message reports counts/categories, never free text or content.
	for _, banned := range []string{"john@example.com", "Contains PII", "email"} {
		if strings.Contains(sum, banned) {
			t.Errorf("summary leaked content %q: %s", banned, sum)
		}
	}
	for _, want := range []string{"2 finding(s)", "1 would-block", "[BLOCK]", "guardrails [pii,secret]", "goal-drift on 1 (2 violation(s))", "max risk 0.90"} {
		if !strings.Contains(sum, want) {
			t.Errorf("summary missing %q: %s", want, sum)
		}
	}
	// ALLOW must not appear as a verdict label.
	if strings.Contains(sum, "ALLOW") {
		t.Errorf("ALLOW should not be listed as a verdict: %s", sum)
	}
}

// TestSummarizeFindings_CategoryBoundedAndSanitized covers G_SEC LOW-1/INFO-1: a
// remote-sourced guardrail category with control chars / excess length is sanitized,
// and a large category set is capped before it is injected into the model context.
func TestSummarizeFindings_CategoryBoundedAndSanitized(t *testing.T) {
	// Control chars stripped, length capped.
	long := strings.Repeat("x", 100)
	rec := advisoryRecord{Verdict: "ALLOW", GuardrailReasons: mkReasons("pi\ni\r\t", long)}
	line, _ := json.Marshal(rec)
	sum := summarizeFindings(append(line, '\n'))
	if strings.ContainsAny(sum, "\n\r\t") {
		t.Errorf("summary must strip control chars from categories: %q", sum)
	}
	if strings.Contains(sum, long) {
		t.Errorf("summary must cap category length: %q", sum)
	}

	// Cardinality cap: > maxCategoriesShown distinct categories → "+N more".
	cats := make([]string, maxCategoriesShown+5)
	for i := range cats {
		cats[i] = "cat" + strconv.Itoa(i)
	}
	rec2 := advisoryRecord{Verdict: "ALLOW", GuardrailReasons: mkReasons(cats...)}
	line2, _ := json.Marshal(rec2)
	sum2 := summarizeFindings(append(line2, '\n'))
	if !strings.Contains(sum2, "+5 more") {
		t.Errorf("summary must cap category cardinality with '+N more': %q", sum2)
	}
}

// TestSummarizeFindings_ConstraintCount covers G3 obs-3: the content-free constraint
// count is surfaced.
func TestSummarizeFindings_ConstraintCount(t *testing.T) {
	rec := advisoryRecord{Verdict: "CONSTRAIN", Constraints: []map[string]any{{"type": "rate_limit"}, {"type": "scope"}}}
	line, _ := json.Marshal(rec)
	sum := summarizeFindings(append(line, '\n'))
	if !strings.Contains(sum, "2 constraint(s)") {
		t.Errorf("summary should surface the constraint count: %q", sum)
	}
}

func TestSummarizeFindings_EmptyAndCorrupt(t *testing.T) {
	if summarizeFindings(nil) != "" {
		t.Error("nil delta should summarize to empty")
	}
	if summarizeFindings([]byte("not json\n{bad}\n")) != "" {
		t.Error("all-corrupt delta should summarize to empty")
	}
	// A corrupt line among valid ones is skipped, not fatal.
	valid, _ := json.Marshal(advisoryRecord{Verdict: "CONSTRAIN"})
	mixed := append([]byte("garbage\n"), append(valid, '\n')...)
	if got := summarizeFindings(mixed); !strings.Contains(got, "1 finding(s)") {
		t.Errorf("mixed delta should count the 1 valid record: %q", got)
	}
}

// ── cursor mechanics ─────────────────────────────────────────────────────────

func TestCursorRoundTrip(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "findings.cursor")
	if readCursor(p) != 0 {
		t.Error("absent cursor must read 0")
	}
	advanceCursor(p, 4242, nopLogger())
	if got := readCursor(p); got != 4242 {
		t.Errorf("cursor round-trip = %d, want 4242", got)
	}
	// A garbage/negative cursor reads back as 0 (fail-safe).
	os.WriteFile(p, []byte("-9"), 0o600)
	if readCursor(p) != 0 {
		t.Error("negative cursor must read 0")
	}
	os.WriteFile(p, []byte("xyz"), 0o600)
	if readCursor(p) != 0 {
		t.Error("garbage cursor must read 0")
	}
}

// ── surfaceFindings: end-to-end via the real function ────────────────────────

func TestSurfaceFindings_SurfaceAdvanceAtMostOnce(t *testing.T) {
	adv, cur := findingsEnv(t, true)
	seedAdvisories(t, adv,
		advisoryRecord{Verdict: "BLOCK", WouldBlock: true, RiskScore: 0.8})

	var out bytes.Buffer
	surfaceFindings(HookPostToolUse, &out, nopLogger())
	first := out.String()
	if !strings.Contains(first, "OpenBox governance") {
		t.Fatalf("first surface should emit a summary, got %q", first)
	}
	// It is ONE valid JSON object with additionalContext + systemMessage, no blocking field.
	assertFindingsShape(t, first, HookPostToolUse)

	// Cursor advanced to EOF.
	fi, _ := os.Stat(adv)
	if readCursor(cur) != fi.Size() {
		t.Errorf("cursor = %d, want advisory size %d", readCursor(cur), fi.Size())
	}

	// Second call, no new records → nothing.
	out.Reset()
	surfaceFindings(HookPostToolUse, &out, nopLogger())
	if out.Len() != 0 {
		t.Errorf("at-most-once: second surface with no new records must be empty, got %q", out.String())
	}

	// A NEW record appears → surfaced on the next call.
	seedAdvisories(t, adv, advisoryRecord{Verdict: "CONSTRAIN"})
	out.Reset()
	surfaceFindings(HookUserPromptSubmit, &out, nopLogger())
	if !strings.Contains(out.String(), "1 finding(s)") {
		t.Errorf("a new record should surface, got %q", out.String())
	}
}

func TestSurfaceFindings_NoSinkAndNilStdout(t *testing.T) {
	findingsEnv(t, true) // adv path set but no file created
	var out bytes.Buffer
	surfaceFindings(HookPostToolUse, &out, nopLogger())
	if out.Len() != 0 {
		t.Errorf("no advisory sink → empty, got %q", out.String())
	}
	// nil stdout must be a safe no-op (INV-3).
	surfaceFindings(HookPostToolUse, nil, nopLogger())
}

func TestSurfaceFindings_StatGuardNoBodyRead(t *testing.T) {
	adv, cur := findingsEnv(t, true)
	seedAdvisories(t, adv, advisoryRecord{Verdict: "BLOCK", WouldBlock: true})
	// Advance the cursor to EOF so there is nothing new.
	fi, _ := os.Stat(adv)
	advanceCursor(cur, fi.Size(), nopLogger())

	var out bytes.Buffer
	surfaceFindings(HookPostToolUse, &out, nopLogger())
	if out.Len() != 0 {
		t.Errorf("stat guard: size==offset must emit nothing, got %q", out.String())
	}
}

func TestSurfaceFindings_ShrunkFileResets(t *testing.T) {
	adv, cur := findingsEnv(t, true)
	seedAdvisories(t, adv, advisoryRecord{Verdict: "BLOCK", WouldBlock: true})
	// Cursor points PAST the current EOF (file was truncated/rotated).
	advanceCursor(cur, 1_000_000, nopLogger())

	var out bytes.Buffer
	surfaceFindings(HookPostToolUse, &out, nopLogger())
	if !strings.Contains(out.String(), "OpenBox governance") {
		t.Errorf("a shrunk file must reset the offset and re-surface, got %q", out.String())
	}
}

// ── RunHook wiring: flag-gated, correct hooks only ───────────────────────────

func TestRunHook_FindingsSurfacedOnPostToolUseAndPrompt(t *testing.T) {
	adv, _ := findingsEnv(t, true)
	isolateConfig(t)
	t.Setenv(envDID, testDID)
	t.Setenv("OPENBOX_SPOOL_DIR", t.TempDir())
	t.Setenv("OPENBOX_SESSION_DIR", t.TempDir())
	// A guardrail reason free-text carrying a fake secret + a drift record.
	seedAdvisories(t, adv,
		advisoryRecord{Verdict: "ALLOW", GuardrailReasons: mkReasonsFull("secret", "api_key", "leaked AKIAsecretVALUE123")},
		advisoryRecord{Verdict: "ALLOW", DriftDetected: true, DriftViolations: 1},
	)

	for _, hook := range []string{"PostToolUse", "UserPromptSubmit"} {
		// Reset the cursor per iteration so each hook sees the seeded records.
		t.Setenv(envFindingsCursor, filepath.Join(t.TempDir(), "cursor"))
		var out bytes.Buffer
		payload := `{"hook_event_name":"` + hook + `","session_id":"sess-1","cwd":"/tmp","tool_name":"Bash","tool_input":{"command":"echo hi"},"prompt":"hello"}`
		RunHook(hook, strings.NewReader(payload), &out, nopLogger())
		s := out.String()
		if !strings.Contains(s, "OpenBox governance") {
			t.Errorf("%s with findings ON should surface a summary, got %q", hook, s)
		}
		// INV-2: the seeded secret / free text must never reach the surface.
		for _, banned := range []string{"AKIAsecretVALUE123", "leaked", "api_key"} {
			if strings.Contains(s, banned) {
				t.Errorf("%s leaked content %q: %s", hook, banned, s)
			}
		}
		// Guardrail CATEGORY is fine; drift surfaces.
		if !strings.Contains(s, "guardrails [secret]") || !strings.Contains(s, "goal-drift") {
			t.Errorf("%s summary missing category/drift: %s", hook, s)
		}
		assertFindingsShape(t, s, HookName(hook))
	}
}

func TestRunHook_FindingsOffIsByteIdentical(t *testing.T) {
	adv, _ := findingsEnv(t, false) // findings OFF
	isolateConfig(t)
	t.Setenv(envDID, testDID)
	t.Setenv("OPENBOX_SPOOL_DIR", t.TempDir())
	t.Setenv("OPENBOX_SESSION_DIR", t.TempDir())
	seedAdvisories(t, adv, advisoryRecord{Verdict: "BLOCK", WouldBlock: true, RiskScore: 0.9})

	for _, hook := range []string{"PostToolUse", "UserPromptSubmit"} {
		var out bytes.Buffer
		payload := `{"hook_event_name":"` + hook + `","session_id":"sess-1","cwd":"/tmp","tool_name":"Bash","tool_input":{"command":"echo hi"},"prompt":"hi"}`
		RunHook(hook, strings.NewReader(payload), &out, nopLogger())
		if out.Len() != 0 {
			t.Errorf("findings OFF: %s must write nothing, got %q", hook, out.String())
		}
	}
}

// TestRunHook_FindingsNotSurfacedOnOtherHooks: only PostToolUse + UserPromptSubmit
// surface; PreToolUse/SessionStart/SessionEnd never do (they own other stdout).
func TestRunHook_FindingsNotSurfacedOnOtherHooks(t *testing.T) {
	adv, _ := findingsEnv(t, true)
	isolateConfig(t)
	t.Setenv(envDID, testDID)
	t.Setenv("OPENBOX_SPOOL_DIR", t.TempDir())
	t.Setenv("OPENBOX_SESSION_DIR", t.TempDir())
	seedAdvisories(t, adv, advisoryRecord{Verdict: "BLOCK", WouldBlock: true})

	// PreToolUse (enforce OFF) must not surface findings.
	var out bytes.Buffer
	payload := `{"hook_event_name":"PreToolUse","session_id":"s","cwd":"/tmp","tool_name":"Read","tool_input":{"file_path":"/x"}}`
	RunHook("PreToolUse", strings.NewReader(payload), &out, nopLogger())
	if strings.Contains(out.String(), "OpenBox governance") {
		t.Errorf("PreToolUse must not surface findings, got %q", out.String())
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────

// assertFindingsShape validates the emitted JSON is exactly the non-blocking
// findings shape: additionalContext + systemMessage, correct hookEventName, and NO
// decision/permissionDecision (INV-3).
func assertFindingsShape(t *testing.T, blob string, hook HookName) {
	t.Helper()
	blob = strings.TrimSpace(blob)
	var m map[string]any
	if err := json.Unmarshal([]byte(blob), &m); err != nil {
		t.Fatalf("findings output is not one JSON object: %v (%q)", err, blob)
	}
	if _, ok := m["decision"]; ok {
		t.Error("INV-3: findings output must not carry a `decision`")
	}
	hso, ok := m["hookSpecificOutput"].(map[string]any)
	if !ok {
		t.Fatal("missing hookSpecificOutput")
	}
	if _, ok := hso["permissionDecision"]; ok {
		t.Error("INV-3: findings output must not carry a `permissionDecision`")
	}
	if hso["hookEventName"] != string(hook) {
		t.Errorf("hookEventName = %v, want %s", hso["hookEventName"], hook)
	}
	if _, ok := hso["additionalContext"].(string); !ok {
		t.Error("missing additionalContext")
	}
	if _, ok := m["systemMessage"].(string); !ok {
		t.Error("missing systemMessage")
	}
}
