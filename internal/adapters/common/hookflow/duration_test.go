package hookflow

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/client"
)

func readSpooledEvents(t *testing.T, spoolDir, sessionID string) []client.DevEvent {
	t.Helper()
	data, err := os.ReadFile((Spool{Dir: spoolDir}).SessionPath(sessionID))
	if err != nil {
		t.Fatalf("read spool: %v", err)
	}
	var out []client.DevEvent
	for _, line := range NonEmptyLines(data) {
		var ev client.DevEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			t.Fatalf("decode spooled event: %v", err)
		}
		out = append(out, ev)
	}
	return out
}

func clockSeq(times ...time.Time) func() time.Time {
	i := 0
	return func() time.Time {
		t := times[i]
		if i < len(times)-1 {
			i++
		}
		return t
	}
}

func TestDurationStash_PutTakeClear(t *testing.T) {
	d := DurationStash{Dir: t.TempDir()}
	const sess, key, ts = "s1", "k1", "2026-07-15T12:00:00Z"

	if err := d.PutStart(sess, key, ts); err != nil {
		t.Fatalf("PutStart: %v", err)
	}
	if got := d.TakeStart(sess, key); got != ts {
		t.Fatalf("TakeStart = %q, want %q", got, ts)
	}
	if got := d.TakeStart(sess, key); got != "" {
		t.Fatalf("second TakeStart = %q, want empty (record removed on read)", got)
	}
}

func TestDurationStash_TakeMissingIsEmpty(t *testing.T) {
	d := DurationStash{Dir: t.TempDir()}
	if got := d.TakeStart("s1", "never-written"); got != "" {
		t.Fatalf("TakeStart on missing = %q, want empty", got)
	}
}

func TestDurationStash_ClearSessionSweepsUnpaired(t *testing.T) {
	root := t.TempDir()
	d := DurationStash{Dir: root}
	if err := d.PutStart("s1", "orphan", "2026-07-15T12:00:00Z"); err != nil {
		t.Fatalf("PutStart: %v", err)
	}
	d.ClearSession("s1")
	if got := d.TakeStart("s1", "orphan"); got != "" {
		t.Fatalf("TakeStart after clear = %q, want empty", got)
	}
}

func TestDurationStash_EmptyDirIsInert(t *testing.T) {
	var d DurationStash // zero value: Dir==""
	if err := d.PutStart("s1", "k", "2026-07-15T12:00:00Z"); err != nil {
		t.Fatalf("PutStart on empty-dir stash must be a no-op, got %v", err)
	}
	if got := d.TakeStart("s1", "k"); got != "" {
		t.Fatalf("TakeStart on empty-dir stash = %q, want empty", got)
	}
	d.ClearSession("s1") // must not panic
}

// TestDurationStash_PutTakePairRoundTrip the pair record (start time plus the
// started half's operation id) survives a write/read cycle intact, and is
// removed on read like PutStart/TakeStart.
func TestDurationStash_PutTakePairRoundTrip(t *testing.T) {
	d := DurationStash{Dir: t.TempDir()}
	const sess, key = "s1", "k1"
	rec := pairRecord{StartedAt: "2026-07-15T12:00:00Z", OperationID: "args:deadbeef"}

	if err := d.putPair(sess, key, rec); err != nil {
		t.Fatalf("putPair: %v", err)
	}
	if got := d.takePair(sess, key); got != rec {
		t.Fatalf("takePair = %+v, want %+v", got, rec)
	}
	if got := d.takePair(sess, key); got != (pairRecord{}) {
		t.Fatalf("second takePair = %+v, want the zero value (record removed on read)", got)
	}
}

// TestDurationStash_TakePairParsesALegacyBareTimestamp a record written before
// the pair format existed is a bare RFC3339 timestamp, no JSON. A session that
// straddles an upgrade must still recover its duration from one.
func TestDurationStash_TakePairParsesALegacyBareTimestamp(t *testing.T) {
	d := DurationStash{Dir: t.TempDir()}
	const sess, key, ts = "s1", "k1", "2026-07-15T12:00:00Z"
	if err := d.putRaw(sess, key, []byte(ts)); err != nil {
		t.Fatalf("seed legacy record: %v", err)
	}
	got := d.takePair(sess, key)
	if got.StartedAt != ts || got.OperationID != "" {
		t.Fatalf("takePair on a legacy bare-timestamp record = %+v, want StartedAt=%q OperationID=\"\"", got, ts)
	}
}

func TestDurationStash_TakePairMissingIsZeroValue(t *testing.T) {
	d := DurationStash{Dir: t.TempDir()}
	if got := d.takePair("s1", "never-written"); got != (pairRecord{}) {
		t.Fatalf("takePair on missing = %+v, want the zero value", got)
	}
}

// TestPairKey_FallsBackToToolCallStartKeyWithoutAnInvocationID pairKey keys on
// the invocation id alone when the event carries one; otherwise it is exactly
// ToolCallStartKey.
func TestPairKey_FallsBackToToolCallStartKeyWithoutAnInvocationID(t *testing.T) {
	noSpan := client.DevEvent{SessionID: "s1", Tool: client.Tool{Name: "Bash"}}
	if got, want := pairKey(noSpan), ToolCallStartKey(noSpan); got != want {
		t.Errorf("pairKey with no Span = %q, want the ToolCallStartKey fallback %q", got, want)
	}

	noInvocation := client.DevEvent{SessionID: "s1", Tool: client.Tool{Name: "Bash"}, Span: &client.Span{}}
	if got, want := pairKey(noInvocation), ToolCallStartKey(noInvocation); got != want {
		t.Errorf("pairKey with an empty invocation id = %q, want the ToolCallStartKey fallback %q", got, want)
	}

	a := client.DevEvent{SessionID: "s1", Span: &client.Span{InvocationID: "tu_1", OperationID: "args:aaa"}}
	b := client.DevEvent{SessionID: "s1", Span: &client.Span{InvocationID: "tu_1", OperationID: "args:bbb"}}
	if pairKey(a) != pairKey(b) {
		t.Error("pairKey must key on the invocation id alone, ignoring a differing operation id")
	}

	c := client.DevEvent{SessionID: "s1", Span: &client.Span{InvocationID: "tu_2", OperationID: "args:aaa"}}
	if pairKey(a) == pairKey(c) {
		t.Error("two different invocation ids collided")
	}
}

// A model-call gate's start is recovered once, by request id, and never crosses
// into a tool call's pairing keys or another gate.
func TestModelCallStartRoundTrip(t *testing.T) {
	d := DurationStash{Dir: t.TempDir()}
	if err := d.PutModelCallStart("s", "req.0", "2026-10-01T12:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if got := d.TakeModelCallStart("s", "other.0"); got != "" {
		t.Errorf("another gate's start = %q, want none", got)
	}
	if got := d.TakeStart("s", "req.0"); got != "" {
		t.Errorf("a tool pairing key read a model-call start: %q", got)
	}
	if got := d.TakeModelCallStart("s", "req.0"); got != "2026-10-01T12:00:00Z" {
		t.Errorf("start = %q", got)
	}
	if got := d.TakeModelCallStart("s", "req.0"); got != "" {
		t.Errorf("a start was readable twice: %q", got)
	}
	if err := d.PutModelCallStart("s", "", "2026-10-01T12:00:00Z"); err != nil || d.TakeModelCallStart("s", "") != "" {
		t.Error("an empty request id must record nothing")
	}
}
