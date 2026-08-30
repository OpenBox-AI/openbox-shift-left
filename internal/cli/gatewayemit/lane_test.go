package gatewayemit

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/gateway"
)

// Lane_test.go; the two in-path model-call producers share this emitter. The
// namespaces are client's (turnActivityIDFor); what this file pins is the half
// that lives here: which discriminator field a lane writes, and that a lane
// nobody configured cannot silently borrow another lane's.

// TestEachLaneWritesOnlyItsOwnDiscriminator.
func TestEachLaneWritesOnlyItsOwnDiscriminator(t *testing.T) {
	t.Run("gateway", func(t *testing.T) {
		ev, err := EventFor(LaneGateway, sampleIdentity(), "req-1", sampleAt, sampleCaptured())
		if err != nil {
			t.Fatalf("EventFor: %v", err)
		}
		if ev.GatewayRequestID != "req-1" {
			t.Errorf("GatewayRequestID = %q, want req-1", ev.GatewayRequestID)
		}
		if ev.ProxyRequestID != "" || ev.OtelRequestID != "" {
			t.Errorf("a gateway event carries another lane's discriminator: proxy=%q otel=%q",
				ev.ProxyRequestID, ev.OtelRequestID)
		}
	})

	t.Run("proxy", func(t *testing.T) {
		ev, err := EventFor(LaneProxy, sampleIdentity(), "req-1", sampleAt, sampleCaptured())
		if err != nil {
			t.Fatalf("EventFor: %v", err)
		}
		if ev.ProxyRequestID != "req-1" {
			t.Errorf("ProxyRequestID = %q, want req-1", ev.ProxyRequestID)
		}
		if ev.GatewayRequestID != "" || ev.OtelRequestID != "" {
			t.Errorf("a proxy event carries another lane's discriminator: gateway=%q otel=%q",
				ev.GatewayRequestID, ev.OtelRequestID)
		}
	})
}

// TestAnUnsetLaneIsRefusedRatherThanDefaulted.
func TestAnUnsetLaneIsRefusedRatherThanDefaulted(t *testing.T) {
	_, err := EventFor(Lane{}, sampleIdentity(), "req-1", sampleAt, sampleCaptured())
	if err == nil {
		t.Fatal("EventFor accepted a zero Lane; an unconfigured lane must be refused, never defaulted")
	}
	if !strings.Contains(err.Error(), "lane") {
		t.Errorf("error %q does not name the lane as the problem", err)
	}
}

// TestTheLanesAreDisjoint.
func TestTheLanesAreDisjoint(t *testing.T) {
	if LaneGateway.IDPrefix == LaneProxy.IDPrefix {
		t.Errorf("both lanes mint the prefix %q; a minted id would not say which lane produced it",
			LaneGateway.IDPrefix)
	}
	if LaneGateway.Name == LaneProxy.Name {
		t.Errorf("both lanes name the namespace %q; activity_ids would collide", LaneGateway.Name)
	}
	if LaneGateway.IDPrefix != GatewayIDPrefix {
		t.Errorf("LaneGateway.IDPrefix = %q, want the shipped %q", LaneGateway.IDPrefix, GatewayIDPrefix)
	}
}

// TestLaneNamesMatchTheActivityIDNamespaces crosses the module seam.
func TestLaneNamesMatchTheActivityIDNamespaces(t *testing.T) {
	for _, tc := range []struct {
		lane Lane
		want string
	}{
		{LaneGateway, ":gateway:req-1"},
		{LaneProxy, ":proxy:req-1"},
	} {
		ev, err := EventFor(tc.lane, sampleIdentity(), "req-1", sampleAt, sampleCaptured())
		if err != nil {
			t.Fatalf("EventFor(%s): %v", tc.lane.Name, err)
		}
		body := postThroughRealClient(t, ev, true)

		var p struct {
			ActivityID string `json:"activity_id"`
			SpanCount  int    `json:"span_count"`
			Spans      []struct {
				SpanID string `json:"span_id"`
			} `json:"spans"`
		}
		if err := json.Unmarshal(body, &p); err != nil {
			t.Fatalf("unmarshal posted payload: %v", err)
		}
		if !strings.HasSuffix(p.ActivityID, tc.want) {
			t.Errorf("lane %s produced activity_id %q, want it to end in %q",
				tc.lane.Name, p.ActivityID, tc.want)
		}
		// A lane whose span went missing would file half its evidence under a row
		// the other half never joins.
		if p.SpanCount != 1 || len(p.Spans) != 1 || p.Spans[0].SpanID == "" {
			t.Errorf("lane %s posted span_count=%d spans=%d; the observed span did not reach the wire",
				tc.lane.Name, p.SpanCount, len(p.Spans))
		}
	}
}

// TestEmitRefusesAnUnconfiguredLane is the runtime half of the refusal: an
// Emitter with no Lane must drop the call loudly rather than file it under
// whichever lane happens to be first in the source.
func TestEmitRefusesAnUnconfiguredLane(t *testing.T) {
	dir := t.TempDir()
	var warned bool
	em := &Emitter{
		Spool: hookflow.Spool{Dir: dir},
		DID:   func() string { return "did:openbox:dev" },
		Warn:  func(string, ...any) { warned = true },
	}
	em.Emit(context.Background(), capturedWithSession("sess-1"))

	if !warned {
		t.Error("an Emitter with no Lane emitted silently; a governance gap nobody is told about " +
			"is indistinguishable from a working lane")
	}
	if n := spoolEntryCount(t, dir); n != 0 {
		t.Errorf("an Emitter with no Lane spooled %d event(s); it must spool none", n)
	}
}

// TestEmitFilesUnderTheConfiguredLane is the positive control for the above:
// the same emitter, with a lane set, does produce the event.
func TestEmitFilesUnderTheConfiguredLane(t *testing.T) {
	dir := t.TempDir()
	em := &Emitter{
		Lane:  LaneProxy,
		Spool: hookflow.Spool{Dir: dir},
		DID:   func() string { return "did:openbox:dev" },
		Warn:  func(string, ...any) {},
	}
	em.Emit(context.Background(), capturedWithSession("sess-1"))

	if n := spoolEntryCount(t, dir); n != 1 {
		t.Fatalf("spooled %d events, want 1", n)
	}
}

func spoolEntryCount(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatalf("read spool dir: %v", err)
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() {
			n++
		}
	}
	return n
}

// mustEvent is EventFor for tests that are not about the lane.
func mustEvent(lane Lane, id Identity, requestID string, at time.Time, c gateway.Captured) client.DevEvent {
	ev, err := EventFor(lane, id, requestID, at, c)
	if err != nil {
		panic(err)
	}
	return ev
}
