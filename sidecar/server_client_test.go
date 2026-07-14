package sidecar

import (
	"bufio"
	"context"
	"encoding/json"
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

// TestDecide_RedactedInputRoundTrips guards STORY-E6-S4's carrier: a
// DecisionResponse carrying redacted_input is surfaced verbatim on
// Decision.RedactedInput (LOCAL-only) for the enforce hook to apply via
// updatedInput. It uses a raw fake server because the metadata-only bundleEvaluator
// produces no redaction today ([EXT-guardrail-redaction]). A fail-open fallback
// carries no RedactedInput (nil), which the enforce hook treats as "no rewrite".
func TestDecide_RedactedInputRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	redacted := `{"content":"api_key=***REDACTED***","file_path":"/x"}`
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = bufio.NewReader(conn).ReadBytes('\n') // drain the request line
		b, _ := json.Marshal(DecisionResponse{
			Protocol:      ProtocolVersion,
			Evaluation:    client.Evaluation{Verdict: client.VerdictAllow},
			Source:        sourceLocalBundle,
			RedactedInput: json.RawMessage(redacted),
		})
		_, _ = conn.Write(append(b, '\n'))
	}()

	c := NewClient(ClientConfig{SocketPath: path, Timeout: time.Second})
	d := c.Decide(context.Background(), toolCall("Write", client.ToolFile, nil))
	if d.FailOpen {
		t.Fatalf("expected a real response, got fail-open (%s)", d.Source)
	}
	if string(d.RedactedInput) != redacted {
		t.Errorf("RedactedInput = %s, want %s", d.RedactedInput, redacted)
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
	// A running daemon that has not yet synced a bundle → NO real verdict (E6-S7,
	// closing E6-S3 INFO-1). The daemon is reachable and answers cleanly, but it
	// produced no policy verdict, so the Client marks the Decision FailOpen (so the
	// E6-S3 failure policy engages: fail-open proceeds; fail-closed denies). The
	// server signals this honestly: VerdictUnknown ("not evaluated") + Source
	// sourceFailOpenNoBundle. This distinguishes a server-side degrade from a
	// client-side one (sourceFailOpenClient) while treating both as FailOpen.
	srv := NewServer(ServerConfig{})
	path := startServer(t, srv)
	c := NewClient(ClientConfig{SocketPath: path, Timeout: time.Second})
	d := c.Decide(context.Background(), toolCall("Bash", client.ToolShell, nil))
	if !d.FailOpen {
		t.Fatalf("cold start obtained no real verdict → want FailOpen: %+v", d)
	}
	if d.Source != sourceFailOpenNoBundle {
		t.Fatalf("cold start source=%q, want %s (server-side degrade)", d.Source, sourceFailOpenNoBundle)
	}
	if d.Evaluation.Verdict != client.VerdictUnknown {
		t.Fatalf("cold start verdict=%q, want UNKNOWN (honest: not evaluated)", d.Evaluation.Verdict)
	}
	if d.Evaluation.WouldBlock() {
		t.Fatal("cold start must never block at the server (fail-open primitive; the failure policy decides)")
	}
}

func TestIsRealVerdictSource(t *testing.T) {
	// Only a resident-evaluator verdict is a real verdict; every degrade / unknown
	// source routes to the failure policy (FailOpen). Guards the E6-S7 mapping.
	cases := map[string]bool{
		sourceLocalBundle:      true,
		sourceFailOpenNoBundle: false,
		sourceFailOpenClient:   false,
		"":                     false, // a stale/foreign peer with no source → safe direction
		"something-new":        false, // an unrecognized source is NOT assumed to be a real verdict
	}
	for src, want := range cases {
		if got := isRealVerdictSource(src); got != want {
			t.Errorf("isRealVerdictSource(%q) = %v, want %v", src, got, want)
		}
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
