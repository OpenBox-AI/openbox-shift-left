package git

import (
	"os"
	"testing"
)

var ambientSessionEnv = []string{
	EnvCodexThreadID,
	EnvSession,
	EnvSessionFile,
	EnvSessionTTL,
}

// scrubAmbientSessionEnv clears the ambient session vars and points the
// registry at a throwaway dir, so a test that forgets an explicit SessionDir
// cannot read or write the developer's real ~/.config registry.
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

// TestHarness_NoAmbientSessionEnv locks the scrub in.
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
