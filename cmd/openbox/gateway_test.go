package main

import (
	"strings"
	"testing"
)

// TestGatewayRefusesNonLoopbackListener crosses the CLI seam. The gateway module
// has its own loopback test, but asserting the module is not asserting the
// command: a wiring that dropped Validate would expose the relay off-machine
// while every gateway-module test stayed green.
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

// TestGatewayIsReachableFromTheDispatcher pins the subcommand into `run`. A
// command that exists but is not dispatched is the same as no command.
func TestGatewayIsReachableFromTheDispatcher(t *testing.T) {
	a, _, errb := testApp(nil)
	// A non-loopback address makes this fail fast instead of serving forever,
	// while still proving the dispatcher routed to runGateway.
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

// TestUsageListsGateway keeps the command discoverable. `openbox doctor` and the
// phase 07 service wrapper both reference it, so an undocumented subcommand is a
// support problem rather than a cosmetic one.
func TestUsageListsGateway(t *testing.T) {
	a, _, errb := testApp(nil)
	a.usage()
	if !strings.Contains(errb.String(), "openbox gateway") {
		t.Error("usage does not list `openbox gateway`")
	}
}
