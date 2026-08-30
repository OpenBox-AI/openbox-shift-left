package gateway

import (
	"fmt"
	"io"
	"net/http"

	"github.com/openbox-ai/openbox-shift-left/internal/client/memhttptest"
	"strings"
	"sync"
	"testing"
)

type verboseRecorder struct {
	mu    sync.Mutex
	lines []string
}

func (v *verboseRecorder) logf(format string, args ...any) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.lines = append(v.lines, fmt.Sprintf(format, args...))
}

func (v *verboseRecorder) all() string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return strings.Join(v.lines, "\n")
}

func secretForLog() string { return "sk" + "-ant-" + strings.Repeat("z4", 20) }

// TestVerboseNeverLogsCredentialsOrBodies is the control on the whole feature.
func TestVerboseNeverLogsCredentialsOrBodies(t *testing.T) {
	upstream := memhttptest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("X-Upstream-Secret-Header", "upstream-header-value-must-not-log")
		io.WriteString(w, `{"role":"assistant","text":"RESPONSE_BODY_MUST_NOT_LOG"}`)
	}))
	defer upstream.Close()

	rec := &verboseRecorder{}
	g, err := New(Config{Addr: DefaultAddr, Upstream: upstream.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srv := memhttptest.NewServer(t, g.WithVerbose(rec.logf))
	defer srv.Close()

	queryToken := "QUERY" + "_TOKEN_MUST_NOT_LOG"
	req, err := http.NewRequest(http.MethodPost,
		srv.URL+"/v1/messages?access_token="+queryToken,
		strings.NewReader(`{"model":"claude-opus-4","messages":[{"role":"user","content":"REQUEST_BODY_MUST_NOT_LOG"}]}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+secretForLog())
	req.Header.Set("X-Api-Key", secretForLog())
	req.Header.Set("X-Claude-Code-Session-Id", "sess-verbose")

	resp, err := probeClient().Do(req)
	if err != nil {
		t.Fatalf("relay: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	got := rec.all()
	if got == "" {
		t.Fatal("verbose mode logged nothing for a relayed call")
	}
	for _, forbidden := range []string{
		secretForLog(),
		queryToken,
		"REQUEST_BODY_MUST_NOT_LOG",
		"RESPONSE_BODY_MUST_NOT_LOG",
		"upstream-header-value-must-not-log",
		"access_token",
	} {
		if strings.Contains(got, forbidden) {
			t.Errorf("verbose log leaked %q; log was:\n%s", forbidden, got)
		}
	}
}

// TestVerboseReportsArrivalAndOutcome is the feature itself: a developer must
// be able to tell "traffic is reaching this process" from "nothing is".
func TestVerboseReportsArrivalAndOutcome(t *testing.T) {
	upstream := memhttptest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		io.WriteString(w, `{}`)
	}))
	defer upstream.Close()

	rec := &verboseRecorder{}
	g, err := New(Config{Addr: DefaultAddr, Upstream: upstream.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srv := memhttptest.NewServer(t, g.WithVerbose(rec.logf))
	defer srv.Close()

	resp, err := probeClient().Post(srv.URL+"/v1/messages", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("relay: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	got := rec.all()
	if !strings.Contains(got, "→ POST /v1/messages") {
		t.Errorf("no arrival line; log was:\n%s", got)
	}
	if !strings.Contains(got, "← POST /v1/messages 418") {
		t.Errorf("no outcome line carrying the upstream status; log was:\n%s", got)
	}
}

// TestVerboseReportsARejectionTooKeepsSilenceHonest. A request refused by the
// relay's own checks never reaches the upstream and never produces evidence;
// so without a line here it is indistinguishable, from the terminal, from no
// traffic at all.
func TestVerboseReportsARejectionTooKeepsSilenceHonest(t *testing.T) {
	reached := false
	upstream := memhttptest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		io.WriteString(w, `{}`)
	}))
	defer upstream.Close()

	rec := &verboseRecorder{}
	g, err := New(Config{Addr: DefaultAddr, Upstream: upstream.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// A refused body is never forwarded even in part.
	g.maxBody = 8
	srv := memhttptest.NewServer(t, g.WithVerbose(rec.logf))
	defer srv.Close()

	resp, err := probeClient().Post(srv.URL+"/v1/messages", "application/json",
		strings.NewReader(strings.Repeat("x", 4096)))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if reached {
		t.Error("an over-cap body was forwarded upstream")
	}

	got := rec.all()
	if !strings.Contains(got, "→ POST") {
		t.Errorf("a refused request logged no arrival; log was:\n%s", got)
	}
	if !strings.Contains(got, "✗ relay refused") {
		t.Errorf("a refused request logged no refusal; log was:\n%s", got)
	}
}

// TestVerboseOffIsSilent keeps the default path exactly what it was.
func TestVerboseOffIsSilent(t *testing.T) {
	upstream := memhttptest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{}`)
	}))
	defer upstream.Close()

	g, err := New(Config{Addr: DefaultAddr, Upstream: upstream.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if g.verbose() {
		t.Error("a Gateway with no WithVerbose reports itself verbose")
	}
	srv := memhttptest.NewServer(t, g) // no panic on the nil seam is the assertion
	defer srv.Close()
	resp, err := probeClient().Post(srv.URL+"/v1/messages", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("relay: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}
