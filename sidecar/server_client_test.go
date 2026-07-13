package sidecar

import (
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/client"
)

// startServer binds a Server on a fresh per-test Unix socket and serves until the
// test ends. Returns the socket path.
func startServer(t *testing.T, srv *Server) string {
	t.Helper()
	// Keep the socket path short: Unix socket paths have a ~104-char limit, and
	// t.TempDir() can be long. Use a short name inside it.
	path := filepath.Join(t.TempDir(), "s.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = srv.Serve(ctx, ln); close(done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("server did not shut down within 2s")
		}
	})
	return path
}

func TestRoundTrip_BlockAndAllow(t *testing.T) {
	srv := NewServer(ServerConfig{})
	srv.SetBundle(&Bundle{Version: "v1", Rules: []Rule{
		{ID: "rmrf", Match: RuleMatch{ToolName: "Bash", AttributeContains: map[string]string{"command": "rm -rf /"}},
			Decision: "block", Reason: "destructive"},
	}})
	path := startServer(t, srv)
	c := NewClient(ClientConfig{SocketPath: path, Timeout: time.Second})

	// Matching call → BLOCK, not fail-open.
	d := c.Decide(context.Background(), toolCall("Bash", client.ToolShell, map[string]any{"command": "rm -rf / now"}))
	if d.FailOpen {
		t.Fatalf("expected real decision, got fail-open (%s)", d.Source)
	}
	if d.Evaluation.Verdict != client.VerdictBlock {
		t.Fatalf("verdict = %q, want BLOCK", d.Evaluation.Verdict)
	}
	if d.Source != sourceLocalBundle {
		t.Errorf("source = %q, want %q", d.Source, sourceLocalBundle)
	}

	// Non-matching call → ALLOW.
	d = c.Decide(context.Background(), toolCall("Bash", client.ToolShell, map[string]any{"command": "ls"}))
	if d.FailOpen || d.Evaluation.Verdict != client.VerdictAllow {
		t.Fatalf("non-matching: got failopen=%v verdict=%q, want ALLOW", d.FailOpen, d.Evaluation.Verdict)
	}
}

func TestFailOpen_SocketAbsent(t *testing.T) {
	// A path with no daemon behind it → the Client fails open (the common case
	// when enforcement is off or the daemon isn't running).
	c := NewClient(ClientConfig{SocketPath: filepath.Join(t.TempDir(), "absent.sock"), Timeout: 50 * time.Millisecond})
	d := c.Decide(context.Background(), toolCall("Bash", client.ToolShell, nil))
	if !d.FailOpen {
		t.Fatalf("absent socket: expected fail-open, got %+v", d)
	}
	if d.Evaluation.Verdict != client.VerdictUnknown {
		t.Errorf("fail-open verdict = %q, want Unknown (degrade-to-observe)", d.Evaluation.Verdict)
	}
	if d.Evaluation.WouldBlock() {
		t.Error("fail-open must never WouldBlock")
	}
}

func TestFailOpen_HungServerWithinBound(t *testing.T) {
	// A daemon that accepts but never replies must NOT hang the tool call: the
	// Client fails open within ~timeout. This is INV-3b's teeth.
	path := filepath.Join(t.TempDir(), "hung.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			// Accept and hold the connection open without ever replying.
			_ = conn
		}
	}()

	timeout := 50 * time.Millisecond
	c := NewClient(ClientConfig{SocketPath: path, Timeout: timeout})
	start := time.Now()
	d := c.Decide(context.Background(), toolCall("Bash", client.ToolShell, nil))
	elapsed := time.Since(start)

	if !d.FailOpen {
		t.Fatalf("hung server: expected fail-open, got %+v", d)
	}
	// Bounded: should return within a small multiple of the timeout, never hang.
	if elapsed > 20*timeout {
		t.Fatalf("hung server: Decide took %v, exceeds bound (timeout=%v)", elapsed, timeout)
	}
}

func TestColdStart_FailOpenNoBundle(t *testing.T) {
	// A running daemon that has not yet synced a bundle → fail-open allow, and it
	// says so via Source (not a fabricated real verdict).
	srv := NewServer(ServerConfig{})
	path := startServer(t, srv)
	c := NewClient(ClientConfig{SocketPath: path, Timeout: time.Second})
	d := c.Decide(context.Background(), toolCall("Bash", client.ToolShell, nil))
	if d.FailOpen {
		t.Fatalf("cold start is a real (server) response, not a client fail-open: %+v", d)
	}
	if d.Evaluation.Verdict != client.VerdictAllow || d.Source != sourceFailOpenNoBundle {
		t.Fatalf("cold start: verdict=%q source=%q, want ALLOW/%s", d.Evaluation.Verdict, d.Source, sourceFailOpenNoBundle)
	}
}

func TestDecide_MissingSessionID(t *testing.T) {
	srv := NewServer(ServerConfig{})
	srv.SetBundle(&Bundle{Version: "v1"})
	resp := srv.decide(DecisionRequest{Protocol: ProtocolVersion, EventType: client.EventToolCall})
	if resp.Evaluation.WouldBlock() {
		t.Error("missing session id must not block")
	}
	if resp.Error == "" {
		t.Error("expected a non-secret diagnostic for missing session id")
	}
}

func TestDecide_Staleness(t *testing.T) {
	// A bundle older than the freshness window is flagged Stale but still served.
	now := time.Now()
	clock := func() time.Time { return now }
	srv := NewServer(ServerConfig{Freshness: time.Minute, now: clock})
	srv.SetBundle(&Bundle{Version: "v1"})
	// Advance the clock past the freshness window.
	now = now.Add(2 * time.Minute)
	resp := srv.decide(toolCall("Bash", client.ToolShell, nil))
	if !resp.Stale {
		t.Error("expected Stale=true for an aged bundle")
	}
	if resp.Evaluation.WouldBlock() {
		t.Error("staleness must never turn into a block")
	}
}

func TestDecide_UnsupportedProtocol(t *testing.T) {
	srv := NewServer(ServerConfig{})
	srv.SetBundle(&Bundle{Version: "v1"})
	resp := srv.decide(DecisionRequest{Protocol: 999, SessionID: "s"})
	if resp.Evaluation.WouldBlock() {
		t.Error("unknown protocol must fail open, never block")
	}
}
