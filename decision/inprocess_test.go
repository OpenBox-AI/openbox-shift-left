package decision

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/client"
)

// writeBundle marshals b to a temp file and returns its path, for exercising the
// file-load path NewInProcessDecider takes.
func writeBundle(t *testing.T, b *Bundle) string {
	t.Helper()
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("marshal bundle: %v", err)
	}
	path := filepath.Join(t.TempDir(), "policy-bundle.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write bundle: %v", err)
	}
	return path
}

// TestInProcess_LoadedBundle_BlockAndAllow proves the in-process decider reaches a
// REAL verdict from the local bundle file — the same outcome the socket round-trip
// produces (TestRoundTrip_BlockAndAllow) — with no daemon and no socket.
func TestInProcess_LoadedBundle_BlockAndAllow(t *testing.T) {
	path := writeBundle(t, &Bundle{Version: "v1", DefaultDecision: "allow", Rules: []Rule{
		{ID: "rmrf", Match: RuleMatch{ToolName: "Bash", AttributeContains: map[string]string{"command": "rm -rf /"}},
			Decision: "block", Reason: "destructive"},
	}})
	d := NewInProcessDecider(InProcessConfig{BundlePath: path})

	block := d.Decide(context.Background(), toolCall("Bash", client.ToolShell, map[string]any{"command": "rm -rf / now"}))
	if block.FailOpen {
		t.Fatalf("expected a real decision, got fail-open (%s)", block.Source)
	}
	if block.Evaluation.Verdict != client.VerdictBlock || !block.Evaluation.WouldBlock() {
		t.Fatalf("verdict = %q (would_block=%t), want BLOCK", block.Evaluation.Verdict, block.Evaluation.WouldBlock())
	}
	if block.Source != sourceLocalBundle {
		t.Errorf("source = %q, want %q", block.Source, sourceLocalBundle)
	}

	allow := d.Decide(context.Background(), toolCall("Bash", client.ToolShell, map[string]any{"command": "echo hi"}))
	if allow.Evaluation.WouldBlock() {
		t.Errorf("benign command should not block: %+v", allow)
	}
	if allow.FailOpen {
		t.Errorf("a real allow from a loaded bundle must not be fail-open (%s)", allow.Source)
	}
}

// TestInProcess_NoBundle_FailsOpen proves an absent bundle file degrades to
// cold-start fail-open (VerdictUnknown / no-bundle source) rather than erroring —
// so the E6-S3 failure policy, not a hard error, governs the outcome.
func TestInProcess_NoBundle_FailsOpen(t *testing.T) {
	d := NewInProcessDecider(InProcessConfig{BundlePath: filepath.Join(t.TempDir(), "absent.json")})

	dec := d.Decide(context.Background(), toolCall("Bash", client.ToolShell, map[string]any{"command": "rm -rf /"}))
	if !dec.FailOpen {
		t.Fatalf("absent bundle must fail open, got %+v", dec)
	}
	if dec.Evaluation.Verdict != client.VerdictUnknown {
		t.Errorf("verdict = %q, want Unknown (not evaluated)", dec.Evaluation.Verdict)
	}
	if dec.Evaluation.WouldBlock() {
		t.Error("a fail-open decision must never report WouldBlock")
	}
	if dec.Source != sourceFailOpenNoBundle {
		t.Errorf("source = %q, want %q", dec.Source, sourceFailOpenNoBundle)
	}
}

// TestInProcess_SecretRedaction proves the Tier-1 secret detector (E6-S9) runs on
// the in-process path exactly as it does in the daemon — content stays in-process,
// never touching a socket (strictly stronger than the socket transport, INV-2).
func TestInProcess_SecretRedaction(t *testing.T) {
	path := writeBundle(t, &Bundle{Version: "v1", DefaultDecision: "allow"})
	d := NewInProcessDecider(InProcessConfig{BundlePath: path})

	req := toolCall("Write", client.ToolFile, map[string]any{"file_path": "config.env"})
	req.Content = &client.Content{FileText: "AWS_SECRET_ACCESS_KEY=abcd1234EXAMPLEabcd1234EXAMPLEabcd1234EX"}

	dec := d.Decide(context.Background(), req)
	if dec.RedactedContent == nil {
		t.Fatal("expected the secret detector to attach RedactedContent")
	}
	if dec.RedactedContent.FileText == req.Content.FileText {
		t.Error("redacted body should differ from the original secret-bearing body")
	}
	if len(dec.RedactionCategories) == 0 {
		t.Error("expected at least one redaction category")
	}
}
