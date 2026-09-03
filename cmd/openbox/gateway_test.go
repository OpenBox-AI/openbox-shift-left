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

// TestTheDaemonSubcommandsDispatchButAreNotAdvertised. These are invoked by
// string, by the supervisor units this tool writes and by nothing a person
// types: leaving them out of the usage text is the whole point of the reduced
// surface, and breaking their dispatch would stop every lane daemon from
// starting. So the two halves are asserted together, because satisfying one by
// breaking the other is the easy mistake.
func TestTheDaemonSubcommandsDispatchButAreNotAdvertised(t *testing.T) {
	a, _, errb := testApp(nil)
	a.usage()
	usage := errb.String()
	for _, cmd := range []string{"openbox gateway", "openbox telemetry", "openbox transport",
		"openbox hook", "openbox rewake"} {
		if strings.Contains(usage, cmd) {
			t.Errorf("usage advertises %q; it is invoked by unit argv, not by a person:\n%s", cmd, usage)
		}
	}
	for _, want := range []string{"openbox auth", "openbox init", "openbox doctor",
		"openbox uninstall", "openbox version"} {
		if !strings.Contains(usage, want) {
			t.Errorf("usage does not list %q:\n%s", want, usage)
		}
	}
	// And each one still dispatches. The caller is a lane unit's argv or a hook
	// command string, never a person -- so a deleted case arm would stop every
	// daemon and every governance hook with no error anywhere.
	//
	// These reach the real daemon entrypoints, which resolve ~/.openbox and mint
	// the transport CA there before they get as far as refusing the address --
	// so the home is isolated first.
	isolateHome(t)
	for _, args := range [][]string{
		{"gateway", "--addr", "0.0.0.0:8788"},
		{"telemetry", "--addr", "0.0.0.0:8789"},
		{"transport", "--addr", "0.0.0.0:8790"},
	} {
		b, _, berr := testApp(nil)
		if code := b.run(args); code != exitError {
			t.Errorf("%v exited %d; expected the loopback refusal, which proves it dispatched", args, code)
		}
		if strings.Contains(berr.String(), "unknown command") {
			t.Errorf("%q is not wired into the dispatcher", args[0])
		}
	}

	// `hook` and `rewake` cannot be probed by a refusal: both are required to
	// exit 0 on every path, because a non-zero hook is a blocked tool call. So
	// they are probed by what they must NOT say.
	for _, args := range [][]string{
		{"hook", "claude-code", "SessionStart"},
		{"rewake", "claude-code"},
	} {
		b, _, berr := testApp(nil)
		b.stdin = strings.NewReader("{}")
		if code := b.run(args); code != exitOK {
			t.Errorf("%v exited %d; a hook path must exit 0 on every path", args, code)
		}
		if strings.Contains(berr.String(), "unknown command") {
			t.Errorf("%q is not wired into the dispatcher", args[0])
		}
	}
}
