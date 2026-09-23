package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/gatewayemit"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/client/memhttptest"
	"github.com/openbox-ai/openbox-shift-left/internal/gateway"
	"github.com/openbox-ai/openbox-shift-left/internal/transport"
)

// Transportcapture_test.go; the control for the transport lane. A fake at each
// end of a seam with no implementation between them proves nothing about the
// seam.

// refusedUpstream is a loopback port nothing listens on, so the relay's
// upstream dial fails immediately and deterministically; no DNS, no timeout,
// no reliance on this sandbox's egress.
const refusedUpstream = "https://127.0.0.1:1"

var fakeKey = "sk-" + "ant-" + strings.Repeat("0123456789", 4)

// TestTransportLaneRecordsThroughTheRealChain.
func TestTransportLaneRecordsThroughTheRealChain(t *testing.T) {
	spoolDir := t.TempDir()
	const sessionID = "7f3c9b2e-0000-5000-a000-00000000cafe"

	ca, err := transport.LoadOrCreateCA(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreateCA: %v", err)
	}

	// testSpoolDeliver writes straight to a local spool dir; no flusher process
	// is ever spawned from a test binary.
	em := &gatewayemit.Emitter{
		Elected: func() bool { return true },
		Lane:    gatewayemit.LaneProxy,
		Deliver: testSpoolDeliver(spoolDir),
		DID:     func() string { return "did:aip:7f3c9b2e-0000-5000-a000-00000000feed" },
		Warn:    func(string, ...any) {},
	}

	p, err := transport.New(transport.Config{Upstream: refusedUpstream}, ca, em)
	if err != nil {
		t.Fatalf("transport.New: %v", err)
	}

	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() { clientConn.Close(); serverConn.Close() })
	go p.ServeIntercepted(serverConn, "api.anthropic.com:443")

	if err := clientConn.SetDeadline(time.Now().Add(20 * time.Second)); err != nil {
		t.Fatalf("SetDeadline: %v", err)
	}
	br := bufio.NewReader(clientConn)
	status, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read CONNECT status: %v", err)
	}
	if !strings.HasPrefix(status, "HTTP/1.1 200") {
		t.Fatalf("CONNECT status = %q, want 200", status)
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read CONNECT headers: %v", err)
		}
		if strings.TrimSpace(line) == "" {
			break
		}
	}
	if br.Buffered() != 0 {
		t.Fatalf("%d bytes buffered past the CONNECT response; the TLS handshake would lose them",
			br.Buffered())
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca.CertPEM()) {
		t.Fatal("CA PEM did not parse")
	}
	tc := tls.Client(clientConn, &tls.Config{ServerName: "api.anthropic.com", RootCAs: pool})
	if err := tc.Handshake(); err != nil {
		t.Fatalf("TLS handshake against the project CA: %v", err)
	}

	req, err := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages",
		strings.NewReader(`{"model":"claude-opus-5","messages":[]}`))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("X-Claude-Code-Session-Id", sessionID)
	req.Header.Set("Anthropic-Version", "2023-06-01")
	req.Header.Set("X-Api-Key", fakeKey)
	if err := req.Write(tc); err != nil {
		t.Fatalf("write request: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(tc), req)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("relay answered %d, want 502 from the refused upstream", resp.StatusCode)
	}

	ev := readOneSpooledEvent(t, spoolDir, sessionID)

	if ev.ProxyRequestID == "" {
		t.Error("the spooled event carries no proxy_request_id, so its activity_id is empty and " +
			"nothing downstream can join it")
	}
	if ev.GatewayRequestID != "" || ev.OtelRequestID != "" {
		t.Errorf("the transport lane filed under another producer: gateway=%q otel=%q",
			ev.GatewayRequestID, ev.OtelRequestID)
	}
	if !strings.HasPrefix(ev.ProxyRequestID, gatewayemit.ProxyIDPrefix) {
		t.Errorf("proxy_request_id %q does not carry the lane's prefix", ev.ProxyRequestID)
	}
	if ev.EventType != client.EventTurnCompleted {
		t.Errorf("event_type = %q, want the completed half", ev.EventType)
	}
	if ev.Span == nil {
		t.Fatal("no span on the spooled event")
	}
	if ev.Span.CredentialFingerprint == "" {
		t.Error("no credential fingerprint; account binding has nothing to match on")
	}
	// Both directions, because only one of them is a control: "the secret is
	// absent" passes trivially if nothing about the credential reached the record
	// at all. The header map used to be the non-vacuity witness; the headers no
	// longer reach the spool (they only ever egressed inside spans[], which core
	// discards, and they are the highest-risk class this client handles), so the
	// fingerprint is the witness now -- it is present precisely when a credential
	// was sent, and it is derived rather than carried.
	if ev.Span.CredentialFingerprint == "" {
		t.Error("nothing about the credential reached the record, so the absence check below " +
			"would pass without measuring anything")
	}
	if len(ev.Span.RequestHeaders) != 0 || len(ev.Span.ResponseHeaders) != 0 {
		t.Errorf("observed headers reached the spool: req=%v resp=%v",
			ev.Span.RequestHeaders, ev.Span.ResponseHeaders)
	}
	if raw := readSpooledLines(t, spoolDir); len(raw) > 0 {
		for _, line := range raw {
			if strings.Contains(string(line), fakeKey) {
				t.Error("the live credential was written to the spool verbatim")
			}
		}
	}
	if ev.SessionID != sessionID {
		t.Errorf("session_id = %q, want %q", ev.SessionID, sessionID)
	}
	if !strings.HasSuffix(ev.Span.HTTPURL, "/v1/messages") {
		t.Errorf("http_url = %q, want the relayed path preserved", ev.Span.HTTPURL)
	}
}

// TestTransportCommandListensAndRecords is the socket-level version: the real
// `openbox transport` command, a real listener, a real CONNECT from a real
// http.Client.
func TestTransportCommandListensAndRecords(t *testing.T) {
	memhttptest.RequireBind(t)

	spoolDir := t.TempDir()
	openboxHome := t.TempDir()
	t.Setenv("OPENBOX_SPOOL_DIR", spoolDir)
	t.Setenv("OPENBOX_HOME", openboxHome)
	t.Setenv(devconfig.EnvAgentID, "7f3c9b2e-0000-5000-a000-00000000feed")
	t.Setenv("OPENBOX_REALTIME", "0")

	addr := freeLoopbackAddr(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ready := make(chan string, 1)
	a, _, errb := testApp(nil)
	a.transportCtx = ctx
	a.transportReady = func(bound string) { ready <- bound }

	done := make(chan int, 1)
	go func() { done <- a.runTransport([]string{"--addr", addr, "--verbose", "--elected"}) }()

	select {
	case <-ready:
	case <-time.After(15 * time.Second):
		t.Fatalf("the transport never reported ready; stderr: %s", errb.String())
	}

	tunnelled := probeThroughProxy(t, addr, "127.0.0.1:1")
	if tunnelled == nil {
		t.Log("blind tunnel to a refused port failed, as expected")
	}
	if n := countSpoolFiles(t, spoolDir); n != 0 {
		t.Errorf("%d spool file(s) exist after a non-allowlisted CONNECT; a tunnelled host must "+
			"produce no capture at all", n)
	}

	cancel()
	select {
	case code := <-done:
		if code != exitOK {
			t.Errorf("runTransport exited %d; stderr: %s", code, errb.String())
		}
	case <-time.After(20 * time.Second):
		t.Fatal("runTransport did not return after cancellation")
	}
}

// probeThroughProxy issues one CONNECT through the proxy and returns the
// error, if any. It never sends application bytes: the assertion is about
// whether a capture was produced, not about the tunnel's payload.
func probeThroughProxy(t *testing.T, proxyAddr, target string) error {
	t.Helper()
	conn, err := net.DialTimeout("tcp", proxyAddr, 10*time.Second)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatalf("SetDeadline: %v", err)
	}
	if _, err := io.WriteString(conn, "CONNECT "+target+" HTTP/1.1\r\nHost: "+target+"\r\n\r\n"); err != nil {
		return err
	}
	br := bufio.NewReader(conn)
	_, err = br.ReadString('\n')
	return err
}

func readOneSpooledEvent(t *testing.T, spoolDir, sessionID string) client.DevEvent {
	t.Helper()
	spool := hookflow.Spool{Dir: spoolDir}
	raw, err := os.ReadFile(spool.SessionPath(sessionID))
	if err != nil {
		entries, _ := os.ReadDir(spoolDir)
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("no spool file for session %s (dir holds %v): %v; the relay answered, so the "+
			"break is between the capture and the spool", sessionID, names, err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	// One relayed call is one activity with TWO rows, an ActivityStarted and an
	// ActivityCompleted sharing an activity_id. Every in-path row used to be
	// single-sided, which is a contract violation rather than a documentable
	// choice: the dashboard timeline pairs the two, so what a reader saw as "one
	// Started, many Completed" was exactly this.
	if len(lines) != 2 {
		t.Fatalf("spool holds %d events, want the Started/Completed pair", len(lines))
	}
	var started, completed client.DevEvent
	if err := json.Unmarshal([]byte(lines[0]), &started); err != nil {
		t.Fatalf("unmarshal spooled event: %v", err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &completed); err != nil {
		t.Fatalf("unmarshal spooled event: %v", err)
	}
	if started.EventType != client.EventTurnStarted || completed.EventType != client.EventTurnCompleted {
		t.Fatalf("spooled %s then %s, want TurnStarted then TurnCompleted; the spool delivers in "+
			"append order and core looks a completion's start up at ingest",
			started.EventType, completed.EventType)
	}
	// The completed half is what a caller means by "the event": it carries the
	// status, the duration and the response.
	return completed
}

func countSpoolFiles(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatalf("read spool dir: %v", err)
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) != ".lock" {
			n++
		}
	}
	return n
}

var _ = gateway.Captured{}

// TestTheTransportDaemonResolvesItsOwnElection is the whole of phase 06 in one
// assertion: the unit's --settings value reaches the daemon, the daemon reads
// that file, the election answers from it, and the emitter's gate is wired to
// the answer.
//
// It is worth an integration test rather than three unit ones because each link
// fails by going quiet. A daemon that cannot read the settings reported "no lane
// is routed" -- indistinguishable from a machine nobody configured -- for the
// whole life of the feature, and an emitter with no gate at all would silence
// the only lane that works.
func TestTheTransportDaemonResolvesItsOwnElection(t *testing.T) {
	memhttptest.RequireBind(t)

	for _, tc := range []struct {
		name        string
		settings    string
		wantElected bool
	}{
		{
			name:        "routed at loopback: this lane is the producer",
			settings:    `{"env":{"HTTPS_PROXY":"http://127.0.0.1:8790"}}`,
			wantElected: true,
		},
		{
			name: "a base URL takes this relay out of the path",
			settings: `{"env":{"HTTPS_PROXY":"http://127.0.0.1:8790",` +
				`"ANTHROPIC_BASE_URL":"http://127.0.0.1:8788"}}`,
			wantElected: false,
		},
		{
			name:        "nothing routed: no lane is the producer",
			settings:    `{"hooks":{}}`,
			wantElected: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			settingsPath := filepath.Join(home, ".claude", "settings.json")
			if err := os.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(settingsPath, []byte(tc.settings), 0o644); err != nil {
				t.Fatal(err)
			}

			t.Setenv("OPENBOX_SPOOL_DIR", t.TempDir())
			t.Setenv("OPENBOX_HOME", t.TempDir())
			t.Setenv(devconfig.EnvAgentID, "7f3c9b2e-0000-5000-a000-00000000feed")
			t.Setenv("OPENBOX_REALTIME", "0")

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ready := make(chan string, 1)
			a, _, errb := testApp(nil)
			a.transportCtx = ctx
			a.transportReady = func(bound string) { ready <- bound }

			done := make(chan int, 1)
			// No --elected: the point is that the daemon works this out for itself
			// from the path the unit handed it.
			go func() {
				done <- a.runTransport([]string{"--addr", freeLoopbackAddr(t), "--settings", settingsPath})
			}()
			select {
			case <-ready:
			case <-time.After(15 * time.Second):
				t.Fatalf("the transport never reported ready; stderr: %s", errb.String())
			}
			cancel()
			select {
			case <-done:
			case <-time.After(20 * time.Second):
				t.Fatal("runTransport did not return after cancellation")
			}

			log := errb.String()
			if strings.Contains(log, "CANNOT DECIDE") {
				t.Fatalf("the daemon could not read the settings path it was given: %s", log)
			}
			elected := strings.Contains(log, "transport: elected producer")
			if elected != tc.wantElected {
				t.Errorf("elected=%v, want %v; log: %s", elected, tc.wantElected, log)
			}
			if !elected && !strings.Contains(log, "NOT the elected producer") {
				t.Errorf("an unelected lane did not say so: %s", log)
			}
		})
	}
}

// TestADaemonWithNoUsableSettingsPathSaysSo is the diagnostic that would have
// caught defect 6a, asserted at the daemon. A relative path is what
// SettingsPath("") produces, and launchd resolves it against /.
func TestADaemonWithNoUsableSettingsPathSaysSo(t *testing.T) {
	memhttptest.RequireBind(t)

	t.Setenv("OPENBOX_SPOOL_DIR", t.TempDir())
	t.Setenv("OPENBOX_HOME", t.TempDir())
	t.Setenv("OPENBOX_REALTIME", "0")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan string, 1)
	a, _, errb := testApp(nil)
	a.transportCtx = ctx
	a.transportReady = func(bound string) { ready <- bound }

	done := make(chan int, 1)
	go func() {
		done <- a.runTransport([]string{"--addr", freeLoopbackAddr(t),
			"--settings", filepath.Join(".claude", "settings.json")})
	}()
	select {
	case <-ready:
	case <-time.After(15 * time.Second):
		t.Fatalf("the transport never reported ready; stderr: %s", errb.String())
	}
	cancel()
	<-done

	log := errb.String()
	if !strings.Contains(log, "CANNOT DECIDE") {
		t.Errorf("a daemon reading an unreachable settings path did not say so, which is exactly "+
			"how this went unnoticed: %s", log)
	}
	if strings.Contains(log, "no lane is routed") {
		t.Errorf("the daemon still reports a configuration answer it cannot have: %s", log)
	}
}
