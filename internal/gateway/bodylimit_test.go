package gateway

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestOverCapRequestIsRefusedNotTruncated is the control on the body bound.
func TestOverCapRequestIsRefusedNotTruncated(t *testing.T) {
	var got recorded
	var reached bool
	upstream := upstreamRecorder(t, &got, func(w http.ResponseWriter) {
		reached = true
		w.WriteHeader(http.StatusOK)
	})

	g, err := New(Config{Upstream: upstream.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	g.maxBody = 128
	gw := serveGateway(t, g)

	oversized := strings.Repeat("x", 4096)
	req, _ := http.NewRequest(http.MethodPost, gw.URL+"/v1/messages", strings.NewReader(oversized))
	resp, err := probeClient().Do(req)
	if err != nil {
		t.Fatalf("request through gateway: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("status: got %d want 413", resp.StatusCode)
	}
	if !strings.Contains(string(body), "openbox_gateway_error") {
		t.Errorf("refusal is not attributable to the gateway: %q", body)
	}
	if reached {
		t.Error("the upstream was contacted for an over-cap request; a partial body may have been forwarded")
	}
	if len(got.body) != 0 {
		t.Errorf("upstream received %d bytes of a refused request: the relay truncated instead of refusing", len(got.body))
	}
}

// TestAtCapRequestForwardsWhole is the other side of the bound: a body right
// up to the limit relays intact. Without this, a cap that was off by one -- or
// that silently dropped the last chunk -- would look correct.
func TestAtCapRequestForwardsWhole(t *testing.T) {
	var got recorded
	upstream := upstreamRecorder(t, &got, nil)

	g, err := New(Config{Upstream: upstream.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	g.maxBody = 4096
	gw := serveGateway(t, g)

	exact := strings.Repeat("y", 4096)
	req, _ := http.NewRequest(http.MethodPost, gw.URL+"/v1/messages", strings.NewReader(exact))
	resp, err := probeClient().Do(req)
	if err != nil {
		t.Fatalf("request through gateway: %v", err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status: got %d want 200 for a body exactly at the cap", resp.StatusCode)
	}
	if string(got.body) != exact {
		t.Errorf("body at the cap was not forwarded whole: got %d bytes want %d", len(got.body), len(exact))
	}
}

// TestProductionCapMatchesTheRepoConvention pins the constant itself.
func TestProductionCapMatchesTheRepoConvention(t *testing.T) {
	const want = 64 << 20
	if maxRequestBody != want {
		t.Errorf("maxRequestBody = %d, want %d (the largest existing cap in this repo)", maxRequestBody, want)
	}
	g, err := New(Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if g.maxBody != want {
		t.Errorf("New left maxBody = %d, want %d", g.maxBody, want)
	}
}
