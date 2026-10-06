package client

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/client/fakecore"
	"github.com/openbox-ai/openbox-shift-left/internal/client/workloadauth"
)

// newV3TestClient builds a Client in v3 (workload-identity) mode against a
// running fakecore.Server. cachePath == "" selects the in-memory cache.
func newV3TestClient(t *testing.T, baseURL, cachePath string) (*Client, *captureLogger) {
	t.Helper()
	log := &captureLogger{}
	c, err := New(Config{
		BaseURL:            baseURL,
		APIKey:             fakecore.APIKey(),
		WorkloadPrivateKey: fakecore.WorkloadPrivateKey(),
		TokenCachePath:     cachePath,
		RetryBase:          durPtr(time.Millisecond),
		Logger:             log,
	})
	if err != nil {
		t.Fatalf("New (v3): %v", err)
	}
	return c, log
}

// TestEmitSendsWorkloadEnvelopeOnV3 pins the exact header set a v3 evaluate
// request carries: the obx_ API key, the exchanged workload bearer, the SDK
// identity and the idempotency key -- and NONE of v1's DID/signature/body-hash
// headers, since a v3 request carries no attribution coordinate at all: it is
// derived in memory and never sent.
func TestEmitSendsWorkloadEnvelopeOnV3(t *testing.T) {
	fc := fakecore.New(t, fakecore.Script{})
	c, _ := newV3TestClient(t, fc.URL(), "")

	if _, err := c.Emit(context.Background(), sampleEvent()); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	inbox := fc.Inbox()
	if len(inbox) != 1 {
		t.Fatalf("inbox length = %d, want 1", len(inbox))
	}
	h := inbox[0].Headers

	for _, want := range []string{
		"Accept", "Content-Type", "Authorization",
		"X-Openbox-Workload-Token", "X-Openbox-Sdk-Version", "User-Agent", "Idempotency-Key",
	} {
		if h.Get(want) == "" {
			t.Errorf("missing header %q; got header set %v", want, h)
		}
	}
	if got := h.Get("Authorization"); got != "Bearer "+fakecore.APIKey() {
		t.Errorf("Authorization = %q, want the obx_ API key, not the workload token", got)
	}

	for _, forbidden := range []string{
		"X-Openbox-Agent-Did", "X-Openbox-Agent-Timestamp",
		"X-Openbox-Agent-Nonce", "X-Openbox-Agent-Signature", "X-Openbox-Body-Sha256",
	} {
		if v := h.Get(forbidden); v != "" {
			t.Errorf("v3 request must carry no %q header, got %q", forbidden, v)
		}
	}

	for name, values := range h {
		if strings.Contains(name, "did:aip:") {
			t.Errorf("header name %q contains did:aip:", name)
		}
		for _, v := range values {
			if strings.Contains(v, "did:aip:") {
				t.Errorf("header %q value %q contains did:aip:", name, v)
			}
		}
	}
}

// TestWarmEmitMakesNoAuthCalls proves a second Emit against a live token
// costs zero extra bootstrap/exchange calls.
func TestWarmEmitMakesNoAuthCalls(t *testing.T) {
	fc := fakecore.New(t, fakecore.Script{})
	c, _ := newV3TestClient(t, fc.URL(), "")
	ctx := context.Background()

	if _, err := c.Emit(ctx, sampleEvent()); err != nil {
		t.Fatalf("first Emit: %v", err)
	}
	ev2 := sampleEvent()
	ev2.EventID = "evt-2"
	if _, err := c.Emit(ctx, ev2); err != nil {
		t.Fatalf("second (warm) Emit: %v", err)
	}

	if got := fc.BootstrapHits(); got != 1 {
		t.Errorf("BootstrapHits = %d, want 1 (the warm call must not re-bootstrap)", got)
	}
	if got := fc.ExchangeHits(); got != 1 {
		t.Errorf("ExchangeHits = %d, want 1", got)
	}
	if got := len(fc.Inbox()); got != 2 {
		t.Errorf("inbox length = %d, want 2", got)
	}
}

// TestColdEmitBootstrapsAndExchangesOnce is TestWarmEmitMakesNoAuthCalls'
// single-call counterpart: exactly one bootstrap and one exchange for the
// first (cold) call.
func TestColdEmitBootstrapsAndExchangesOnce(t *testing.T) {
	fc := fakecore.New(t, fakecore.Script{})
	c, _ := newV3TestClient(t, fc.URL(), "")

	if _, err := c.Emit(context.Background(), sampleEvent()); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if got := fc.BootstrapHits(); got != 1 {
		t.Errorf("BootstrapHits = %d, want 1", got)
	}
	if got := fc.ExchangeHits(); got != 1 {
		t.Errorf("ExchangeHits = %d, want 1", got)
	}
}

// TestCached401InvalidatesThenColdRetry: a cached token that a runtime route
// now rejects gets exactly one further cold retry before Emit gives up -- the
// owner decision that replaced "401 is never resent" (3 parallel hooks each
// fetching a fresh token once saw core answer a spurious 401 on the first,
// then 200 twice more moments later with an identical identity). While the
// identity stays revoked, both the original attempt (cached token) and its
// cold retry (a freshly bootstrapped+exchanged token) fail, so one Emit call
// now costs two evaluate attempts and one more bootstrap+exchange pair; once
// the identity is unrevoked, the token the failed cold retry just cached is
// still good, so the next Emit succeeds without bootstrapping again.
func TestCached401InvalidatesThenColdRetry(t *testing.T) {
	fc := fakecore.New(t, fakecore.Script{})
	cachePath := filepath.Join(t.TempDir(), "workload-token.json")
	c, _ := newV3TestClient(t, fc.URL(), cachePath)
	ctx := context.Background()

	if _, err := c.Emit(ctx, sampleEvent()); err != nil {
		t.Fatalf("warming Emit: %v", err)
	}
	if _, err := os.Stat(cachePath); err != nil {
		t.Fatalf("cache file missing after a successful cold Emit: %v", err)
	}

	fc.Revoke()
	ev2 := sampleEvent()
	ev2.EventID = "evt-2"
	_, err := c.Emit(ctx, ev2)
	if err == nil {
		t.Fatal("Emit succeeded despite a revoked identity")
	}
	if !errors.Is(err, ErrDelivery) {
		t.Errorf("err = %v, want ErrDelivery", err)
	}
	if errors.Is(err, ErrRefused) {
		t.Error("a 401 held after its one cold retry must still not be a refusal (it would spend the spool's one attempt on a datastore hiccup indistinguishable from a real rejection)")
	}
	if got := fc.V3EvaluateAttempts(); got != 3 {
		t.Fatalf("evaluate attempts = %d, want 3 (1 for the warming Emit, 2 for evt-2: the cached-token 401 earns exactly one cold retry)", got)
	}
	if got := fc.BootstrapHits(); got != 2 {
		t.Errorf("BootstrapHits = %d, want 2 (1 warming + 1 for evt-2's cold retry)", got)
	}
	if got := fc.ExchangeHits(); got != 2 {
		t.Errorf("ExchangeHits = %d, want 2", got)
	}

	// The cold retry's own freshly exchanged token is still cached even though
	// it too was rejected; unrevoking makes that same token good, so the next
	// Emit succeeds without another bootstrap+exchange round trip.
	fc.Unrevoke()
	ev3 := sampleEvent()
	ev3.EventID = "evt-3"
	if _, err := c.Emit(ctx, ev3); err != nil {
		t.Fatalf("Emit after Unrevoke: %v", err)
	}
	if got := fc.BootstrapHits(); got != 2 {
		t.Errorf("BootstrapHits = %d, want 2 (Emit after Unrevoke reuses the cached token)", got)
	}
	if got := fc.ExchangeHits(); got != 2 {
		t.Errorf("ExchangeHits = %d, want 2", got)
	}
}

// TestFresh401StillGetsOneColdRetryThenHeldNotRefused covers the
// fromCache=false half of the same rule: a just-acquired token a runtime
// route rejects still earns the one cold retry; when the identity stays
// revoked throughout, the failure is held (ErrDelivery), never refused
// (ErrRefused), and costs exactly two evaluate attempts.
func TestFresh401StillGetsOneColdRetryThenHeldNotRefused(t *testing.T) {
	fc := fakecore.New(t, fakecore.Script{})
	fc.Revoke()
	c, _ := newV3TestClient(t, fc.URL(), "")

	_, err := c.Emit(context.Background(), sampleEvent())
	if err == nil {
		t.Fatal("Emit succeeded despite a revoked identity")
	}
	if !errors.Is(err, ErrDelivery) {
		t.Errorf("err = %v, want ErrDelivery", err)
	}
	if errors.Is(err, ErrRefused) {
		t.Error("a 401 held after its one cold retry must still not be a refusal")
	}
	if got := fc.V3EvaluateAttempts(); got != 2 {
		t.Errorf("evaluate attempts = %d, want 2 (the one cold retry, still rejected)", got)
	}
}

// TestTokenFailureIsDeliveryNotRefusal: a bootstrap or exchange rejection is
// never a judgment on the event itself, so it must never look like ErrRefused
// -- only ErrDelivery.
func TestTokenFailureIsDeliveryNotRefusal(t *testing.T) {
	cases := []struct {
		name    string
		setup   func(*fakecore.Server)
		wantSub string
	}{
		{"bootstrap 403", func(fc *fakecore.Server) { fc.SetBootstrapFailure(http.StatusForbidden, "agent_inactive") }, "agent_inactive"},
		{"bootstrap 409", func(fc *fakecore.Server) {
			fc.SetBootstrapFailure(http.StatusConflict, "workload_identity_unavailable")
		}, "workload_identity_unavailable"},
		{"exchange 400", func(fc *fakecore.Server) { fc.SetExchangeFailure(http.StatusBadRequest, "invalid_client") }, "exchange"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fc := fakecore.New(t, fakecore.Script{})
			tc.setup(fc)
			c, _ := newV3TestClient(t, fc.URL(), "")

			_, err := c.Emit(context.Background(), sampleEvent())
			if err == nil {
				t.Fatal("Emit succeeded despite a token-acquisition failure")
			}
			if !errors.Is(err, ErrDelivery) {
				t.Errorf("err = %v, want ErrDelivery", err)
			}
			if errors.Is(err, ErrRefused) {
				t.Error("a token-acquisition failure must never be a refusal")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("err = %q, want it to contain %q", err.Error(), tc.wantSub)
			}
		})
	}
}

// TestTransientTokenFailureRetriesWithinBudget: an unreachable token endpoint
// is a transient acquisition failure, retried within the configured budget --
// and never mistaken for a refusal.
func TestTransientTokenFailureRetriesWithinBudget(t *testing.T) {
	fc := fakecore.New(t, fakecore.Script{})
	fc.TokenEndpointDown()

	maxRetries := 2
	c, err := New(Config{
		BaseURL:            fc.URL(),
		APIKey:             fakecore.APIKey(),
		WorkloadPrivateKey: fakecore.WorkloadPrivateKey(),
		RetryBase:          durPtr(time.Millisecond),
		MaxRetries:         intPtr(maxRetries),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, emitErr := c.Emit(context.Background(), sampleEvent())
	if emitErr == nil {
		t.Fatal("Emit succeeded despite an unreachable token endpoint")
	}
	if !errors.Is(emitErr, ErrDelivery) {
		t.Errorf("err = %v, want ErrDelivery", emitErr)
	}
	if errors.Is(emitErr, ErrRefused) {
		t.Error("a transient acquisition failure must never be a refusal")
	}
	if got := fc.BootstrapHits(); got != maxRetries+1 {
		t.Errorf("BootstrapHits = %d, want %d (one cold attempt per retry)", got, maxRetries+1)
	}
	if got := fc.ExchangeHits(); got != 0 {
		t.Errorf("ExchangeHits = %d, want 0 (the token endpoint is unreachable; the exchange handler is never reached)", got)
	}
}

// TestPollApprovalUsesV3 proves the approval poll carries the same v3
// envelope Emit does, not the v1 signature.
func TestPollApprovalUsesV3(t *testing.T) {
	fc := fakecore.New(t, fakecore.Script{})
	var gotHeaders http.Header
	fc.Approval(func(r fakecore.Received) (int, string) {
		gotHeaders = r.Headers
		return http.StatusOK, `{"id":"ge-1","action":"allow","approval_expiration_time":"2026-01-01T00:00:00Z"}`
	})
	c, _ := newV3TestClient(t, fc.URL(), "")

	status, err := c.PollApproval(context.Background(), ApprovalKey{WorkflowID: "wf", RunID: "run", ActivityID: "act"})
	if err != nil {
		t.Fatalf("PollApproval: %v", err)
	}
	if status.EventID != "ge-1" {
		t.Errorf("EventID = %q, want ge-1", status.EventID)
	}
	if fc.ApprovalPolls() != 1 {
		t.Errorf("ApprovalPolls = %d, want 1", fc.ApprovalPolls())
	}
	if gotHeaders.Get("X-Openbox-Workload-Token") == "" {
		t.Error("approval poll did not carry the workload token header; the v1 path was used instead of v3")
	}
	if gotHeaders.Get("X-Openbox-Agent-Did") != "" {
		t.Error("approval poll must not carry a v1 DID header in v3 mode")
	}
}

// TestValidateUsesV3AndNamesTheStage covers both ValidateDetailed's success
// shape and its acquisition-failure shape, plus Validate's continued
// compatibility.
func TestValidateUsesV3AndNamesTheStage(t *testing.T) {
	t.Run("success returns the parsed result", func(t *testing.T) {
		fc := fakecore.New(t, fakecore.Script{})
		c, _ := newV3TestClient(t, fc.URL(), "")

		res, err := c.ValidateDetailed(context.Background())
		if err != nil {
			t.Fatalf("ValidateDetailed: %v", err)
		}
		if !res.Valid || !res.Active {
			t.Errorf("res = %+v, want Valid and Active", res)
		}
		if res.AgentID != fakecore.AgentID() {
			t.Errorf("AgentID = %q, want %q", res.AgentID, fakecore.AgentID())
		}
		if res.AgentName == "" {
			t.Error("AgentName is empty")
		}
	})

	t.Run("acquisition failure names the stage", func(t *testing.T) {
		fc := fakecore.New(t, fakecore.Script{})
		fc.SetBootstrapFailure(http.StatusForbidden, "agent_inactive")
		c, _ := newV3TestClient(t, fc.URL(), "")

		_, err := c.ValidateDetailed(context.Background())
		if err == nil {
			t.Fatal("ValidateDetailed succeeded despite a bootstrap failure")
		}
		var werr *workloadauth.Error
		if !errors.As(err, &werr) {
			t.Fatalf("err does not wrap *workloadauth.Error: %v", err)
		}
		if werr.Stage != workloadauth.StageBootstrap {
			t.Errorf("Stage = %q, want %q", werr.Stage, workloadauth.StageBootstrap)
		}
		if !strings.Contains(err.Error(), string(workloadauth.StageBootstrap)) {
			t.Errorf("err = %q, does not name the stage", err.Error())
		}
	})

	t.Run("Validate stays source-compatible", func(t *testing.T) {
		fc := fakecore.New(t, fakecore.Script{})
		c, _ := newV3TestClient(t, fc.URL(), "")
		if err := c.Validate(context.Background()); err != nil {
			t.Fatalf("Validate: %v", err)
		}
	})
}
