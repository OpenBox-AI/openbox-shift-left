package codex

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestFixtures_AllFiveEventsObserveOnly drives every testdata fixture (the
// v0.145.0-shaped payloads the story's manual validation pipes into the real
// binary) through the engine and asserts the observe contract for each: no
// stdout, and the tool/prompt fixtures spool without leaking their payload
// content. Keeps the fixtures parse-honest — a field rename breaks here.
func TestFixtures_AllFiveEventsObserveOnly(t *testing.T) {
	spool := setHookEnv(t)
	fixtures := []struct{ sub, file string }{
		{"SessionStart", "sessionstart.json"},
		{"UserPromptSubmit", "userpromptsubmit.json"},
		{"PreToolUse", "pretooluse.json"},
		{"PostToolUse", "posttooluse.json"},
		{"SessionEnd", "sessionend.json"},
	}
	for _, f := range fixtures {
		raw, err := os.ReadFile(filepath.Join("testdata", f.file))
		if err != nil {
			t.Fatalf("fixture %s: %v", f.file, err)
		}
		stdout, stderr := runHook(t, f.sub, string(raw))
		if stdout != "" {
			t.Fatalf("%s: stdout must be empty, got %q", f.sub, stdout)
		}
		if strings.Contains(stderr, "dropping") {
			t.Fatalf("%s: fixture failed to parse/map: %s", f.sub, stderr)
		}
	}

	// All five events mapped: the four pre-SessionEnd ones + SessionEnded went
	// through the spool (SessionEnd's flush is fail-open offline here, so the
	// spool file remains with all 5 lines).
	entries, _ := os.ReadDir(spool)
	var lines int
	var spooled string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		raw, _ := os.ReadFile(filepath.Join(spool, e.Name()))
		lines += strings.Count(string(raw), "\n")
		spooled += string(raw)
		// UNCONDITIONAL (SL3-SEC-3): the tool command and the tool output must
		// never be spooled, in any posture.
		for _, secret := range []string{"go test ./...", "0.412s"} {
			if strings.Contains(string(raw), secret) {
				t.Fatalf("tool content leaked into the spool: %s", raw)
			}
		}
	}
	if lines != 5 {
		t.Errorf("spooled %d events, want 5 (SessionStarted, PromptSubmitted, ToolCall, ToolResult, SessionEnded)", lines)
	}
	// This run uses the DEFAULT posture (content-capture ON as of 2026-07-15,
	// no dev.json / no env override), so the prompt — the one gated field — IS
	// captured. Pin that so a silent posture regression fails here; the opt-out
	// path is covered by TestMap_PromptCaptureGatedOnContentCapture.
	if !strings.Contains(spooled, "add a health endpoint") {
		t.Errorf("default content-ON posture should capture the prompt; spool: %s", spooled)
	}
}
