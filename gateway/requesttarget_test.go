package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/client/memhttptest"
)

// requesttarget_test.go pins the origin-form requirement, whose whole job is to
// keep the upstream HOST fixed.
//
// The target is built by concatenation, so a request-target that does not start
// with "/" splices into the authority instead of the path. These tests drive the
// RAW request line over a socket rather than calling ServeHTTP with a synthetic
// *http.Request, because the bug lives in what net/http's own parser hands the
// handler — a hand-built Request could not reproduce it, and a test that cannot
// reproduce the bug cannot hold the fix.

// rawRequestLine sends one literal request line to a live gateway and returns the
// status line plus body. No http.Client, which would refuse to emit these forms.
func rawRequestLine(t *testing.T, addr, line string) (int, string) {
	t.Helper()
	conn, err := memhttptest.DialContext(context.Background(), "tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := fmt.Fprintf(conn, "%s\r\nHost: %s\r\n\r\n", line, addr); err != nil {
		t.Fatalf("write request line: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// hostPort reduces a test server's URL to the host:port a raw socket needs.
func hostPort(t *testing.T, srv *memhttptest.Server) string {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	return u.Host
}

// TestNonOriginFormTargetsCannotRetargetTheUpstreamHost is the control for the
// host-splice. An authority-form line is the one that produced a syntactically
// valid URL on a different host, so it is asserted by name.
func TestNonOriginFormTargetsCannotRetargetTheUpstreamHost(t *testing.T) {
	// A recorder that must never be reached: any forward at all is a failure,
	// because every line below names a host that is not this one.
	var reached bool
	upstream := memhttptest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	addr := hostPort(t, newTestGateway(t, upstream.URL))

	for _, tc := range []struct {
		name string
		line string
		// oursToRefuse is false for a form net/http answers BEFORE the handler,
		// so the test asserts the property that actually matters (nothing was
		// forwarded) without claiming our code produced the response.
		oursToRefuse bool
	}{
		// The measured case: r.RequestURI is "evil.com:443", which concatenates
		// onto the upstream to yield host "…comevil.com:443".
		{"authority form (CONNECT)", "CONNECT evil.com:443 HTTP/1.1", true},
		{"authority form with a path", "CONNECT evil.com:443/v1/messages HTTP/1.1", true},
		// Absolute-form does not currently reach another host (it yields an
		// invalid port and a 502), but it is refused by name so a later change to
		// the concatenation cannot quietly make it one.
		{"absolute form", "GET http://evil.com/v1/messages HTTP/1.1", true},
		// Asterisk-form never reaches ServeHTTP: net/http intercepts
		// `OPTIONS *` in serverHandler.ServeHTTP and answers it with its own
		// global handler. Kept in the table because the property under test is
		// "not forwarded upstream", and that holds for the same reason either
		// way — but asserting OUR 400 here would be asserting the stdlib's
		// behaviour and would break if it ever delegated instead.
		{"asterisk form", "OPTIONS * HTTP/1.1", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reached = false
			status, body := rawRequestLine(t, addr, tc.line)

			// The invariant, and it holds for every form: the upstream host is
			// fixed, so a target naming another host reaches nothing.
			if reached {
				t.Error("the request was forwarded upstream; a non-origin-form target must not be relayed at all")
			}
			if !tc.oursToRefuse {
				return
			}
			if status != http.StatusBadRequest {
				t.Errorf("status = %d, want %d (%s must be refused, not relayed)",
					status, http.StatusBadRequest, tc.name)
			}
			// The refusal has to name the reason: a bare 400 is indistinguishable
			// from the gateway being broken.
			var env struct {
				Error struct {
					Type    string `json:"type"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal([]byte(body), &env); err != nil {
				t.Fatalf("refusal body is not JSON: %v (%q)", err, body)
			}
			if !strings.Contains(env.Error.Message, "origin-form") {
				t.Errorf("refusal message %q does not say why", env.Error.Message)
			}
		})
	}
}

// TestOriginFormStillRelaysVerbatim is the other half: the check must not narrow
// what a real client sends. A percent-escape Go would not have chosen itself is
// included because it is the case the `target` comment exists for — the forwarded
// target must stay byte-identical.
func TestOriginFormStillRelaysVerbatim(t *testing.T) {
	for _, target := range []string{
		"/v1/messages",
		"/v1/messages?beta=true",
		"/v1/messages/count_tokens",
		"/v1/models",
		"/v1/%2E%2E/messages", // escapes preserved, not re-encoded
		"//v1/messages",       // leading double slash is a PATH, not an authority
	} {
		t.Run(target, func(t *testing.T) {
			var got string
			upstream := memhttptest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = r.RequestURI
				w.WriteHeader(http.StatusOK)
			}))
			defer upstream.Close()
			addr := hostPort(t, newTestGateway(t, upstream.URL))

			status, _ := rawRequestLine(t, addr, "GET "+target+" HTTP/1.1")
			if status != http.StatusOK {
				t.Fatalf("status = %d, want 200 — origin-form must still relay", status)
			}
			if got != target {
				t.Errorf("upstream received %q, want %q (byte-identical)", got, target)
			}
		})
	}
}
