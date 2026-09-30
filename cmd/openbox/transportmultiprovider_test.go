package main

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/gatewayemit"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/client/fakecore"
	"github.com/openbox-ai/openbox-shift-left/internal/client/memhttptest"
	"github.com/openbox-ai/openbox-shift-left/internal/gateway"
	"github.com/openbox-ai/openbox-shift-left/internal/gateway/gatewaytest"
	"github.com/openbox-ai/openbox-shift-left/internal/transport"
)

const (
	multiCCDID    = "did:aip:00000000-0000-5000-a000-0000000000a1"
	multiCodexDID = "did:aip:00000000-0000-5000-a000-0000000000a2"
)

// multiRelay is the production relay wiring (relayProviderLanes and
// transport.CandidatesForHost) over an in-memory upstream, delivering into one
// spool directory per provider so a test can see who recorded what.
type multiRelay struct {
	p       *transport.Proxy
	ca      *transport.CA
	spools  map[string]string
	verbose *warnLog
	warn    *warnLog
}

// testHostHeader carries the host a test client asked for through the relay,
// so the captured URL's host can be restored per call without shared state.
const testHostHeader = "X-Test-Requested-Host"

func newMultiRelay(t *testing.T, identities map[string]providerIdentity, providers ...string) *multiRelay {
	t.Helper()
	upstream := memhttptest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"type":"message","role":"assistant","content":[{"type":"text","text":"ok"}]}`)
	}))
	t.Cleanup(upstream.Close)
	gatewaytest.SwapUpstreamDial(t, memhttptest.DialContext)
	t.Setenv(devconfig.EnvHaltDir, t.TempDir())

	ca, err := transport.LoadOrCreateCA(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreateCA: %v", err)
	}
	r := &multiRelay{ca: ca, spools: map[string]string{}, verbose: &warnLog{}, warn: &warnLog{}}
	spools := map[string]func(context.Context, client.DevEvent) bool{}
	for _, tool := range []string{"claude-code", "codex"} {
		r.spools[tool] = t.TempDir()
		spools[tool] = testSpoolDeliver(r.spools[tool])
	}
	force := true
	em := &gatewayemit.Emitter{
		Lane: gatewayemit.LaneProxy,
		Deliver: func(ctx context.Context, ev client.DevEvent) bool {
			deliver, ok := spools[ev.Tool.Name]
			return ok && deliver(ctx, ev)
		},
		Warn:              r.warn.record,
		Verbose:           r.verbose.record,
		CandidatesForHost: transport.CandidatesForHost,
		// The claude-code election is forced on: this test is about
		// attribution and identity, not about settings-derived election.
		Providers: relayProviderLanes(identities, filepath.Join(t.TempDir(), "settings.json"), &force),
	}
	p, err := transport.New(transport.Config{Upstream: upstream.URL, Providers: providers}, ca, hostRestoringEmitter{next: em})
	if err != nil {
		t.Fatalf("transport.New: %v", err)
	}
	r.p = p
	return r
}

// hostRestoringEmitter puts the host a client asked for back into the
// captured URL. A test upstream override makes the relay capture the
// override's own address, while a real relay captures the provider's host,
// and the host is what attribution reads.
type hostRestoringEmitter struct {
	next gateway.Emitter
}

func (h hostRestoringEmitter) Emit(ctx context.Context, c gateway.Captured) {
	if u, err := url.Parse(c.HTTPURL); err == nil {
		if host := c.RequestHeaders[testHostHeader]; host != "" {
			u.Host = host
		}
		c.HTTPURL = u.String()
	}
	h.next.Emit(ctx, c)
}

func (r *multiRelay) call(t *testing.T, host, path string, headers map[string]string) {
	t.Helper()
	conn := connectAndHandshake(t, r.p, r.ca, host)
	req, err := http.NewRequest(http.MethodPost, "https://"+host+path,
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(testHostHeader, host)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if err := req.Write(conn); err != nil {
		t.Fatalf("write request: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}

func (r *multiRelay) sessions(tool string) []string {
	entries, err := os.ReadDir(r.spools[tool])
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".jsonl") {
			out = append(out, e.Name())
		}
	}
	return out
}

func bothIdentities() map[string]providerIdentity {
	return map[string]providerIdentity{
		"claude-code": {DID: multiCCDID},
		"codex":       {DID: multiCodexDID},
	}
}

func TestRelayRecordsAClaudeCodeCallOnItsOwnHostAndOnTheSharedHost(t *testing.T) {
	r := newMultiRelay(t, bothIdentities(), "claude-code", "codex")

	r.call(t, "api.anthropic.com", "/v1/messages", map[string]string{"X-Claude-Code-Session-Id": "cc-own-host"})
	waitForSpool(t, r.spools["claude-code"], "cc-own-host", r.warn)

	r.call(t, "api.meta.ai", "/v1/responses", map[string]string{"X-Claude-Code-Session-Id": "cc-shared-host"})
	waitForSpool(t, r.spools["claude-code"], "cc-shared-host", r.warn)

	if got := r.sessions("codex"); len(got) != 0 {
		t.Errorf("codex recorded %v for claude-code's calls", got)
	}
	for _, ev := range readBothSpooledHalves(t, r.spools["claude-code"]) {
		if ev.Tool.Name != "claude-code" || ev.DeveloperDID != multiCCDID {
			t.Errorf("event is %s signed %s, want claude-code signed %s", ev.Tool.Name, ev.DeveloperDID, multiCCDID)
		}
	}
}

func TestRelaySkipsASharedHostCallWithNoCarrier(t *testing.T) {
	r := newMultiRelay(t, bothIdentities(), "claude-code", "codex")

	r.call(t, "api.meta.ai", "/v1/responses", nil)
	r.call(t, "api.meta.ai", "/v1/chat/completions", map[string]string{"X-Client-Request-Id": "not-codex-only"})
	// A later, attributable call: once it has landed, the earlier ones had
	// their chance to be recorded too.
	r.call(t, "api.meta.ai", "/v1/responses", map[string]string{"X-Claude-Code-Session-Id": "cc-after-skips"})
	waitForSpool(t, r.spools["claude-code"], "cc-after-skips", r.warn)

	if got := r.sessions("claude-code"); len(got) != 1 {
		t.Errorf("claude-code spool holds %v, want only the attributed call", got)
	}
	if got := r.sessions("codex"); len(got) != 0 {
		t.Errorf("codex spool holds %v, want nothing", got)
	}
	if !strings.Contains(r.verbose.String(), "no_provider_carrier") {
		t.Errorf("the skip was not reported with its reason: %s", r.verbose)
	}
}

// TestRelayDoesNotYetRecordCodexWhileTelemetryIsItsProducer: Codex's carrier
// resolves and its identity exists, but its model calls are still the
// telemetry receiver's, so the relay records nothing and says nothing.
func TestRelayDoesNotYetRecordCodexWhileTelemetryIsItsProducer(t *testing.T) {
	r := newMultiRelay(t, bothIdentities(), "claude-code", "codex")

	r.call(t, "api.openai.com", "/v1/responses", map[string]string{"X-Client-Request-Id": "codex-thread-a"})
	r.call(t, "api.anthropic.com", "/v1/messages", map[string]string{"X-Claude-Code-Session-Id": "cc-after-codex"})
	waitForSpool(t, r.spools["claude-code"], "cc-after-codex", r.warn)

	if got := r.sessions("codex"); len(got) != 0 {
		t.Errorf("codex spool holds %v while it is not elected to this relay", got)
	}
	if got := r.warn.String(); strings.Contains(got, "codex") {
		t.Errorf("an unelected Codex call warned: %s", got)
	}
}

// TestRelayOnACodexOnlyMachineNamesTheMissingIdentity: with no claude-code
// identity the relay still serves, and says which provider it cannot record.
func TestRelayOnACodexOnlyMachineNamesTheMissingIdentity(t *testing.T) {
	r := newMultiRelay(t, map[string]providerIdentity{"codex": {DID: multiCodexDID}}, "claude-code", "codex")

	r.call(t, "api.anthropic.com", "/v1/messages", map[string]string{"X-Claude-Code-Session-Id": "cc-no-identity"})
	r.call(t, "api.openai.com", "/v1/responses", map[string]string{"X-Client-Request-Id": "codex-thread-b"})
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline) &&
		!strings.Contains(r.warn.String(), "for claude-code"); {
		time.Sleep(10 * time.Millisecond)
	}

	if got := r.sessions("claude-code"); len(got) != 0 {
		t.Errorf("claude-code recorded %v with no identity", got)
	}
	if got := r.warn.String(); !strings.Contains(got, "for claude-code") || !strings.Contains(got, "--provider claude-code") {
		t.Errorf("the missing identity is not named: %s (verbose: %s)", got, r.verbose)
	}
}

// TestTransportDaemonRunsOnACodexOnlyMachine: the daemon used to bind
// claude-code's identity store for its whole life and refuse to start without
// it. With only Codex's identity resolvable it must come up, serve, and stop.
func TestTransportDaemonRunsOnACodexOnlyMachine(t *testing.T) {
	memhttptest.RequireBind(t)

	t.Setenv("OPENBOX_SPOOL_DIR", t.TempDir())
	t.Setenv(devconfig.EnvHome, t.TempDir())
	t.Setenv(devconfig.EnvHaltDir, t.TempDir())
	t.Setenv("OPENBOX_REALTIME", "0")
	fake := fakecore.New(t, fakecore.Script{})
	seedV3ToolIdentity(t, "codex", fake.URL())

	addr := freeLoopbackAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ready := make(chan string, 1)
	a, _, errb := testApp(nil)
	a.transportCtx = ctx
	a.transportReady = func(bound string) { ready <- bound }

	done := make(chan int, 1)
	go func() { done <- a.runTransport([]string{"--addr", addr, "--providers", "codex"}) }()

	select {
	case <-ready:
	case <-time.After(15 * time.Second):
		t.Fatalf("the transport never came up on a Codex-only machine; stderr: %s", errb.String())
	}
	cancel()
	select {
	case code := <-done:
		if code != exitOK {
			t.Fatalf("runTransport exited %d; stderr: %s", code, errb.String())
		}
	case <-time.After(20 * time.Second):
		t.Fatal("runTransport did not return after cancellation")
	}

	out := errb.String()
	if line, _, _ := strings.Cut(out[strings.Index(out, "intercepting "):], ";"); !strings.Contains(line, "api.openai.com") {
		t.Errorf("the daemon does not intercept Codex's host; stderr: %s", out)
	}
	if !strings.Contains(out, "claude-code has no usable identity") {
		t.Errorf("the missing claude-code identity was not reported; stderr: %s", out)
	}
}
