package git

import (
	"os"
	"testing"
)

// Ambient agent context must not reach the tests.
//
// A developer (or CI job) running this suite from inside a live Codex session
// has CODEX_THREAD_ID exported; Resolve reads it at Tier-0 and the explicit
// override vars at Tier-1 (session.go), so any test that does not inject
// Getenv would resolve the developer's real session instead of its own
// fixture. The subprocess tests inherit os.Environ(), so the same vars reach
// the hook child. Report SL-11: with CODEX_THREAD_ID set, 14 tests here and
// one in cli fail in ways that read as product bugs.
//
// Scrubbing once in TestMain fixes both paths — unset here means absent from
// os.Environ() too, so the child processes are clean without every call site
// having to build a filtered environment.
var ambientSessionEnv = []string{
	EnvCodexThreadID,
	EnvSession,
	EnvSessionFile,
	EnvSessionTTL,
}

// scrubAmbientSessionEnv clears the ambient session vars and points the
// registry at a throwaway dir, so a test that forgets an explicit SessionDir
// cannot read or write the developer's real ~/.config registry. Returns the
// cleanup for the temp dir (TestMain calls os.Exit, so it cannot be deferred).
func scrubAmbientSessionEnv() func() {
	for _, k := range ambientSessionEnv {
		os.Unsetenv(k)
	}
	dir, err := os.MkdirTemp("", "obgit-registry")
	if err != nil {
		panic("scrub registry dir: " + err.Error())
	}
	os.Setenv(EnvSessionDir, dir)
	return func() { os.RemoveAll(dir) }
}

// TestHarness_NoAmbientSessionEnv locks the scrub in. Without it the failures
// land in unrelated tests with misleading messages; this one names the cause.
func TestHarness_NoAmbientSessionEnv(t *testing.T) {
	for _, k := range ambientSessionEnv {
		if v := os.Getenv(k); v != "" {
			t.Errorf("%s=%q leaked into the test process: TestMain must call scrubAmbientSessionEnv "+
				"or ambient agent context contaminates session resolution (E8-S1 / report SL-11)", k, v)
		}
	}
	if os.Getenv(EnvSessionDir) == "" {
		t.Errorf("%s unset: the scrub must point the registry at a throwaway dir so tests "+
			"never touch the developer's real registry", EnvSessionDir)
	}
}
