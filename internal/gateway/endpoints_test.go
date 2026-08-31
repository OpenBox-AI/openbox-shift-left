package gateway

import (
	"io"
	"net/http"

	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/client/memhttptest"
)

func sseUpstream(t *testing.T, write func(w http.ResponseWriter, ctl *http.ResponseController)) *memhttptest.Server {
	t.Helper()
	srv := memhttptest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		write(w, http.NewResponseController(w))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestNamedEndpointsServed covers the four routes Claude Code depends on.
func TestNamedEndpointsServed(t *testing.T) {
	cases := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{"messages", http.MethodPost, "/v1/messages", `{"model":"claude-opus-4"}`},
		{"count_tokens", http.MethodPost, "/v1/messages/count_tokens", `{"model":"claude-opus-4"}`},
		{"models", http.MethodGet, "/v1/models", ""},
		{"hello", http.MethodHead, "/api/hello", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got recorded
			upstream := upstreamRecorder(t, &got, nil)
			gw := newTestGateway(t, upstream.URL)

			var bodyReader io.Reader
			if tc.body != "" {
				bodyReader = strings.NewReader(tc.body)
			}
			req, err := http.NewRequest(tc.method, gw.URL+tc.path, bodyReader)
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			resp, err := probeClient().Do(req)
			if err != nil {
				t.Fatalf("request through gateway: %v", err)
			}
			defer resp.Body.Close()
			io.Copy(io.Discard, resp.Body)

			if resp.StatusCode != http.StatusOK {
				t.Errorf("status: got %d want 200", resp.StatusCode)
			}
			if got.method != tc.method {
				t.Errorf("method: got %q want %q", got.method, tc.method)
			}
			if got.target != tc.path {
				t.Errorf("path: got %q want %q", got.target, tc.path)
			}
			if tc.body != "" && string(got.body) != tc.body {
				t.Errorf("body: got %q want %q", got.body, tc.body)
			}
			if tc.body == "" {
				if v := got.header.Get("Content-Length"); v != "" {
					t.Errorf("relay added Content-Length: %q to a bodyless %s", v, tc.method)
				}
				if len(got.transfer) != 0 {
					t.Errorf("relay added Transfer-Encoding %v to a bodyless %s", got.transfer, tc.method)
				}
				if len(got.body) != 0 {
					t.Errorf("relay invented a %d-byte body on %s", len(got.body), tc.method)
				}
			}
		})
	}
}

// TestRedirectNotFollowed matters most for /v1/models.
func TestRedirectNotFollowed(t *testing.T) {
	var reached bool
	elsewhere := memhttptest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))
	defer elsewhere.Close()

	upstream := memhttptest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+"/v1/models", http.StatusFound)
	}))
	defer upstream.Close()

	gw := newTestGateway(t, upstream.URL)
	req, _ := http.NewRequest(http.MethodGet, gw.URL+"/v1/models", nil)
	resp, err := probeClient().Do(req)
	if err != nil {
		t.Fatalf("request through gateway: %v", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	if resp.StatusCode != http.StatusFound {
		t.Errorf("status: got %d want 302 relayed to the client", resp.StatusCode)
	}
	if reached {
		t.Error("gateway followed the redirect; the developer's credential would have gone to the redirect target")
	}
	if loc := resp.Header.Get("Location"); loc == "" {
		t.Error("Location header not relayed, so the client cannot see where it was sent")
	}
}

// TestUpstreamUnreachableIsNotSilent keeps a dead upstream legible.
func TestUpstreamUnreachableIsNotSilent(t *testing.T) {
	gw := newTestGateway(t, "http://127.0.0.1:1")

	req, _ := http.NewRequest(http.MethodPost, gw.URL+"/v1/messages", strings.NewReader(`{}`))
	resp, err := probeClient().Do(req)
	if err != nil {
		t.Fatalf("request through gateway: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status: got %d want 502", resp.StatusCode)
	}
	if !strings.Contains(string(body), "openbox_gateway_error") {
		t.Errorf("gateway failure not attributable to the gateway: %q", body)
	}
}
