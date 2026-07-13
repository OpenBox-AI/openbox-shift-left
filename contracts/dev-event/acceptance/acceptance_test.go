// Package acceptance holds the STORY-SL-13 core-acceptance contract test.
//
// It proves the EXT-core dependency (contracts/dev-event/ext-core/) is actually
// satisfied instead of assumed: given a running core + dev creds, it POSTs a
// minimal valid event of EACH of the 7 developer-runtime types to
// /api/v1/governance/evaluate and asserts a non-400 (accepted) outcome. Against
// stock core (patch NOT applied) every type is rejected 400 "invalid event_type"
// and the failure names the fix ("apply contracts/dev-event/ext-core/").
//
// It lives in its own module (not conformance/, which is deliberately
// dependency-free and offline) so it can reuse the SL-3 client's AIP signing via
// a local `replace` — no new external dependency, no change to client/ egress.
//
// The LIVE test skips cleanly when OPENBOX_URL / creds are absent, so
// `go test ./...` stays green offline and never blocks unit CI. The negative
// case (stock core → 400 → "apply ext-core") is pinned as a self-contained
// httptest so it runs offline every time, without needing an un-patched core.
package acceptance

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
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

// the 7 developer-runtime event types that EXT-core must accept-list, in a
// COHERENT LIFECYCLE order: SessionStarted first (the patch's site C maps it to
// create the session), the in-session events next, SessionEnded last (mapped to
// terminal). Emitting them as one real session — sharing a single session id —
// exercises not just the 400 accept-list gate but the lifecycle mapping too, and
// avoids the orphan-terminal 500 a bare out-of-session SessionEnded would hit.
// The set (order-independent) is enforced == the SL-1 enum by the conformance
// drift guard, so this slice only needs one minimal valid event per type.
var devEventTypes = []client.EventType{
	client.EventSessionStarted,
	client.EventPromptSubmitted,
	client.EventToolCall,
	client.EventToolResult,
	client.EventCommitCreated,
	client.EventDeploy,
	client.EventSessionEnded,
}

// captureLogger records every fail-open drop line Emit writes. Emit logs ONLY on
// a drop (a non-2xx or transport failure); a clean 2xx logs nothing. That is our
// observation channel: Emit is fail-open and swallows the HTTP status, so the
// SL-10 drop diagnostic is how we tell "accepted" from "400 invalid event_type".
type captureLogger struct {
	mu    sync.Mutex
	lines []string
}

func (l *captureLogger) Printf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *captureLogger) drain() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := l.lines
	l.lines = nil
	return out
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
		Tool:          client.Tool{Name: "sl13-acceptance", Kind: client.ToolShell},
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
		ev.Metadata = map[string]any{"commit_sha": "0000000000000000000000000000000000000000", "repo": "openbox-ai/sl13-acceptance"}
	case client.EventDeploy:
		ev.Metadata = map[string]any{"deploy_id": "sl13-accept-deploy", "commit_sha": "0000000000000000000000000000000000000000"}
	}
	return ev
}

// probeTypes emits one minimal event per type as a single coherent session and
// classifies each outcome from the fail-open drop log: no drop ⇒ accepted (2xx);
// "HTTP 400 … invalid event_type" ⇒ EXT-core not applied; any other drop ⇒
// inconclusive (a stack/creds problem to resolve before acceptance can be proven).
func probeTypes(t *testing.T, ctx context.Context, c *client.Client, log *captureLogger, did string) (rejected, inconclusive []string) {
	t.Helper()
	// One shared session id for the whole lifecycle, unique per run so re-runs
	// never collide with a prior session row.
	sessionID := fmt.Sprintf("sl13-acceptance-%d", time.Now().UnixNano())

	for _, et := range devEventTypes {
		log.drain() // isolate this event's drop lines
		ev := minimalEvent(et, did, sessionID)

		if _, err := c.Emit(ctx, ev); err != nil {
			// Emit only returns an error for an un-buildable event (a test bug), never
			// a transport failure (fail-open). Surface it — it means the minimal event
			// is malformed, not that core rejected it.
			t.Fatalf("Emit(%s) returned a build error (test-side): %v", et, err)
		}

		drops := log.drain()
		if len(drops) == 0 {
			continue // no drop logged ⇒ 2xx accepted ⇒ this type is accept-listed ✓
		}
		line := strings.Join(drops, " | ")
		switch {
		case strings.Contains(line, "HTTP 400") && strings.Contains(line, "invalid event_type"):
			rejected = append(rejected, string(et))
		default:
			inconclusive = append(inconclusive, fmt.Sprintf("%s: %s", et, line))
		}
	}
	return rejected, inconclusive
}

// missingExtCoreMessage is the actionable failure a stock core produces: it names
// the artifact to apply and cross-refs the SL-10 reason map, so a 400 reads as a
// task, not a mystery.
func missingExtCoreMessage(rejected []string) string {
	return fmt.Sprintf("core rejected %d/%d developer event types with 400 \"invalid event_type\" (%s) — "+
		"the EXT-core dependency is NOT satisfied. Apply contracts/dev-event/ext-core/ "+
		"(./apply.sh /path/to/openbox-core) and rebuild core; see the SL-10 reason map "+
		"(client/signingerr.go: \"core has not accept-listed the dev event types yet … EXT-core\").",
		len(rejected), len(devEventTypes), strings.Join(rejected, ", "))
}

// TestAcceptanceCoreAcceptsDevEventTypes is the env-gated LIVE probe.
func TestAcceptanceCoreAcceptsDevEventTypes(t *testing.T) {
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

	// Preflight: a signed GET /auth/validate (SL-11). This isolates a
	// creds/signing/stack problem (401/500/unreachable) from the thing we are
	// actually testing (event_type accept-listing → 400). If auth itself is
	// broken, every /evaluate POST would 401 and the probe below could not prove
	// anything — so fail loudly here with the mapped reason instead.
	if err := c.Validate(ctx); err != nil {
		if ve, ok := client.AsValidateError(err); ok {
			t.Fatalf("preflight auth/validate failed (fix creds/core BEFORE this can prove event-type acceptance): HTTP %d — %s", ve.Status, ve.Diagnostic)
		}
		t.Fatalf("preflight auth/validate could not reach core (fix the stack first): %v", err)
	}

	rejected, inconclusive := probeTypes(t, ctx, c, log, did)

	if len(rejected) > 0 {
		t.Error(missingExtCoreMessage(rejected))
	}
	if len(inconclusive) > 0 {
		t.Errorf("%d event type(s) dropped for a reason other than 400 event_type — "+
			"acceptance could not be proven; resolve these first:\n  - %s",
			len(inconclusive), strings.Join(inconclusive, "\n  - "))
	}
	if len(rejected) == 0 && len(inconclusive) == 0 {
		t.Logf("✓ all %d developer event types accepted (non-400) by core at %s — EXT-core dependency satisfied", len(devEventTypes), baseURL)
	}
}

// TestAcceptanceReportsMissingExtCore pins the negative case offline: a fake
// stock core that rejects every dev event_type with 400 "invalid event_type"
// must make the probe report all 7 as rejected AND produce the actionable
// "apply contracts/dev-event/ext-core/" message. No un-patched core required.
func TestAcceptanceReportsMissingExtCore(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == client.AuthValidatePath:
			// Preflight succeeds — auth/signing is fine; only the accept-list is missing.
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"code":200,"message":"ok"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/governance/evaluate":
			// Stock core: reject the un-accept-listed dev type exactly as
			// internal/api/governance.go does (Abort(c, 400, "invalid event_type: X")).
			var payload struct {
				EventType string `json:"event_type"`
			}
			_ = json.NewDecoder(r.Body).Decode(&payload)
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

	// Preflight must pass against the fake (auth OK) — proving the probe below
	// attributes the failure to the accept-list, not to signing.
	if err := c.Validate(ctx); err != nil {
		t.Fatalf("fake-core preflight should succeed: %v", err)
	}

	rejected, inconclusive := probeTypes(t, ctx, c, log, did)

	if len(rejected) != len(devEventTypes) {
		t.Fatalf("expected all %d types reported rejected on stock core, got %d %v (inconclusive: %v)",
			len(devEventTypes), len(rejected), rejected, inconclusive)
	}
	if len(inconclusive) != 0 {
		t.Fatalf("expected no inconclusive drops against the fake stock core, got %v", inconclusive)
	}

	msg := missingExtCoreMessage(rejected)
	for _, want := range []string{"contracts/dev-event/ext-core/", "apply.sh", "invalid event_type", "SL-10", "EXT-core"} {
		if !strings.Contains(msg, want) {
			t.Errorf("failure message must name %q so a 400 is actionable; got:\n%s", want, msg)
		}
	}
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
