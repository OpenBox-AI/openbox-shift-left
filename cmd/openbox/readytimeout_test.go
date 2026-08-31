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

// TestWaitForPortFreeReturnsAtOnceWhenNothingHoldsThePort is the cheap half:
// init tears a lane down before rebuilding it, and a port already free must
// not cost a poll interval.
func TestWaitForPortFreeReturnsAtOnceWhenNothingHoldsThePort(t *testing.T) {
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

// TestWaitForPortFreeGivesUpAtItsDeadline is the half that actually gives up.
// A port somebody else is holding must be reported occupied after exactly the
// timeout, not sooner and not forever: init writes the env var only once the
// rebuild can proceed, and a wait that ends early points the tool at a port
// the old process still owns.
func TestWaitForPortFreeGivesUpAtItsDeadline(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("host denies bind (%v); this test needs a port something really holds", err)
	}
	defer lis.Close()
	held := lis.Addr().String()

	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		if waitForPortFree(held, gatewayReadyTimeout) {
			t.Fatalf("waitForPortFree reported %s free while a listener held it", held)
		}
		if d := time.Since(start); d != gatewayReadyTimeout {
			t.Errorf("gave up after %v, want exactly %v", d, gatewayReadyTimeout)
		}
	})
}
