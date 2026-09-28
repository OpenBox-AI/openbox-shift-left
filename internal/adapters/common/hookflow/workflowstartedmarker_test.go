package hookflow

import (
	"context"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/client"
)

// TestSpool_WorkflowStartedDeliveryWritesTheMarker pins owner ruling
// 2026-09-28 (1)'s write side: a WorkflowStarted event (client.
// EventSessionStarted, the wire type client/payload.go's wireTypeFor maps
// to WorkflowStarted) that is actually delivered writes the accepted
// marker, readable back through WorkflowStartedAccepted.
func TestSpool_WorkflowStartedDeliveryWritesTheMarker(t *testing.T) {
	s := Spool{Dir: t.TempDir()}
	ev := client.DevEvent{
		SchemaVersion: client.SchemaVersion,
		EventID:       "e1",
		EventType:     client.EventSessionStarted,
		SessionID:     "sess-1",
		DeveloperDID:  "did:aip:dev",
		Timestamp:     "2026-08-01T12:00:00Z",
	}
	if err := s.Append(ev); err != nil {
		t.Fatalf("append: %v", err)
	}

	if s.WorkflowStartedAccepted("sess-1") {
		t.Fatal("the marker must not exist before delivery is even attempted")
	}

	n, err := s.DrainSession(context.Background(), "sess-1", func(context.Context, client.DevEvent) error { return nil },
		DrainOptions{Mode: Block, AttemptTimeout: time.Second})
	if err != nil || n != 1 {
		t.Fatalf("DrainSession: n=%d err=%v, want 1 delivered", n, err)
	}

	if !s.WorkflowStartedAccepted("sess-1") {
		t.Error("a delivered WorkflowStarted event must leave a marker WorkflowStartedAccepted can read back")
	}
}

// TestSpool_ToolCallDeliveryWritesNoMarker: only a WorkflowStarted event
// writes the marker; an ordinary ToolCall event, even successfully
// delivered, must never be mistaken for one.
func TestSpool_ToolCallDeliveryWritesNoMarker(t *testing.T) {
	s := Spool{Dir: t.TempDir()}
	ev := client.DevEvent{
		SchemaVersion: client.SchemaVersion,
		EventID:       "e1",
		EventType:     client.EventToolCall,
		SessionID:     "sess-2",
		DeveloperDID:  "did:aip:dev",
		Timestamp:     "2026-08-01T12:00:00Z",
	}
	if err := s.Append(ev); err != nil {
		t.Fatalf("append: %v", err)
	}
	n, err := s.DrainSession(context.Background(), "sess-2", func(context.Context, client.DevEvent) error { return nil },
		DrainOptions{Mode: Block, AttemptTimeout: time.Second})
	if err != nil || n != 1 {
		t.Fatalf("DrainSession: n=%d err=%v, want 1 delivered", n, err)
	}

	if s.WorkflowStartedAccepted("sess-2") {
		t.Error("a delivered ToolCall event must never write the WorkflowStarted marker")
	}
}

// TestSpool_FailedWorkflowStartedWritesNoMarker: a WorkflowStarted event
// that FAILS to deliver must never leave a marker behind -- presence must
// mean "core has this", never "an attempt was merely made".
func TestSpool_FailedWorkflowStartedWritesNoMarker(t *testing.T) {
	s := Spool{Dir: t.TempDir()}
	ev := client.DevEvent{
		SchemaVersion: client.SchemaVersion,
		EventID:       "e1",
		EventType:     client.EventSessionStarted,
		SessionID:     "sess-3",
		DeveloperDID:  "did:aip:dev",
		Timestamp:     "2026-08-01T12:00:00Z",
	}
	if err := s.Append(ev); err != nil {
		t.Fatalf("append: %v", err)
	}
	_, _ = s.DrainSession(context.Background(), "sess-3",
		func(context.Context, client.DevEvent) error { return client.ErrUnbuildable },
		DrainOptions{Mode: Block, AttemptTimeout: time.Second})

	if s.WorkflowStartedAccepted("sess-3") {
		t.Error("a failed WorkflowStarted delivery must never write the marker")
	}
}

// TestSpool_WorkflowStartedAcceptedUnknownRunIsFalse: an unwritten marker
// (a run whose WorkflowStarted this spool never drained) reads as
// unproven, not "not started" -- the same "unknown" a gate before this
// marker existed always saw.
func TestSpool_WorkflowStartedAcceptedUnknownRunIsFalse(t *testing.T) {
	s := Spool{Dir: t.TempDir()}
	if s.WorkflowStartedAccepted("never-seen") {
		t.Error("an unwritten marker must read as unproven (false), never true")
	}
	if s.WorkflowStartedAccepted("") {
		t.Error("an empty run id must never read as accepted")
	}
}
