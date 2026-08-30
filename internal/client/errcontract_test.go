package client

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"
)

// TestEmit_UnbuildableEventReportsLoss a build failure used to return a nil
// error, which a durable caller reads as "delivered"; so an event that never
// left the machine was dropped from the spool as though core had accepted it.
func TestEmit_UnbuildableEventReportsLoss(t *testing.T) {
	c, log := newTestClient(t, "https://core.example", false)

	_, err := c.Emit(context.Background(), DevEvent{
		SchemaVersion: SchemaVersion,
		EventID:       "ev-nan",
		EventType:     EventSessionStarted,
		SessionID:     "sess-1",
		DeveloperDID:  "did:aip:x",
		Timestamp:     "2026-07-31T09:00:00Z",
		Metadata:      map[string]any{"bad": math.NaN()},
	})

	if !errors.Is(err, ErrUnbuildable) {
		t.Fatalf("err = %v, want ErrUnbuildable so the caller records a drop", err)
	}
	if errors.Is(err, ErrDelivery) {
		t.Error("an unbuildable event must not look retryable")
	}
	if log.all() == "" {
		t.Error("the drop should still be logged")
	}
}

// TestNew_ZeroRetriesIsExpressible zero used to mean "unset, use the default",
// so no-retries was inexpressible.
func TestNew_ZeroRetriesIsExpressible(t *testing.T) {
	zero := 0
	c, err := New(Config{
		BaseURL: "https://core.example", APIKey: testAPIKey, DID: testDID, PrivateKeyB64: testPrivateKeyB64,
		MaxRetries: &zero,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.maxRetries != 0 {
		t.Errorf("maxRetries = %d, want 0 (an explicit zero must not be read as unset)", c.maxRetries)
	}
}

func TestNew_DefaultsWhenUnset(t *testing.T) {
	c, err := New(Config{BaseURL: "https://core.example", APIKey: testAPIKey, DID: testDID, PrivateKeyB64: testPrivateKeyB64})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.maxRetries != defaultMaxRetries || c.retryBase != defaultRetryBase {
		t.Errorf("got %d/%v, want the defaults %d/%v", c.maxRetries, c.retryBase, defaultMaxRetries, defaultRetryBase)
	}
}

// TestNew_NegativeRetryConfigIsRejected a negative value used to skip the send
// loop and return a nil body with no error, which parsed as VerdictUnknown; a
// silently ungoverned client.
func TestNew_NegativeRetryConfigIsRejected(t *testing.T) {
	neg := -1
	if _, err := New(Config{
		BaseURL: "https://core.example", APIKey: testAPIKey, DID: testDID, PrivateKeyB64: testPrivateKeyB64,
		MaxRetries: &neg,
	}); err == nil {
		t.Error("a negative MaxRetries must be rejected at construction, not silently disable sending")
	}
	negd := -time.Second
	if _, err := New(Config{
		BaseURL: "https://core.example", APIKey: testAPIKey, DID: testDID, PrivateKeyB64: testPrivateKeyB64,
		RetryBase: &negd,
	}); err == nil {
		t.Error("a negative RetryBase must be rejected at construction")
	}
}
