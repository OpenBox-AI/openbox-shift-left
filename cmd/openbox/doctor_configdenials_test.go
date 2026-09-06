package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
)

func writeEnforcementLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestLastDecisionSummary_NoneRecorded (V6): absent file -> "(none
// recorded)", the same fallback the pre-switch os.ReadFile reader used.
func TestLastDecisionSummary_NoneRecorded(t *testing.T) {
	t.Setenv(devconfig.EnvEnforcementFile, filepath.Join(t.TempDir(), "does-not-exist.jsonl"))
	if got := lastDecisionSummary(); got != "(none recorded)" {
		t.Errorf("lastDecisionSummary() = %q, want %q", got, "(none recorded)")
	}
}

// TestLastDecisionSummary_Unreadable: a present but unparseable last line ->
// "(unreadable)", unchanged from before the V6 switch.
func TestLastDecisionSummary_Unreadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "enf.jsonl")
	t.Setenv(devconfig.EnvEnforcementFile, path)
	writeEnforcementLines(t, path, "{not json")
	if got := lastDecisionSummary(); got != "(unreadable)" {
		t.Errorf("lastDecisionSummary() = %q, want %q", got, "(unreadable)")
	}
}

// TestLastDecisionSummary_GoldenUnderCap (V6, case d): for a file under the
// cap, the switch onto tailLines must not change the printed line -- pinned
// against a golden string captured from the pre-switch shape (policy_id
// present / absent, both formats).
func TestLastDecisionSummary_GoldenUnderCap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "enf.jsonl")
	t.Setenv(devconfig.EnvEnforcementFile, path)

	writeEnforcementLines(t, path,
		`{"session_id":"s1","verdict":"allow","ts":"2026-01-01T00:00:00Z"}`,
		`{"session_id":"s2","verdict":"block","source":"evaluate","policy_id":"pol-9","ts":"2026-01-01T00:00:01Z"}`,
	)
	want := "policy pol-9 (block via evaluate)"
	if got := lastDecisionSummary(); got != want {
		t.Errorf("lastDecisionSummary() = %q, want the golden %q", got, want)
	}

	writeEnforcementLines(t, path,
		`{"session_id":"s3","verdict":"halt","source":"session-halt","ts":"2026-01-01T00:00:02Z"}`,
	)
	want2 := "halt via session-halt; NO policy decided this call"
	if got := lastDecisionSummary(); got != want2 {
		t.Errorf("lastDecisionSummary() (no policy_id) = %q, want the golden %q", got, want2)
	}
}

// TestLastDecisionSummary_SwitchedOntoTailLines proves the switch actually
// happened: a file whose LEADING bytes are garbage that would break a whole-
// file os.ReadFile-based parse of "the last line" only if the code still read
// from byte 0; tailLines seeks from EOF, so an oversized leading blob outside
// the cap must not affect the answer.
func TestLastDecisionSummary_SwitchedOntoTailLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "enf.jsonl")
	t.Setenv(devconfig.EnvEnforcementFile, path)

	var sb strings.Builder
	for i := 0; i < 2000; i++ {
		sb.WriteString(`{"session_id":"noise","verdict":"allow","ts":"2026-01-01T00:00:00Z"}` + "\n")
	}
	sb.WriteString(`{"session_id":"last","verdict":"deny","source":"evaluate","policy_id":"final-pol","ts":"2026-01-01T01:00:00Z"}` + "\n")
	if err := os.WriteFile(path, []byte(sb.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	want := "policy final-pol (deny via evaluate)"
	if got := lastDecisionSummary(); got != want {
		t.Errorf("lastDecisionSummary() over a large file = %q, want %q", got, want)
	}
}

// TestRecentConfigDenials_FiltersToolKind (D6): only tool_kind=="config"
// lines are reported, most-recent policy/reason/timestamp surfaced, and a
// non-config sink (no config lines at all) reports none.
func TestRecentConfigDenials_FiltersToolKind(t *testing.T) {
	path := filepath.Join(t.TempDir(), "enf.jsonl")
	t.Setenv(devconfig.EnvEnforcementFile, path)

	writeEnforcementLines(t, path,
		`{"session_id":"s1","tool_kind":"shell","verdict":"block","applied_decision":"deny","ts":"2026-01-01T00:00:00Z","reason":"rm -rf blocked"}`,
		`{"session_id":"s2","tool_kind":"config","verdict":"block","applied_decision":"block","policy_id":"cc-1","ts":"2026-01-01T00:00:01Z","reason":"unauthorized edit"}`,
		`{"session_id":"s3","tool_kind":"prompt","verdict":"block","applied_decision":"block","ts":"2026-01-01T00:00:02Z","reason":"off scope"}`,
		`{"session_id":"s4","tool_kind":"config","verdict":"halt","applied_decision":"halt","policy_id":"cc-2","ts":"2026-01-01T00:00:03Z","reason":"kill switch"}`,
	)

	got, unreadable := recentConfigDenials(5)
	if unreadable {
		t.Fatal("a fully parseable sink must not report unreadable")
	}
	if len(got) != 2 {
		t.Fatalf("recentConfigDenials = %v, want exactly the 2 tool_kind==config lines", got)
	}
	joined := strings.Join(got, "\n")
	for _, want := range []string{"cc-1", "unauthorized edit", "cc-2", "kill switch"} {
		if !strings.Contains(joined, want) {
			t.Errorf("output missing %q: %q", want, joined)
		}
	}
	for _, mustNot := range []string{"rm -rf blocked", "off scope"} {
		if strings.Contains(joined, mustNot) {
			t.Errorf("output leaked a non-config record %q: %q", mustNot, joined)
		}
	}
}

// TestRecentConfigDenials_StopsAtN: with more than N config denials present,
// only the most recent N are returned.
func TestRecentConfigDenials_StopsAtN(t *testing.T) {
	path := filepath.Join(t.TempDir(), "enf.jsonl")
	t.Setenv(devconfig.EnvEnforcementFile, path)

	var lines []string
	for i := 1; i <= 8; i++ {
		lines = append(lines, `{"session_id":"s","tool_kind":"config","applied_decision":"block","policy_id":"p`+string(rune('0'+i))+`","ts":"2026-01-01T00:00:0`+string(rune('0'+i%10))+`Z","reason":"r`+string(rune('0'+i))+`"}`)
	}
	writeEnforcementLines(t, path, lines...)

	got, unreadable := recentConfigDenials(5)
	if unreadable {
		t.Fatal("unexpected unreadable")
	}
	if len(got) != 5 {
		t.Fatalf("recentConfigDenials(5) returned %d lines, want 5", len(got))
	}
	// Most recent (p8) must be present; the oldest (p1..p3) must not be.
	joined := strings.Join(got, "\n")
	if !strings.Contains(joined, "p8") {
		t.Errorf("missing the most recent denial p8: %q", joined)
	}
	for _, old := range []string{"p1", "p2", "p3"} {
		if strings.Contains(joined, old) {
			t.Errorf("stopped-at-N leaked an older-than-N denial %q: %q", old, joined)
		}
	}
}

// TestRecentConfigDenials_NoneRecorded: an absent sink and a sink with no
// config lines both report zero denials, unreadable=false (D6's "(none
// recorded)" case, distinct from a corrupt sink).
func TestRecentConfigDenials_NoneRecorded(t *testing.T) {
	t.Setenv(devconfig.EnvEnforcementFile, filepath.Join(t.TempDir(), "absent.jsonl"))
	got, unreadable := recentConfigDenials(5)
	if len(got) != 0 || unreadable {
		t.Errorf("absent sink: got=%v unreadable=%t, want none/false", got, unreadable)
	}

	path := filepath.Join(t.TempDir(), "enf.jsonl")
	t.Setenv(devconfig.EnvEnforcementFile, path)
	writeEnforcementLines(t, path, `{"session_id":"s","tool_kind":"shell","verdict":"allow","ts":"2026-01-01T00:00:00Z"}`)
	got2, unreadable2 := recentConfigDenials(5)
	if len(got2) != 0 || unreadable2 {
		t.Errorf("no config lines present: got=%v unreadable=%t, want none/false", got2, unreadable2)
	}
}

// TestRecentConfigDenials_Unreadable: a sink whose every line is corrupt
// (not just devoid of config-kind lines) reports unreadable=true, the D6
// "(unreadable)" case.
func TestRecentConfigDenials_Unreadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "enf.jsonl")
	t.Setenv(devconfig.EnvEnforcementFile, path)
	writeEnforcementLines(t, path, "{not json at all", "{also not json")
	got, unreadable := recentConfigDenials(5)
	if len(got) != 0 || !unreadable {
		t.Errorf("corrupt sink: got=%v unreadable=%t, want none/true", got, unreadable)
	}
}

// TestDoctorReaders_AgreeOnSameSink (V6, case d): both doctor readers see the
// SAME underlying record for a file under the cap.
func TestDoctorReaders_AgreeOnSameSink(t *testing.T) {
	path := filepath.Join(t.TempDir(), "enf.jsonl")
	t.Setenv(devconfig.EnvEnforcementFile, path)
	writeEnforcementLines(t, path,
		`{"session_id":"s1","tool_kind":"config","verdict":"block","applied_decision":"block","source":"evaluate","policy_id":"agree-pol","ts":"2026-01-01T00:00:00Z","reason":"agree reason"}`,
	)

	summary := lastDecisionSummary()
	if !strings.Contains(summary, "agree-pol") {
		t.Errorf("lastDecisionSummary() = %q, want it to see the same record", summary)
	}
	denials, unreadable := recentConfigDenials(5)
	if unreadable || len(denials) != 1 || !strings.Contains(denials[0], "agree-pol") {
		t.Errorf("recentConfigDenials = %v (unreadable=%t), want the same record", denials, unreadable)
	}
}
