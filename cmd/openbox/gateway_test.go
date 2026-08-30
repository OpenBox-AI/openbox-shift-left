package main

import (
	"strings"
	"testing"
)

// TestGatewayRefusesNonLoopbackListener crosses the CLI seam.
func TestGatewayRefusesNonLoopbackListener(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:8788", ":8788", "10.0.0.5:8788"} {
		t.Run(addr, func(t *testing.T) {
			a, _, errb := testApp(nil)
			if code := a.runGateway([]string{"--addr", addr}); code != exitError {
				t.Fatalf("exit code: got %d want %d for listener %q", code, exitError, addr)
			}
			if !strings.Contains(errb.String(), "loopback") {
				t.Errorf("error does not name the reason: %q", errb.String())
			}
		})
	}
}

// TestGatewayRefusesRelativeUpstream keeps a misconfigured upstream from
// surfacing later as every request 502ing.
func TestGatewayRefusesRelativeUpstream(t *testing.T) {
	a, _, errb := testApp(nil)
	if code := a.runGateway([]string{"--upstream", "api.anthropic.com"}); code != exitError {
		t.Fatalf("exit code: got %d want %d", code, exitError)
	}
	if !strings.Contains(errb.String(), "absolute") {
		t.Errorf("error does not name the reason: %q", errb.String())
	}
}

// TestGatewayIsReachableFromTheDispatcher pins the subcommand into `run`.
func TestGatewayIsReachableFromTheDispatcher(t *testing.T) {
	a, _, errb := testApp(nil)
	if code := a.run([]string{"gateway", "--addr", "0.0.0.0:8788"}); code != exitError {
		t.Fatalf("exit code: got %d want %d", code, exitError)
	}
	if got := errb.String(); !strings.Contains(got, "loopback") {
		t.Errorf("dispatcher did not reach runGateway; stderr was %q", got)
	}
	if strings.Contains(errb.String(), "unknown command") {
		t.Error("`gateway` is not wired into the dispatcher")
	}
}

// TestUsageListsGateway keeps the command discoverable.
func TestUsageListsGateway(t *testing.T) {
	a, _, errb := testApp(nil)
	a.usage()
	if !strings.Contains(errb.String(), "openbox gateway") {
		t.Error("usage does not list `openbox gateway`")
	}
}
