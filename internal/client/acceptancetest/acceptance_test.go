// Package acceptance holds the core-acceptance contract test: every dev event
// maps onto a stock core wire type, so a stock core accepts all of them. It
// runs against the workload-identity client.
package acceptancetest

import (
	"context"
	"errors"
	"fmt"

	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/client/fakecore"
)

// devEventTypes deliberately orders the v1.8 vocabulary (35 entries: the
// original 12, the model-call gate pair, and 21 observe-only lifecycle signals) into one coherent
// session rather than repeating client.AllEventTypes' declaration order:
// Setup/InstructionsLoaded before SessionStarted, Elicitation mid-session,
// PostCompact after PreCompact, SessionEnded always last (/clear is a
// continue-as-new, never a mid-session terminal class).
var devEventTypes = []client.EventType{
	client.EventSetup,
	client.EventInstructionsLoaded,
	client.EventSessionStarted,
	client.EventPromptSubmitted,
	client.EventUserPromptExpansion,
	client.EventSubagentStarted,
	client.EventPermissionRequest,
	client.EventToolCall,
	client.EventPermissionDenied,
	client.EventToolResult,
	client.EventPostToolBatch,
	client.EventMessageDisplay,
	client.EventNotification,
	client.EventTaskCreated,
	client.EventTaskCompleted,
	client.EventTeammateIdle,
	client.EventConfigChange,
	client.EventCwdChanged,
	client.EventDirectoryAdded,
	client.EventFileChanged,
	client.EventWorktreeRemove,
	client.EventPreModelSwitch,
	client.EventPostModelSwitch,
	client.EventTurnStarted,
	client.EventTurnCompleted,
	client.EventModelCallRequested,
	client.EventModelCallFinished,
	client.EventAPIError,
	client.EventElicitation,
	client.EventElicitationResult,
	client.EventPreCompact,
	client.EventPostCompact,
	client.EventCommitCreated,
	client.EventDeploy,
	client.EventSessionEnded,
}

// stockWireTypes is exactly the base SDK's accept-listed set; what a stock
// core admits with no accept-list patch. The client must emit only these.
var stockWireTypes = map[string]bool{
	"WorkflowStarted":   true,
	"WorkflowCompleted": true,
	"WorkflowFailed":    true,
	"SignalReceived":    true,
	"ActivityStarted":   true,
	"ActivityCompleted": true,
	"Handoff":           true,
}

type captureLogger struct {
	mu    sync.Mutex
	lines []string
}

func (l *captureLogger) Printf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

// String renders the captured drop lines for a failure diagnostic.
func (l *captureLogger) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

func minimalEvent(et client.EventType, did, sessionID string) client.DevEvent {
	now := time.Now().UTC().Format(time.RFC3339)
	ev := client.DevEvent{
		SchemaVersion: client.SchemaVersion,
		EventID:       sessionID + "-" + string(et),
		EventType:     et,
		SessionID:     sessionID,
		DeveloperDID:  did,
		Timestamp:     now,
		Tool:          client.Tool{Name: "acceptance", Kind: client.ToolShell},
	}
	switch et {
	case client.EventSessionStarted:
		ev.StartedAt = now
	case client.EventSessionEnded:
		ev.EndedAt = now
	case client.EventToolCall:
		ev.Span = &client.Span{SemanticType: "internal", Stage: "started"}
	case client.EventToolResult:
		ev.Span = &client.Span{SemanticType: "internal", Stage: "completed"}
	case client.EventTurnStarted, client.EventTurnCompleted:
		// Both map onto Activity* wire types (payload.go), which require a
		// non-empty activity_id; turnActivityIDFor derives one from TurnIndex.
		turnIndex := 0
		ev.TurnIndex = &turnIndex
	case client.EventModelCallRequested, client.EventModelCallFinished:
		// Activity* wire types again: the gate's id is its activity_id.
		ev.ModelCallRequestID = "acceptance-req.1"
	case client.EventCommitCreated:
		ev.Metadata = map[string]any{"commit_sha": "0000000000000000000000000000000000000000", "repo": "openbox-ai/acceptance"}
	case client.EventDeploy:
		ev.Metadata = map[string]any{"deploy_id": "accept-deploy", "commit_sha": "0000000000000000000000000000000000000000"}
	}
	return ev
}

func probeTypes(t *testing.T, ctx context.Context, c *client.Client, did string) (rejected, inconclusive []string) {
	t.Helper()
	sessionID := fmt.Sprintf("acceptance-%d", time.Now().UnixNano())

	for _, et := range devEventTypes {
		_, err := c.Emit(ctx, minimalEvent(et, did, sessionID))
		switch {
		case err == nil:
		case !errors.Is(err, client.ErrDelivery):
			t.Fatalf("Emit(%s) returned a caller-precondition error (test-side bug): %v", et, err)
		case strings.Contains(err.Error(), "HTTP 400") && strings.Contains(err.Error(), "invalid event_type"):
			rejected = append(rejected, string(et))
		default:
			inconclusive = append(inconclusive, fmt.Sprintf("%s: %v", et, err))
		}
	}
	return rejected, inconclusive
}

// TestAcceptanceStockCoreAcceptsEmittedEvents is the env-gated live probe:
// against a running stock core (no accept-list patch), every emitted dev event
// must be accepted (non-400) because the client maps them onto stock base wire
// types.
func TestAcceptanceStockCoreAcceptsEmittedEvents(t *testing.T) {
	baseURL := firstEnv("OPENBOX_URL", "OPENBOX_BASE_URL")
	apiKey := os.Getenv("OPENBOX_API_KEY")
	agentID := os.Getenv("OPENBOX_AGENT_ID")
	workloadKey := os.Getenv("OPENBOX_WORKLOAD_PRIVATE_KEY")

	if baseURL == "" || apiKey == "" || agentID == "" || workloadKey == "" {
		t.Skip("skipping live core-acceptance test: set OPENBOX_URL (or OPENBOX_BASE_URL), " +
			"OPENBOX_API_KEY, OPENBOX_AGENT_ID, OPENBOX_WORKLOAD_PRIVATE_KEY to run it against a live core")
	}

	did, err := devconfig.AttributionDIDFor(agentID)
	if err != nil {
		t.Fatalf("derive attribution DID from OPENBOX_AGENT_ID: %v", err)
	}

	log := &captureLogger{}
	c, err := client.New(client.Config{BaseURL: baseURL, APIKey: apiKey, WorkloadPrivateKey: workloadKey, Logger: log})
	if err != nil {
		t.Fatalf("build client: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := c.Validate(ctx); err != nil {
		if ve, ok := client.AsValidateError(err); ok {
			t.Fatalf("preflight auth/validate failed (fix creds/core first): HTTP %d; %s", ve.Status, ve.Diagnostic)
		}
		t.Fatalf("preflight auth/validate could not reach core (fix the stack first): %v", err)
	}

	rejected, inconclusive := probeTypes(t, ctx, c, did)

	if len(rejected) > 0 {
		t.Errorf("stock core rejected %d/%d emitted events with 400 \"invalid event_type\" (%s); "+
			"the client is emitting a NON-stock wire type; the base-wire mapping (docs/mapping.md) is broken. "+
			"Every dev event must map to a stock base type (Workflow*/SignalReceived/ActivityStarted).",
			len(rejected), len(devEventTypes), strings.Join(rejected, ", "))
	}
	if len(inconclusive) > 0 {
		t.Errorf("%d event(s) dropped for a reason other than 400 event_type; acceptance could not be "+
			"proven; resolve these first:\n  - %s\n\nclient drop log:\n%s",
			len(inconclusive), strings.Join(inconclusive, "\n  - "), log)
	}
	if len(rejected) == 0 && len(inconclusive) == 0 {
		t.Logf("✓ all %d dev events accepted (non-400) by STOCK core at %s; base-wire mapping holds", len(devEventTypes), baseURL)
	}
}

// TestAcceptanceEmitsOnlyStockWireTypes pins the same property offline: every
// event the client actually put on the wire, against a real fakecore, must be
// one of the base wire types (proving the client emits no developer-specific
// event_type, so no core accept-list patch is needed). The fake
// accepts everything here -- the assertion is what it recorded, not what it
// refused.
func TestAcceptanceEmitsOnlyStockWireTypes(t *testing.T) {
	fc := fakecore.New(t, fakecore.Script{})
	did := fakecore.AttributionDID()
	log := &captureLogger{}
	c, err := client.New(client.Config{
		BaseURL:            fc.URL(),
		APIKey:             fakecore.APIKey(),
		WorkloadPrivateKey: fakecore.WorkloadPrivateKey(),
		Logger:             log,
	})
	if err != nil {
		t.Fatalf("build client: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := c.Validate(ctx); err != nil {
		t.Fatalf("fake-core preflight should succeed: %v", err)
	}

	rejected, inconclusive := probeTypes(t, ctx, c, did)

	if len(rejected) != 0 {
		t.Fatalf("fakecore rejected %v with a 400 invalid event_type; the client emitted a "+
			"non-stock wire type (core would need an accept-list patch). Every dev event must map to a "+
			"base type in stockWireTypes.", rejected)
	}
	if len(inconclusive) != 0 {
		t.Fatalf("unexpected inconclusive drops against fakecore: %v\n\nclient drop log:\n%s", inconclusive, log)
	}

	seenTypes := map[string]struct{}{} // what the client actually put on the wire
	for _, r := range fc.Inbox() {
		seenTypes[r.EventType()] = struct{}{}
	}
	for et := range seenTypes {
		if !stockWireTypes[et] {
			t.Errorf("client emitted non-stock wire event_type %q; must be one of the base accept-listed types", et)
		}
	}
}

func firstEnv(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}
