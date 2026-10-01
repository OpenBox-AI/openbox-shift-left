package telemetryemit

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/trace"
)

type sink struct {
	mu     sync.Mutex
	events []client.DevEvent
	refuse map[client.EventType]bool
	got    chan struct{}
}

func newSink() *sink { return &sink{got: make(chan struct{}, 16)} }

func (s *sink) deliver(_ context.Context, ev client.DevEvent) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.refuse[ev.EventType] {
		return false
	}
	s.events = append(s.events, ev)
	s.got <- struct{}{}
	return true
}

func (s *sink) snapshot() []client.DevEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]client.DevEvent(nil), s.events...)
}

func (s *sink) waitFor(t *testing.T, n int) []client.DevEvent {
	t.Helper()
	for i := 0; i < n; i++ {
		select {
		case <-s.got:
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for delivery %d of %d", i+1, n)
		}
	}
	return s.snapshot()
}

func enrichEmitter(s *sink, en *Enricher) *Emitter {
	return &Emitter{
		Mapper:  museMapper(),
		DID:     func() string { return testDID },
		Deliver: s.deliver,
		Enrich:  en,
	}
}

func scratchTrace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	restore := trace.SetDefault(&trace.Writer{Dir: dir})
	t.Cleanup(restore)
	return dir
}

func findings(t *testing.T, dir, outcome string) []map[string]any {
	t.Helper()
	recs, _, err := trace.Read(dir, func(r trace.Record) bool { return r.Outcome == outcome })
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, r := range recs {
		out = append(out, r.Detail)
	}
	return out
}

func TestNilEnricherDeliversTodaysPairSynchronously(t *testing.T) {
	s := newSink()
	em := enrichEmitter(s, nil)
	if err := em.Emit(context.Background(), museModelCall(nil)); err != nil {
		t.Fatal(err)
	}
	got := s.snapshot()
	want, _ := museMapper().EventsFor(museModelCall(nil))
	if len(got) != 2 {
		t.Fatalf("delivered %d, want the pair at once", len(got))
	}
	for i := range want {
		want[i].DeveloperDID = testDID
		if !reflect.DeepEqual(got[i], want[i]) {
			t.Errorf("half %d differs from the mapper's own output:\n got %+v\nwant %+v", i, got[i], want[i])
		}
	}
}

func TestEnrichedPairCarriesBodiesAndNeverBlocksEmit(t *testing.T) {
	s := newSink()
	release := make(chan struct{})
	var call Call
	em := enrichEmitter(s, &Enricher{
		Outcome: "muse.content",
		Enrich: func(_ context.Context, c Call) Result {
			call = c
			<-release
			return Result{Request: `{"messages":[]}`, Response: `{"output":[]}`}
		},
	})
	done := make(chan struct{})
	go func() { _ = em.Emit(context.Background(), museModelCall(nil)); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Emit blocked on the enrichment")
	}
	if n := len(s.snapshot()); n != 0 {
		t.Fatalf("%d event(s) delivered before the enrichment finished", n)
	}
	close(release)
	em.Wait()
	got := s.snapshot()
	if len(got) != 2 || got[0].EventType != client.EventTurnStarted || got[1].EventType != client.EventTurnCompleted {
		t.Fatalf("delivery = %+v, want Started then Completed", got)
	}
	if got[0].Span.RequestBody != `{"messages":[]}` || got[0].Span.ResponseBody != "" {
		t.Errorf("Started span = %+v, want only the request body", got[0].Span)
	}
	if got[1].Span.ResponseBody != `{"output":[]}` || got[1].Span.RequestBody != "" {
		t.Errorf("Completed span = %+v, want only the response body", got[1].Span)
	}
	if got[0].DeveloperDID != testDID {
		t.Errorf("DID = %q", got[0].DeveloperDID)
	}
	if call.RequestID != "resp_scrubbed0002" || call.Session != museMain {
		t.Errorf("call = %+v", call)
	}
	if d := time.Until(call.Deadline); d <= 0 || d > DefaultEnrichWait {
		t.Errorf("deadline is %v away, want within (0, %v]", d, DefaultEnrichWait)
	}
}

func TestEnrichmentLooksUpTheUnfoldedSessionButShipsTheFoldedOne(t *testing.T) {
	s := newSink()
	var seen Call
	m := museMapper().WithParentOf(func(child string) string {
		if child == museChild {
			return museMain
		}
		return ""
	})
	em := &Emitter{Mapper: m, DID: func() string { return testDID }, Deliver: s.deliver,
		Enrich: &Enricher{Enrich: func(_ context.Context, c Call) Result { seen = c; return Result{} }}}
	_ = em.Emit(context.Background(), museModelCall(map[string]string{"session_id": museChild}))
	em.Wait()
	if seen.Session != museChild {
		t.Errorf("enricher saw session %q, want the child's own %q", seen.Session, museChild)
	}
	for _, ev := range s.snapshot() {
		if ev.SessionID != museMain {
			t.Errorf("event session = %q, want the folded parent %q", ev.SessionID, museMain)
		}
	}
}

func TestDisabledEnricherReadsNothingAndDeliversAtOnce(t *testing.T) {
	s := newSink()
	called := false
	em := enrichEmitter(s, &Enricher{
		Enabled: func() bool { return false },
		Enrich:  func(context.Context, Call) Result { called = true; return Result{} },
	})
	_ = em.Emit(context.Background(), museModelCall(nil))
	if len(s.snapshot()) != 2 {
		t.Error("a disabled enricher must deliver the pair synchronously")
	}
	em.Wait()
	if called {
		t.Error("the enricher ran with capture off")
	}
}

func TestSaturatedEnrichmentDeliversMetadataOnlyAtOnce(t *testing.T) {
	dir := scratchTrace(t)
	s := newSink()
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	em := enrichEmitter(s, &Enricher{
		Outcome: "muse.content", MaxInFlight: 1,
		Enrich: func(context.Context, Call) Result {
			started <- struct{}{}
			<-release
			return Result{Request: "req", Response: "resp"}
		},
	})
	_ = em.Emit(context.Background(), museModelCall(nil))
	<-started
	_ = em.Emit(context.Background(), museModelCall(map[string]string{"gen_ai_response_id": "resp_second"}))
	got := s.snapshot()
	if len(got) != 2 || got[0].OtelRequestID != "resp_second" {
		t.Fatalf("second call not delivered at once: %+v", got)
	}
	for _, ev := range got {
		if ev.Span.RequestBody != "" || ev.Span.ResponseBody != "" {
			t.Errorf("a saturated call carries a body: %+v", ev.Span)
		}
	}
	if f := findings(t, dir, "muse.content"); len(f) != 1 || f[0]["reason"] != "enrich_saturated" {
		t.Errorf("findings = %v, want one enrich_saturated", f)
	}
	close(release)
	em.Wait()
	if len(s.snapshot()) != 4 {
		t.Errorf("delivered %d, want both pairs", len(s.snapshot()))
	}
}

func TestShutdownDeliversMetadataOnlyWithoutEnriching(t *testing.T) {
	dir := scratchTrace(t)
	s := newSink()
	life, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	em := enrichEmitter(s, &Enricher{Outcome: "muse.content",
		Enrich: func(context.Context, Call) Result { called = true; return Result{} }})
	em.Lifetime = life
	_ = em.Emit(context.Background(), museModelCall(nil))
	if len(s.snapshot()) != 2 || called {
		t.Errorf("delivered %d, enricher called = %v; want the pair at once and no enrichment", len(s.snapshot()), called)
	}
	if f := findings(t, dir, "muse.content"); len(f) != 1 || f[0]["reason"] != "shutdown" {
		t.Errorf("findings = %v, want one shutdown", f)
	}
}

func TestMissesAreTracedWithoutContentAndTheRowStillShips(t *testing.T) {
	dir := scratchTrace(t)
	s := newSink()
	em := enrichEmitter(s, &Enricher{Outcome: "muse.content",
		Enrich: func(context.Context, Call) Result {
			return Result{Response: "only the reply", Misses: []Miss{{Part: "request", Reason: "stash_absent"}}}
		}})
	_ = em.Emit(context.Background(), museModelCall(nil))
	em.Wait()
	got := s.snapshot()
	if len(got) != 2 || got[0].Span.RequestBody != "" || got[1].Span.ResponseBody != "only the reply" {
		t.Fatalf("each half must carry what it has: %+v", got)
	}
	f := findings(t, dir, "muse.content")
	if len(f) != 1 || f[0]["part"] != "request" || f[0]["reason"] != "stash_absent" {
		t.Fatalf("findings = %v", f)
	}
	for k, v := range f[0] {
		if str, ok := v.(string); ok && (str == "only the reply") {
			t.Errorf("finding %q carries content", k)
		}
	}
}

func TestEnricherPanicShipsMetadataOnly(t *testing.T) {
	scratchTrace(t)
	s := newSink()
	em := enrichEmitter(s, &Enricher{Enrich: func(context.Context, Call) Result { panic("boom") }})
	_ = em.Emit(context.Background(), museModelCall(nil))
	em.Wait()
	if len(s.snapshot()) != 2 {
		t.Errorf("a panicking enricher lost the row: %d delivered", len(s.snapshot()))
	}
}

func TestEnrichedCompletedIsNeverSentAfterARefusedStarted(t *testing.T) {
	scratchTrace(t)
	s := newSink()
	s.refuse = map[client.EventType]bool{client.EventTurnStarted: true}
	em := enrichEmitter(s, &Enricher{Enrich: func(context.Context, Call) Result { return Result{Request: "r", Response: "p"} }})
	_ = em.Emit(context.Background(), museModelCall(nil))
	em.Wait()
	if n := len(s.snapshot()); n != 0 {
		t.Errorf("delivered %d event(s) after Started was refused", n)
	}
}
