package hookflow

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/trace"
)

// TestMaxBodyBytesMatchesMaxRedactBody is the phase-2 contract check: the
// trace's per-body cap must never diverge from the cap the in-process secret
// detector already applies, or a trace record could store MORE of a body
// than the redaction path itself ever reasons about.
func TestMaxBodyBytesMatchesMaxRedactBody(t *testing.T) {
	if trace.MaxBodyBytes != MaxRedactBody {
		t.Fatalf("trace.MaxBodyBytes (%d) != hookflow.MaxRedactBody (%d)", trace.MaxBodyBytes, MaxRedactBody)
	}
}

// evalEmitterFunc adapts a plain func to the Emitter interface, for a fake
// core this test controls directly, including its returned Evaluation
// (deliveryhalt_test.go's own emitterFunc always returns the zero
// Evaluation, which does not fit a test that needs a real verdict).
type evalEmitterFunc func(context.Context, client.DevEvent) (client.Evaluation, error)

func (f evalEmitterFunc) Emit(ctx context.Context, ev client.DevEvent) (client.Evaluation, error) {
	return f(ctx, ev)
}

func withTraceDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	restore := trace.SetDefault(&trace.Writer{Dir: dir})
	t.Cleanup(restore)
	return dir
}

// TestDeliverTracesOneAttemptAndOneResult covers the phase-2 test: a Deliver
// call produces exactly one deliver.attempt + one deliver.result row, and the
// egressed event it carries is the (already redacted) form Deliver actually
// received -- never a secret that never reached this function in the first
// place.
func TestDeliverTracesOneAttemptAndOneResult(t *testing.T) {
	dir := withTraceDir(t)

	secretLike := "AKIA" + strings.Repeat("Q", 16) // AWS-access-key SHAPED, derived here, never real
	const redactedMarker = "[REDACTED:aws_key]"
	ev := client.DevEvent{
		EventID:   "ev-attempt-1",
		SessionID: "sess-attempt-1",
		EventType: client.EventToolCall,
		Content:   &client.Content{FileText: redactedMarker},
		Metadata:  map[string]any{"provider": "claude-code"},
	}

	em := evalEmitterFunc(func(context.Context, client.DevEvent) (client.Evaluation, error) {
		return client.Evaluation{Verdict: client.VerdictAllow}, nil
	})
	if _, err := Deliver(context.Background(), em, nil, ev, nil); err != nil {
		t.Fatalf("Deliver: %v", err)
	}

	recs, skipped, err := trace.Read(dir, nil)
	if err != nil {
		t.Fatalf("trace.Read: %v", err)
	}
	if skipped != 0 {
		t.Fatalf("trace.Read skipped %d corrupt line(s)", skipped)
	}

	var attempts, results int
	for _, r := range recs {
		body := fmt.Sprintf("%v", r.Detail["event"])
		switch r.Stage {
		case trace.StageDeliverAttempt:
			attempts++
			if r.SessionID != ev.SessionID || r.EventID != ev.EventID {
				t.Errorf("deliver.attempt missing event ids: %+v", r)
			}
		case trace.StageDeliverResult:
			results++
			if r.Outcome != "accepted" {
				t.Errorf("deliver.result outcome = %q, want accepted", r.Outcome)
			}
			if !strings.Contains(body, redactedMarker) {
				t.Errorf("deliver.result should carry the redacted content, got %s", body)
			}
		}
		if strings.Contains(body, secretLike) {
			t.Fatalf("a trace record carried the raw secret, never handed to Deliver: %s", body)
		}
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1", attempts)
	}
	if results != 1 {
		t.Errorf("results = %d, want 1", results)
	}
}

// TestDeliverRetryTracesTwoAttempts covers the phase-2 test: a retried
// delivery (one transient failure, then success, through
// Spool.attemptLines/DrainSession -- the one retry every drainer allows) is
// visible as two attempt/result pairs, not one.
func TestDeliverRetryTracesTwoAttempts(t *testing.T) {
	dir := withTraceDir(t)

	spoolDir := t.TempDir()
	s := Spool{Dir: spoolDir}
	ev := client.DevEvent{EventID: "ev-retry-1", SessionID: "sess-retry-1", EventType: client.EventToolResult}
	if err := s.Append(ev); err != nil {
		t.Fatalf("Append: %v", err)
	}

	var calls int
	em := evalEmitterFunc(func(context.Context, client.DevEvent) (client.Evaluation, error) {
		calls++
		if calls == 1 {
			return client.Evaluation{}, fmt.Errorf("%w: dial tcp: connection refused", client.ErrDelivery)
		}
		return client.Evaluation{Verdict: client.VerdictAllow}, nil
	})
	fn := func(ctx context.Context, ev client.DevEvent) error {
		_, err := Deliver(ctx, em, nil, ev, nil)
		return err
	}

	n, err := s.DrainSession(context.Background(), ev.SessionID, fn, DrainOptions{Mode: Block, AttemptTimeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("DrainSession: %v", err)
	}
	if n != 1 {
		t.Fatalf("DrainSession delivered = %d, want 1", n)
	}
	if calls != 2 {
		t.Fatalf("emitter called %d time(s), want 2 (one retry)", calls)
	}

	recs, _, err := trace.Read(dir, nil)
	if err != nil {
		t.Fatalf("trace.Read: %v", err)
	}
	var attempts, results int
	var ordinals []int
	for _, r := range recs {
		switch r.Stage {
		case trace.StageDeliverAttempt:
			attempts++
			ordinals = append(ordinals, r.Attempt)
		case trace.StageDeliverResult:
			results++
		}
	}
	if attempts != 2 {
		t.Errorf("attempts = %d, want 2", attempts)
	}
	if results != 2 {
		t.Errorf("results = %d, want 2", results)
	}
	if len(ordinals) != 2 || ordinals[0] != 1 || ordinals[1] != 2 {
		t.Errorf("attempt ordinals = %v, want [1 2]", ordinals)
	}
}

// TestTraceHookInCarriesRawStdinEvenOnParseFailure covers the phase-2 test:
// hook.in records the raw payload -- including a secret-shaped value -- even
// when the payload cannot be parsed at all.
func TestTraceHookInCarriesRawStdinEvenOnParseFailure(t *testing.T) {
	dir := withTraceDir(t)

	secretLike := "AKIA" + strings.Repeat("Q", 16) // AWS-access-key SHAPED, derived here, never real
	raw := []byte(`{not valid json, but contains ` + secretLike)

	TraceHookIn("claude-code", "PreToolUse", "", raw, fmt.Errorf("parse hook payload: unexpected end of JSON input"))

	recs, _, err := trace.Read(dir, nil)
	if err != nil {
		t.Fatalf("trace.Read: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("got %d record(s), want 1", len(recs))
	}
	r := recs[0]
	if r.Stage != trace.StageHookIn {
		t.Fatalf("stage = %q, want %q", r.Stage, trace.StageHookIn)
	}
	if r.Outcome != "parse_error" || r.Err == "" {
		t.Errorf("expected a recorded parse error, got outcome=%q err=%q", r.Outcome, r.Err)
	}
	body := fmt.Sprintf("%v", r.Detail["stdin"])
	if !strings.Contains(body, secretLike) {
		t.Errorf("hook.in should carry the raw stdin including the secret-shaped value, got %s", body)
	}
}

// TestHookOutputBufferKeepsOnlyWhatTheTraceCanStore: the copy of a hook's
// stdout kept for its hook.out record stops growing at the trace's own
// per-body cap, while every byte still counts and the write never fails.
func TestHookOutputBufferKeepsOnlyWhatTheTraceCanStore(t *testing.T) {
	var b HookOutputBuffer
	chunk := strings.Repeat("x", 1<<20)
	for i := 0; i < 3; i++ {
		n, err := b.Write([]byte(chunk))
		if err != nil || n != len(chunk) {
			t.Fatalf("Write = %d, %v; want %d, nil", n, err, len(chunk))
		}
	}
	if got := len(b.Bytes()); got != trace.MaxBodyBytes {
		t.Fatalf("kept %d bytes, want the %d-byte cap", got, trace.MaxBodyBytes)
	}
	if got := b.Total(); got != 3<<20 {
		t.Fatalf("Total() = %d, want %d", got, 3<<20)
	}
}

// TestDeliverRecordsNameTheAttemptActivityAndLane: a delivery record joins
// its core row by the wire activity_id, says which lane produced it, and
// says which attempt it was -- the retry is attempt 2, not a second attempt 1.
func TestDeliverRecordsNameTheAttemptActivityAndLane(t *testing.T) {
	dir := withTraceDir(t)
	em := evalEmitterFunc(func(context.Context, client.DevEvent) (client.Evaluation, error) {
		return client.Evaluation{Verdict: client.VerdictAllow}, nil
	})
	ev := client.DevEvent{EventID: "e-lane", SessionID: "s-lane", EventType: client.EventTurnStarted, ProxyRequestID: "req_lane"}
	if _, err := Deliver(WithDeliveryAttempt(context.Background(), 2), em, nil, ev, nil); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	recs, _, err := trace.Read(dir, func(r trace.Record) bool { return r.Stage == trace.StageDeliverResult })
	if err != nil || len(recs) != 1 {
		t.Fatalf("Read = %d, %v; want 1 deliver.result", len(recs), err)
	}
	r := recs[0]
	if r.ActivityID != "s-lane:proxy:req_lane" || r.Lane != "proxy" || r.Attempt != 2 {
		t.Fatalf("activity=%q lane=%q attempt=%d; want s-lane:proxy:req_lane, proxy, 2", r.ActivityID, r.Lane, r.Attempt)
	}
}
