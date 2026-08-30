package main

import (
	"os"
	"testing"
)

// ambientSessionEnv ambient agent context must not reach these tests.
var ambientSessionEnv = []string{
	"CODEX_THREAD_ID",
	"OPENBOX_SESSION",
	"OPENBOX_SESSION_FILE",
	"OPENBOX_SESSION_TTL",
	"OPENBOX_SESSION_DIR",
}

func scrubAmbientSessionEnv() {
	for _, k := range ambientSessionEnv {
		os.Unsetenv(k)
	}
}

// TestHarness_NoAmbientSessionEnv names the cause if the scrub is ever
// removed.
func TestHarness_NoAmbientSessionEnv(t *testing.T) {
	for _, k := range ambientSessionEnv {
		if v := os.Getenv(k); v != "" {
			t.Errorf("%s=%q leaked into the test process: TestMain must unset it or ambient "+
				"agent context contaminates session resolution (E8-S1 / report SL-11)", k, v)
		}
	}
}
