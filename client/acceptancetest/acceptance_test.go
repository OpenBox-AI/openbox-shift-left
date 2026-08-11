// Package acceptance holds the core-acceptance contract test.
//
// HISTORY: this test began (STORY-SL-13) as proof that the EXT-core accept-list
// patch was applied — it POSTed the 7 developer-runtime event types and expected
// a 400 "invalid event_type" on stock (un-patched) core. ADR-0004 / E7 RETIRED
// that patch: the client no longer emits developer-specific event_type strings;
// it maps every dev event onto a base wire type openbox-core already accept-lists
// (Workflow*/SignalReceived/ActivityStarted/ActivityCompleted, all span-less).
// See ../../contracts/dev-event/ext-core/README.md.
//
// The test is now the INVERSE guarantee: shift-left emits ONLY stock base wire
// types, so a STOCK core accepts every event with NO 400 — no external patch
// required. That is E7-S2's retirement, made executable.
//
// It lives in its own module (not conformance/, which is deliberately
// dependency-free and offline) so it can reuse the SL-3 client's AIP signing via
// a local `replace` — no new external dependency, no change to client/ egress.
//
// The LIVE test skips cleanly when OPENBOX_URL / creds are absent, so
// `go test ./...` stays green offline. The offline case pins the emit-only-stock-
// types guarantee against a fake stock core with no un-patched core required.
package acceptancetest

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/client"
)

// The 7 normalized developer-runtime event types, emitted as one coherent session
// (shared session id). The client maps each onto a base wire type; this slice
// exercises the whole lifecycle, not just one event.
var devEventTypes = []client.EventType{
	client.EventSessionStarted,
	client.EventPromptSubmitted,
	client.EventToolCall,
	client.EventToolResult,
	client.EventTurnStarted,
	client.EventTurnCompleted,
	client.EventCommitCreated,
	client.EventDeploy,
	client.EventSessionEnded,
}

// stockWireTypes is exactly the base SDK's accept-listed set — what a STOCK core
// admits with no EXT-core patch. The client must emit only these.
var stockWireTypes = map[string]bool{
	"WorkflowStarted":   true,
	"WorkflowCompleted": true,
	"WorkflowFailed":    true,
	"SignalReceived":    true,
	"ActivityStarted":   true,
	"ActivityCompleted": true,
	"Handoff":           true,
}

// captureLogger keeps Emit's fail-open drop lines out of the test output while
// retaining them for a failure message. It is no longer the observation channel:
// probeTypes classifies on Emit's returned error, since ErrDelivery (E8-S7) says
// precisely what the log used to have to be parsed for.
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

// minimalEvent builds the smallest schema-valid DevEvent for a type: the SL-1
// contract requires `tool` on every event, `span` on ToolCall/ToolResult, and
// specific metadata on CommitCreated/Deploy (dev-event.schema.json oneOf).
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
	case client.EventCommitCreated:
		ev.Metadata = map[string]any{"commit_sha": "0000000000000000000000000000000000000000", "repo": "openbox-ai/acceptance"}
	case client.EventDeploy:
		ev.Metadata = map[string]any{"deploy_id": "accept-deploy", "commit_sha": "0000000000000000000000000000000000000000"}
	}
	return ev
}

// probeTypes emits one minimal event per type as a single coherent session and
// classifies each outcome from Emit's returned error: nil ⇒ accepted (2xx);
// ErrDelivery mentioning a 400 invalid event_type ⇒ core rejected the emitted
// wire type; any other ErrDelivery ⇒ inconclusive (a stack/creds problem to
// resolve first).
//
// Classification reads the returned error rather than scraping the drop log.
// The log is a side channel that Emit is free to reword; more importantly, since
// E8-S7 a delivery failure returns ErrDelivery, so the old code's "any error is
// a test-side build bug" Fatalf fired on exactly the rejection this probe exists
// to report.
func probeTypes(t *testing.T, ctx context.Context, c *client.Client, did string) (rejected, inconclusive []string) {
	t.Helper()
	sessionID := fmt.Sprintf("acceptance-%d", time.Now().UnixNano())

	for _, et := range devEventTypes {
		_, err := c.Emit(ctx, minimalEvent(et, did, sessionID))
		switch {
		case err == nil:
			// 2xx ⇒ this event's wire type is accept-listed ✓
		case !errors.Is(err, client.ErrDelivery):
			// A caller-precondition error (empty EventID/SessionID) is a bug in
			// this harness, not a finding about core.
			t.Fatalf("Emit(%s) returned a caller-precondition error (test-side bug): %v", et, err)
		case strings.Contains(err.Error(), "HTTP 400") && strings.Contains(err.Error(), "invalid event_type"):
			rejected = append(rejected, string(et))
		default:
			inconclusive = append(inconclusive, fmt.Sprintf("%s: %v", et, err))
		}
	}
	return rejected, inconclusive
}

// TestAcceptanceStockCoreAcceptsEmittedEvents is the env-gated LIVE probe: against
// a running STOCK core (no EXT-core patch), every emitted dev event must be
// accepted (non-400) because the client maps them onto stock base wire types.
// This is E7-S2's retirement proven end-to-end.
func TestAcceptanceStockCoreAcceptsEmittedEvents(t *testing.T) {
	baseURL := firstEnv("OPENBOX_URL", "OPENBOX_BASE_URL")
	apiKey := os.Getenv("OPENBOX_API_KEY")
	did := os.Getenv("OPENBOX_AGENT_DID")
	seed := os.Getenv("OPENBOX_ED25519_SEED")

	if baseURL == "" || apiKey == "" || did == "" || seed == "" {
		t.Skip("skipping live core-acceptance test: set OPENBOX_URL (or OPENBOX_BASE_URL), " +
			"OPENBOX_API_KEY, OPENBOX_AGENT_DID, OPENBOX_ED25519_SEED to run it against a live core")
	}

	log := &captureLogger{}
	c, err := client.New(client.Config{BaseURL: baseURL, APIKey: apiKey, DID: did, SeedB64: seed, Logger: log})
	if err != nil {
		t.Fatalf("build client: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Preflight: a signed GET /auth/validate (SL-11) isolates a creds/signing/stack
	// problem (401/500/unreachable) from the thing under test (wire-type acceptance).
	if err := c.Validate(ctx); err != nil {
		if ve, ok := client.AsValidateError(err); ok {
			t.Fatalf("preflight auth/validate failed (fix creds/core first): HTTP %d — %s", ve.Status, ve.Diagnostic)
		}
		t.Fatalf("preflight auth/validate could not reach core (fix the stack first): %v", err)
	}

	rejected, inconclusive := probeTypes(t, ctx, c, did)

	if len(rejected) > 0 {
		t.Errorf("stock core rejected %d/%d emitted events with 400 \"invalid event_type\" (%s) — "+
			"the client is emitting a NON-stock wire type; the base-wire mapping (MAPPING.md §2) is broken. "+
			"Every dev event must map to a stock base type (Workflow*/SignalReceived/ActivityStarted).",
			len(rejected), len(devEventTypes), strings.Join(rejected, ", "))
	}
	if len(inconclusive) > 0 {
		t.Errorf("%d event(s) dropped for a reason other than 400 event_type — acceptance could not be "+
			"proven; resolve these first:\n  - %s\n\nclient drop log:\n%s",
			len(inconclusive), strings.Join(inconclusive, "\n  - "), log)
	}
	if len(rejected) == 0 && len(inconclusive) == 0 {
		t.Logf("✓ all %d dev events accepted (non-400) by STOCK core at %s — EXT-core retired, base-wire mapping holds", len(devEventTypes), baseURL)
	}
}

// TestAcceptanceEmitsOnlyStockWireTypes pins the retirement offline: a fake STOCK
// core that accept-lists ONLY the base wire types (and 400s anything else) must
// see NO rejection — proving the client emits no developer-specific event_type
// (the EXT-core accept-list is genuinely unnecessary). No live core required.
func TestAcceptanceEmitsOnlyStockWireTypes(t *testing.T) {
	var seenTypes sync.Map // event_type -> struct{}: what the client actually put on the wire

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == client.AuthValidatePath:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"code":200,"message":"ok"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/governance/evaluate":
			var payload struct {
				EventType string `json:"event_type"`
			}
			_ = json.NewDecoder(r.Body).Decode(&payload)
			seenTypes.Store(payload.EventType, struct{}{})
			// Stock core: accept-list the base types exactly as
			// internal/api/governance.go's isValidGovernanceEventType does.
			if stockWireTypes[payload.EventType] {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"verdict":"allow","action":"continue"}`))
				return
			}
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(fmt.Sprintf(`{"code":400,"message":"invalid event_type: %s"}`, payload.EventType)))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	did := "did:aip:00000000-0000-0000-0000-000000000000"
	log := &captureLogger{}
	c, err := client.New(client.Config{BaseURL: srv.URL, APIKey: "obx_test_stockcore", DID: did, SeedB64: ephemeralSeedB64(t), Logger: log})
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
		t.Fatalf("stock-core fake rejected %v — the client emitted a non-stock wire type (EXT-core would still be needed). "+
			"Every dev event must map to a base type in stockWireTypes.", rejected)
	}
	if len(inconclusive) != 0 {
		t.Fatalf("unexpected inconclusive drops against the fake stock core: %v\n\nclient drop log:\n%s", inconclusive, log)
	}

	// Every wire type the client emitted must be a stock base type.
	seenTypes.Range(func(k, _ any) bool {
		et := k.(string)
		if !stockWireTypes[et] {
			t.Errorf("client emitted non-stock wire event_type %q — must be one of the base accept-listed types", et)
		}
		return true
	})
}

// ephemeralSeedB64 mints a throwaway base64 Ed25519 seed for the fake-core test
// (the fake never verifies the signature; the client just needs a valid signer).
func ephemeralSeedB64(t *testing.T) string {
	t.Helper()
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		t.Fatalf("mint ephemeral seed: %v", err)
	}
	return base64.StdEncoding.EncodeToString(seed)
}

func firstEnv(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}
