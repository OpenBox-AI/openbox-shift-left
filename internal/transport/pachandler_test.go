package transport

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestPACHandlerServesGetAndHead: GET and HEAD on /proxy.pac answer with the
// generated body (GET) or its length (HEAD), the PAC content type and
// no-store, so a browser never caches a stale union.
func TestPACHandlerServesGetAndHead(t *testing.T) {
	h := pacHandler(DefaultAddr, []string{"claude-code"})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/proxy.pac")
	if err != nil {
		t.Fatalf("GET /proxy.pac: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /proxy.pac status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != pacContentType {
		t.Errorf("Content-Type = %q, want %q", ct, pacContentType)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want %q", cc, "no-store")
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if want := PACBody(DefaultAddr, "claude-code"); string(body) != want {
		t.Errorf("GET body = %q, want %q", body, want)
	}

	head, err := http.Head(srv.URL + "/proxy.pac")
	if err != nil {
		t.Fatalf("HEAD /proxy.pac: %v", err)
	}
	defer head.Body.Close()
	if head.StatusCode != http.StatusOK {
		t.Fatalf("HEAD /proxy.pac status = %d, want 200", head.StatusCode)
	}
	if ct := head.Header.Get("Content-Type"); ct != pacContentType {
		t.Errorf("HEAD Content-Type = %q, want %q", ct, pacContentType)
	}
}

// TestPACHandler404sEverythingElse: any other path, and POST specifically on
// /proxy.pac, must 404 -- this endpoint never accepts input.
func TestPACHandler404sEverythingElse(t *testing.T) {
	h := pacHandler(DefaultAddr, []string{"claude-code"})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	cases := []struct {
		method, path string
	}{
		{http.MethodGet, "/"},
		{http.MethodGet, "/proxy.pac/"},
		{http.MethodGet, "/PROXY.PAC"},
		{http.MethodGet, "/other"},
		{http.MethodPost, "/proxy.pac"},
		{http.MethodPut, "/proxy.pac"},
		{http.MethodDelete, "/proxy.pac"},
	}
	for _, c := range cases {
		req, err := http.NewRequest(c.method, srv.URL+c.path, nil)
		if err != nil {
			t.Fatalf("build request %s %s: %v", c.method, c.path, err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", c.method, c.path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s %s status = %d, want 404", c.method, c.path, resp.StatusCode)
		}
	}
}

// TestNonproxyHandlerServesPACWithoutTouchingConnectOrRelay: the PAC handler
// is wired as the goproxy engine's NonproxyHandler, and it must
// never interfere with a CONNECT (allowlisted or not) or an absolute-URI
// proxied request -- only a direct, non-absolute-URI request like a browser
// fetching its own proxy.pac reaches it.
func TestNonproxyHandlerServesPACWithoutTouchingConnectOrRelay(t *testing.T) {
	ca := testCA(t)
	p, err := New(Config{}, ca, &stubEmitter{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var relayed int
	p.handlerFor = func(string) (http.Handler, error) {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			relayed++
			w.WriteHeader(http.StatusOK)
		}), nil
	}
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)

	// A direct, non-absolute-URI request to the proxy's own address reaches
	// NonproxyHandler and gets the PAC.
	resp, err := http.Get(srv.URL + "/proxy.pac")
	if err != nil {
		t.Fatalf("GET /proxy.pac: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "FindProxyForURL") {
		t.Fatalf("GET /proxy.pac through the real engine did not serve the PAC: status=%d body=%q",
			resp.StatusCode, body)
	}

	// An absolute-URI GET through the proxy (the normal relay path for an
	// allowlisted-by-config upstream test double) must still be relayed, not
	// answered by NonproxyHandler.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	t.Cleanup(upstream.Close)

	proxyURL, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse proxy URL: %v", err)
	}
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}
	absResp, err := client.Get(upstream.URL + "/anything")
	if err != nil {
		t.Fatalf("absolute-URI GET through the proxy: %v", err)
	}
	absResp.Body.Close()
	if absResp.StatusCode != http.StatusTeapot {
		t.Errorf("absolute-URI GET through the proxy status = %d, want %d (the upstream's own status, "+
			"proving it was relayed rather than answered by NonproxyHandler)", absResp.StatusCode, http.StatusTeapot)
	}

	// A CONNECT to the allowlisted host is still hijacked and relayed, not
	// swallowed by NonproxyHandler: reuse the in-memory CONNECT choreography
	// proxy_test.go already exercises, directly against the same *Proxy.
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()
	done := make(chan error, 1)
	go func() { done <- p.interceptConn(serverConn, "api.anthropic.com:443") }()
	if err := clientConn.SetDeadline(time.Now().Add(15 * time.Second)); err != nil {
		t.Fatalf("SetDeadline: %v", err)
	}
	got := readConnectResponse(t, clientConn)
	if !strings.HasPrefix(got, "HTTP/1.1 200") {
		t.Fatalf("CONNECT was not hijacked and relayed; got %q", got)
	}
	clientConn.Close()
	<-done
}
