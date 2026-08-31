package main

import (
	"net"
	"testing"
	"testing/synctest"
	"time"
)

// TestWaitForListenerGivesUpAtItsDeadline. Every existing test replaces
// waitForListenerFn with a constant, so the real function -- the one that
// decides how long `openbox init` stares at a gateway that never came up --
// has no coverage at all. Exercising it for real costs gatewayReadyTimeout of
// wall clock, which is why it has none.
//
// Inside a bubble the polling sleeps run on the fake clock while the dials
// stay real: connecting to a refused port returns immediately and never parks
// the goroutine, so the loop runs to its deadline in microseconds and the
// deadline is assertable to the nanosecond.
func TestWaitForListenerGivesUpAtItsDeadline(t *testing.T) {
	const dead = "127.0.0.1:1" // nothing listens on port 1 for an unprivileged user
	if c, err := net.DialTimeout("tcp", dead, 500*time.Millisecond); err == nil {
		c.Close()
		t.Skipf("something is listening on %s; this test needs a refused port", dead)
	}

	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		if waitForListener(dead, gatewayReadyTimeout) {
			t.Fatalf("waitForListener reported %s ready", dead)
		}
		if d := time.Since(start); d != gatewayReadyTimeout {
			t.Errorf("gave up after %v, want exactly %v. Install ordering depends on this bound: the env "+
				"var is written only after the listener answers, and a wait that ends early points the "+
				"tool at a dead port while init prints success", d, gatewayReadyTimeout)
		}
	})
}

// TestWaitForPortFreeGivesUpAtItsDeadline is the mirror bound, used when init
// tears a lane down before rebuilding it. A free port must return true at
// once rather than after a poll interval.
func TestWaitForPortFreeGivesUpAtItsDeadline(t *testing.T) {
	const free = "127.0.0.1:1"
	if c, err := net.DialTimeout("tcp", free, 500*time.Millisecond); err == nil {
		c.Close()
		t.Skipf("something is listening on %s; this test needs a free port", free)
	}

	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		if !waitForPortFree(free, gatewayReadyTimeout) {
			t.Fatalf("waitForPortFree reported %s still occupied", free)
		}
		if d := time.Since(start); d != 0 {
			t.Errorf("waited %v for a port that was already free, want 0", d)
		}
	})
}
