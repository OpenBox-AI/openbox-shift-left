package main

// TestGitActionDeploysOnV3 proves a workload agent's deploy event reaches
// core over the compiled binary, not just the in-process run() call
// TestGitActionReachesV3 (main_test.go) already exercises. This test compiles
// the real openbox-git-action binary and execs it as a child process, so it
// needs fakecore.NewReal (a real OS socket): memhttptest's in-process bufconn
// listener lives behind only this test binary's own http.DefaultTransport
// and a separate process cannot reach it.
//
// This lives in its own package/test binary deliberately, not beside
// cmd/openbox's other workload-identity evals: cmd/openbox already runs
// TestHookRealtimeDelivery, itself a real-socket, real-subprocess test. Two
// such tests sharing one test binary produced a reproducible data race
// (fakecore.Server.URL read/written without its mutex) when a stray request
// from one test's detached child process landed on the other test's
// freshly-bound, coincidentally-reused ephemeral port. Each `go test`
// invocation of a different package is its own OS process with its own port
// timeline, so keeping this test in cmd/openbox-git-action's binary rather
// than cmd/openbox's cannot collide with TestHookRealtimeDelivery that way.

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/client/fakecore"
)

func TestGitActionDeploysOnV3(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary; skipped in -short")
	}

	dir := t.TempDir()
	bin := filepath.Join(dir, "openbox-git-action")
	if out, err := exec.Command("go", "build", "-o", bin,
		"github.com/openbox-ai/openbox-shift-left/cmd/openbox-git-action").CombinedOutput(); err != nil {
		t.Fatalf("build openbox-git-action: %v\n%s", err, out)
	}

	repo, sha := initRepo(t)

	// NewReal, not New: this test execs a real, separately-built binary as a
	// child process (see the package doc comment above).
	fake := fakecore.NewReal(t, fakecore.Script{})

	cmd := exec.Command(bin, "--dir", repo, "--sha", sha, "--repo", "o/r")
	cmd.Env = append(os.Environ(),
		"OPENBOX_BASE_URL="+fake.URL(),
		"OPENBOX_API_KEY="+fakecore.APIKey(),
		"OPENBOX_WORKLOAD_PRIVATE_KEY="+fakecore.WorkloadPrivateKey(),
		"OPENBOX_AGENT_ID="+fakecore.AgentID(),
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("openbox-git-action exited non-zero: %v\nstdout: %s\nstderr: %s", err, stdout.String(), stderr.String())
	}

	inbox := fake.Inbox()
	if len(inbox) != 1 {
		t.Fatalf("fakecore inbox = %d entries, want 1\nstdout: %s\nstderr: %s", len(inbox), stdout.String(), stderr.String())
	}
	h := inbox[0].Headers
	if h.Get("Authorization") == "" {
		t.Error("missing Authorization header (the obx_ API key)")
	}
	if h.Get("X-Openbox-Workload-Token") == "" {
		t.Error("missing X-Openbox-Workload-Token header (the exchanged workload bearer)")
	}
	if strings.TrimSpace(stdout.String()) == "" {
		t.Error("the binary printed nothing to stdout on a successful, non-dry-run deploy")
	}
}
