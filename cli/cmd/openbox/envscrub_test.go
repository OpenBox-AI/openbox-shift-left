package main

import (
	"os"
	"testing"
)

// Ambient agent context must not reach these tests.
//
// The git-hook cases spawn git (which spawns our hook) with os.Environ(), and
// the hook resolves a session from CODEX_THREAD_ID at Tier-0 — so running the
// suite from inside a live Codex session stamps that session instead of the
// fixture's and the assertion fails on what looks like a product bug (report
// SL-11). Unsetting before m.Run keeps the vars out of every child env; the
// mirror of this scrub lives in adapters/common/git/envscrub_test.go.
//
// Note for anyone re-running the repro: `go test` caches results, and the env
// here is read by a spawned child rather than the test binary, so the cache
// cannot see the difference — use -count=1.
var ambientSessionEnv = []string{
	"CODEX_THREAD_ID",
	"OPENBOX_SESSION",
	"OPENBOX_SESSION_FILE",
	"OPENBOX_SESSION_TTL",
	"OPENBOX_SESSION_DIR",
}

func TestMain(m *testing.M) {
	for _, k := range ambientSessionEnv {
		os.Unsetenv(k)
	}
	os.Exit(m.Run())
}

// TestHarness_NoAmbientSessionEnv names the cause if the scrub is ever removed.
func TestHarness_NoAmbientSessionEnv(t *testing.T) {
	for _, k := range ambientSessionEnv {
		if v := os.Getenv(k); v != "" {
			t.Errorf("%s=%q leaked into the test process: TestMain must unset it or ambient "+
				"agent context contaminates session resolution (E8-S1 / report SL-11)", k, v)
		}
	}
}
