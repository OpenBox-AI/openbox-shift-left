package fakecore

import (
	"net/http"
	"testing"
	"time"
)

// TestServerAttemptsByKey_CountsEveryRequestIncludingAnOutage an outage (or
// any other early-refusal branch) must not undercount: AttemptsByKey exists
// so a grader can prove "sent twice, same key" even when one of those
// attempts never reached far enough to get an ordinary verdict.
func TestServerAttemptsByKey_CountsEveryRequestIncludingAnOutage(t *testing.T) {
	f := New(t, Script{})
	body := []byte(`{"source":"developer-runtime","event_type":"WorkflowStarted","workflow_id":"w","run_id":"r","timestamp":"2026-09-14T00:00:00Z"}`)

	post(t, f, body, "backlog-1")

	f.SetOutage(true)
	req := v3AuthedRequest(t, f, body)
	req.Header.Set("Idempotency-Key", "backlog-1")
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatalf("post during outage: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status during outage = %d, want 503", resp.StatusCode)
	}

	if got := f.AttemptsByKey()["backlog-1"]; got != 2 {
		t.Errorf("attempts for backlog-1 = %d, want 2; an outage response must still be counted", got)
	}
}

// TestServerHeldByKey_CountsOnlyTheAttemptsThisScriptDelayed HeldByKey
// narrows AttemptsByKey to the attempts this Script's own Delay/DelayFor
// actually held (delay > 0), so a grader can tell "resent because this
// attempt's own answer was held" apart from any other reason an event might
// be resent.
func TestServerHeldByKey_CountsOnlyTheAttemptsThisScriptDelayed(t *testing.T) {
	f := New(t, Script{
		DelayFor: map[string]time.Duration{
			"WorkflowStarted": 30 * time.Millisecond,
		},
	})
	held := []byte(`{"source":"developer-runtime","event_type":"WorkflowStarted","workflow_id":"w","run_id":"r","timestamp":"2026-09-14T00:00:00Z"}`)
	notHeld := []byte(`{"source":"developer-runtime","event_type":"WorkflowCompleted","workflow_id":"w","run_id":"r","timestamp":"2026-09-14T00:00:00Z"}`)

	post(t, f, held, "held-key")
	post(t, f, notHeld, "not-held-key")

	attempts := f.AttemptsByKey()
	if attempts["held-key"] != 1 || attempts["not-held-key"] != 1 {
		t.Fatalf("attempts by key = %+v, want exactly one each", attempts)
	}

	heldByKey := f.HeldByKey()
	if heldByKey["held-key"] != 1 {
		t.Errorf("held-key held count = %d, want 1", heldByKey["held-key"])
	}
	if heldByKey["not-held-key"] != 0 {
		t.Errorf("not-held-key held count = %d, want 0; DelayFor never applied to it", heldByKey["not-held-key"])
	}
}
