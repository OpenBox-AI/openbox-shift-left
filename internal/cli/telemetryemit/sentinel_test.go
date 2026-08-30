package telemetryemit

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/client/memhttptest"
)

func testSeed() string {
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i)
	}
	return base64.StdEncoding.EncodeToString(seed)
}

var testCoreKey = "obx_" + "test_sentinel"

// sentinels are the poisoned attribute values. That would route content around
// the content gate entirely, under key names contentMetadataKeys has never
// heard of, and nothing would error.
var sentinels = map[string]string{
	"prompt":          "SENTINEL_PROMPT",
	"tool_parameters": "SENTINEL_TOOLPARAMS",
	"tool_input":      "SENTINEL_TOOLINPUT",
	"response":        "SENTINEL_RESPONSE",
	"body_ref":        "/SENTINEL_BODYREF/path.json",
	"user.email":      "SENTINEL_EMAIL@example.com",
	"user.id":         "SENTINEL_USERID",
	"organization.id": "SENTINEL_ORGID",
	"error":           "SENTINEL_ERRORTEXT",
}

func emitThrough(t *testing.T, captureOn bool, ev client.DevEvent) string {
	t.Helper()
	var mu sync.Mutex
	var bodies []string
	srv := memhttptest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(raw))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"verdict":"allow"}`))
	}))

	c, err := client.New(client.Config{
		BaseURL:               srv.URL,
		APIKey:                testCoreKey,
		DID:                   testDID,
		PrivateKeyB64:         testSeed(),
		ContentCaptureEnabled: captureOn,
	})
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}
	if _, err := c.Emit(context.Background(), ev); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) == 0 {
		t.Fatal("nothing was POSTed")
	}
	return strings.Join(bodies, "\n")
}

// TestNoContentOnWireAtEitherPosture is this lane's sentinel. That the claim
// holds with capture ON is the load-bearing half; capture-off proving it would
// prove only that the gate works, which is client's test, not this one.
func TestNoContentOnWireAtEitherPosture(t *testing.T) {
	attrs := map[string]string{}
	for k, v := range sentinels {
		attrs[k] = v
	}
	rec := apiRequest(attrs)

	ev, out := elected().EventFor(rec)
	if out != Emitted {
		t.Fatal("no event")
	}

	for _, captureOn := range []bool{false, true} {
		name := "capture_off"
		if captureOn {
			name = "capture_on"
		}
		t.Run(name, func(t *testing.T) {
			body := emitThrough(t, captureOn, ev)
			for attr, marker := range sentinels {
				if strings.Contains(body, marker) {
					t.Errorf("attribute %q leaked to the wire (marker %q). Ranging Record.Attrs into metadata routes content around the content gate entirely.", attr, marker)
				}
			}
			var p struct {
				EventType      string `json:"event_type"`
				ActivityType   string `json:"activity_type"`
				ActivityID     string `json:"activity_id"`
				ActivityOutput struct {
					Model string `json:"model"`
					Usage struct {
						Input         *int `json:"input_tokens"`
						Output        *int `json:"output_tokens"`
						CacheRead     *int `json:"cache_read_input_tokens"`
						CacheCreation *int `json:"cache_creation_input_tokens"`
					} `json:"usage"`
				} `json:"activity_output"`
			}
			if err := json.Unmarshal([]byte(body), &p); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if p.EventType != "ActivityCompleted" {
				t.Errorf("event_type = %q; the turn did not ship, so the absences above prove nothing", p.EventType)
			}
			if p.ActivityType != "llm_completion" {
				t.Errorf("activity_type = %q, want llm_completion", p.ActivityType)
			}
			if !strings.Contains(p.ActivityID, ":otel:") {
				t.Errorf("activity_id = %q, want the :otel: namespace; without it two lanes' evidence can merge", p.ActivityID)
			}
			if p.ActivityOutput.Model == "" {
				t.Error("activity_output.model is absent; it is core's aggregation key")
			}
			if p.ActivityOutput.Usage.Input == nil || p.ActivityOutput.Usage.Output == nil ||
				p.ActivityOutput.Usage.CacheRead == nil || p.ActivityOutput.Usage.CacheCreation == nil {
				t.Errorf("activity_output.usage is incomplete: %+v", p.ActivityOutput.Usage)
			}
		})
	}
}

// TestContentFieldsAreUnsetOnTheEvent is the structural half.
func TestContentFieldsAreUnsetOnTheEvent(t *testing.T) {
	attrs := map[string]string{}
	for k, v := range sentinels {
		attrs[k] = v
	}
	ev, out := elected().EventFor(apiRequest(attrs))
	if out != Emitted {
		t.Fatal("no event")
	}
	if ev.Content != nil {
		t.Errorf("Content is populated (%+v); this slice binds no content", ev.Content)
	}
	if ev.Span != nil {
		if ev.Span.RequestBody != "" || ev.Span.ResponseBody != "" {
			t.Error("span carries a body; body attachment is deferred pending the confinement root")
		}
		if len(ev.Span.RequestHeaders) != 0 || len(ev.Span.ResponseHeaders) != 0 {
			t.Error("span carries headers; this lane observes none")
		}
	}
	if len(ev.Metadata) != 0 {
		t.Errorf("metadata is populated (%v); every content key would route around the gate", ev.Metadata)
	}
}
