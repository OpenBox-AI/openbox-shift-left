package codex

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
)

// TestDelivery_UndeliveredEventIsRetriedNotDropped e8-S7.
func TestDelivery_UndeliveredEventIsRetriedNotDropped(t *testing.T) {
	dir := t.TempDir()
	s := hookflow.Spool{Dir: dir}
	if err := s.Append(client.DevEvent{EventID: "evt-1", SessionID: "s1"}); err != nil {
		t.Fatalf("append: %v", err)
	}

	n, err := s.FlushAll(context.Background(), func(context.Context, client.DevEvent) error {
		return errors.New("network down")
	})
	if err != nil {
		t.Fatalf("a delivery failure must not fail the drain (INV-3): %v", err)
	}
	if n != 0 {
		t.Errorf("undelivered event counted as delivered: n=%d", n)
	}

	var got []string
	n, err = s.FlushAll(context.Background(), func(_ context.Context, ev client.DevEvent) error {
		got = append(got, ev.EventID)
		return nil
	})
	if err != nil {
		t.Fatalf("recovery flush: %v", err)
	}
	if n != 1 || len(got) != 1 || got[0] != "evt-1" {
		t.Fatalf("event was not retried: n=%d got=%v", n, got)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "*.jsonl")); len(left) != 0 {
		t.Errorf("spool should be empty after a successful drain, got %v", left)
	}
}

// TestDelivery_RetryIsBounded the retry must be bounded. An event the server
// will never accept would otherwise be re-sent on every flush forever, costing
// a request each time on a developer's machine.
func TestDelivery_RetryIsBounded(t *testing.T) {
	dir := t.TempDir()
	s := hookflow.Spool{Dir: dir}
	if err := s.Append(client.DevEvent{EventID: "evt-poison", SessionID: "s1"}); err != nil {
		t.Fatalf("append: %v", err)
	}

	alwaysFails := func(context.Context, client.DevEvent) error { return errors.New("rejected") }
	attempts := 0
	for i := 0; i < hookflow.MaxRecoveryAttempts+3; i++ {
		before, _ := filepath.Glob(filepath.Join(dir, "*.jsonl"))
		if len(before) == 0 {
			break // given up, as intended
		}
		if _, err := s.FlushAll(context.Background(), alwaysFails); err != nil {
			t.Fatalf("flush %d: %v", i, err)
		}
		attempts++
	}

	if left, _ := filepath.Glob(filepath.Join(dir, "*.jsonl")); len(left) != 0 {
		t.Errorf("a permanently undeliverable event must eventually be dropped, still have %v", left)
	}
	if attempts > hookflow.MaxRecoveryAttempts+1 {
		t.Errorf("retried %d times, want at most %d", attempts, hookflow.MaxRecoveryAttempts+1)
	}
	if attempts < 2 {
		t.Errorf("gave up after %d attempt(s); a transient failure deserves a retry", attempts)
	}
}

// TestDelivery_RecoveryAttemptParsing the attempt counter is read back from
// the filename, so the bound survives the process exiting between flushes (the
// normal case: one flush per session end).
func TestDelivery_RecoveryAttemptParsing(t *testing.T) {
	cases := map[string]int{
		"s1.jsonl":              0, // a fresh spool file
		"s1.rec-abc.jsonl":      0, // legacy name, written before the counter
		"s1.rec1-abc.jsonl":     1,
		"s1.rec4-abc.jsonl":     4,
		"s1.jsonl.flushing.abc": 0, // an orphan, never carried over
		"s1.recX-abc.jsonl":     0, // unparsable ⇒ full allowance, not a drop
		"s1.rec-1-abc.jsonl":    0,
		"weird":                 0,
	}
	for name, want := range cases {
		if got := hookflow.RecoveryAttempt(name); got != want {
			t.Errorf("hookflow.RecoveryAttempt(%q) = %d, want %d", name, got, want)
		}
	}
}

// TestDelivery_IdempotencyKeyStableAcrossRespool the Idempotency-Key the
// server deduplicates on must be identical across a re-send, or the retry
// above would double-count instead of dedupe.
func TestDelivery_IdempotencyKeyStableAcrossRespool(t *testing.T) {
	dir := t.TempDir()
	s := hookflow.Spool{Dir: dir}
	m := testMapper()
	original, ok := m.Map(HookPreToolUse, &HookEvent{
		SessionID: "s1", ToolName: "Bash", ToolUseID: "toolu_1", PermissionMode: "default",
	})
	if !ok {
		t.Fatal("map failed")
	}
	if err := s.Append(original); err != nil {
		t.Fatalf("append: %v", err)
	}

	var firstSend, secondSend string
	fail := func(_ context.Context, ev client.DevEvent) error {
		firstSend = ev.EventID
		return errors.New("network down")
	}
	if _, err := s.FlushAll(context.Background(), fail); err != nil {
		t.Fatalf("first flush: %v", err)
	}
	succeed := func(_ context.Context, ev client.DevEvent) error {
		secondSend = ev.EventID
		return nil
	}
	if _, err := s.FlushAll(context.Background(), succeed); err != nil {
		t.Fatalf("second flush: %v", err)
	}

	if firstSend == "" || secondSend == "" {
		t.Fatalf("event was not sent twice: %q then %q", firstSend, secondSend)
	}
	if firstSend != secondSend {
		t.Errorf("event_id changed across respool (%q → %q): the server would see two "+
			"different Idempotency-Keys and count the event twice", firstSend, secondSend)
	}
	if firstSend != original.EventID {
		t.Errorf("spooled id %q does not match the mapped id %q", firstSend, original.EventID)
	}
}

// TestDelivery_CorruptLineSkipped a corrupt line must not be retried forever
// either; it is skipped, as before.
func TestDelivery_CorruptLineSkipped(t *testing.T) {
	dir := t.TempDir()
	s := hookflow.Spool{Dir: dir}
	path := filepath.Join(dir, "s1.jsonl")
	if err := os.WriteFile(path, []byte("{not json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	n, err := s.FlushAll(context.Background(), func(context.Context, client.DevEvent) error {
		t.Error("a corrupt line should never reach the emitter")
		return nil
	})
	if err != nil || n != 0 {
		t.Fatalf("corrupt drain = (%d, %v), want (0, nil)", n, err)
	}
	left, _ := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	for _, f := range left {
		if strings.Contains(filepath.Base(f), ".rec") {
			t.Errorf("a corrupt line must be dropped, not carried over: %v", left)
		}
	}
}

// TestEvidenceState_ReportedOnSessionEnd the SessionEnded event reports
// telemetry completeness as the client can see it: nothing carried over ⇒
// complete; a failed earlier flush ⇒ degraded with a count.
func TestEvidenceState_ReportedOnSessionEnd(t *testing.T) {
	m := testMapper()

	m.Evidence = &EvidenceState{Undelivered: 0}
	clean, ok := m.Map(HookSessionEnd, &HookEvent{SessionID: "s1", Reason: "other"})
	if !ok {
		t.Fatal("SessionEnd did not map")
	}
	if clean.Metadata["evidence_state"] != "complete" {
		t.Errorf("clean session should be complete, got %v", clean.Metadata)
	}
	if _, present := clean.Metadata["evidence_undelivered"]; present {
		t.Errorf("no count should be emitted when nothing is undelivered: %v", clean.Metadata)
	}

	m.Evidence = &EvidenceState{Undelivered: 3}
	degraded, _ := m.Map(HookSessionEnd, &HookEvent{SessionID: "s1", Reason: "other"})
	if degraded.Metadata["evidence_state"] != "degraded" || degraded.Metadata["evidence_undelivered"] != 3 {
		t.Errorf("degraded session mis-reported: %v", degraded.Metadata)
	}

	for _, hook := range []HookName{HookSessionStart, HookPreToolUse, HookUserPromptSubmit} {
		ev, _ := m.Map(hook, &HookEvent{SessionID: "s1", ToolName: "Bash"})
		if _, present := ev.Metadata["evidence_state"]; present {
			t.Errorf("%s must not carry evidence_state", hook)
		}
	}
}

// TestEvidenceState_CountsOnlyCarriedOverEvents undeliveredCount counts carry-
// over files only.
func TestEvidenceState_CountsOnlyCarriedOverEvents(t *testing.T) {
	dir := t.TempDir()
	s := hookflow.Spool{Dir: dir}
	if err := s.Append(client.DevEvent{EventID: "evt-live", SessionID: "s1"}); err != nil {
		t.Fatal(err)
	}
	if got := s.UndeliveredCount(); got != 0 {
		t.Errorf("a not-yet-flushed event is not undelivered evidence, got %d", got)
	}

	if _, err := s.FlushAll(context.Background(), func(context.Context, client.DevEvent) error {
		return errors.New("network down")
	}); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if got := s.UndeliveredCount(); got != 1 {
		t.Errorf("after a failed flush the event is undelivered evidence, got %d", got)
	}

	if got := (hookflow.Spool{Dir: filepath.Join(dir, "nope")}).UndeliveredCount(); got != 0 {
		t.Errorf("missing dir should report 0, got %d", got)
	}
}
