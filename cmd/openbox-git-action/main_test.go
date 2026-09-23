package main

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"

	"github.com/openbox-ai/openbox-shift-left/internal/client/fakecore"
	"github.com/openbox-ai/openbox-shift-left/internal/client/memhttptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	gitaction "github.com/openbox-ai/openbox-shift-left/internal/actions/openbox-git-action"
)

func TestMain(m *testing.M) {
	if _, err := exec.LookPath("git"); err != nil {
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func initRepo(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	run("config", "user.name", "Dev")
	run("config", "user.email", "dev@example.com")
	run("config", "commit.gpgsign", "false")
	msg := filepath.Join(dir, "m")
	if err := os.WriteFile(msg, []byte("ship\n\nOpenBox-Session: sess-A\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("commit", "--allow-empty", "--cleanup=verbatim", "-F", msg)
	sha := revParse(t, dir)
	return dir, sha
}

func revParse(t *testing.T, dir string) string {
	cmd := exec.Command("git", "-C", dir, "rev-parse", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

func TestCLI_DryRunResolvesWithoutCreds(t *testing.T) {
	dir, sha := initRepo(t)
	var out, errb bytes.Buffer
	code := run([]string{"--dry-run", "--dir", dir, "--sha", sha, "--repo", "o/r", "--environment", "staging"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit = %d, want 0\nstderr:\n%s", code, errb.String())
	}
	var payload struct {
		Resolution map[string]any `json:"resolution"`
		Event      map[string]any `json:"event"`
		Emitted    bool           `json:"emitted"`
	}
	if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
		t.Fatalf("dry-run output is not JSON: %v\n%s", err, out.String())
	}
	if payload.Emitted {
		t.Fatal("dry-run must not emit")
	}
	if payload.Resolution["commit_sha"] != sha {
		t.Fatalf("resolved commit_sha = %v, want %s", payload.Resolution["commit_sha"], sha)
	}
}

func TestCLI_MissingSHAisUsageError(t *testing.T) {
	t.Setenv("GITHUB_SHA", "")
	var out, errb bytes.Buffer
	code := run([]string{"--dry-run"}, &out, &errb)
	if code != 2 {
		t.Fatalf("exit = %d, want 2 (usage)", code)
	}
}

func discardLogger() *log.Logger { return log.New(bytes.NewBuffer(nil), "", 0) }

func TestSelectVerifier_FlagOffIsNoop(t *testing.T) {
	t.Setenv("OPENBOX_OWNERSHIP_VERIFY", "")
	t.Setenv("OPENBOX_OWNERSHIP_API_URL", "https://backend.example/")
	if _, ok := selectVerifier(false, discardLogger()).(gitaction.NoopVerifier); !ok {
		t.Fatal("flag off must select NoopVerifier")
	}
}

func TestSelectVerifier_DryRunNeverVerifies(t *testing.T) {
	t.Setenv("OPENBOX_OWNERSHIP_VERIFY", "1")
	t.Setenv("OPENBOX_OWNERSHIP_API_URL", "https://backend.example/")
	if _, ok := selectVerifier(true, discardLogger()).(gitaction.NoopVerifier); !ok {
		t.Fatal("dry-run must select NoopVerifier")
	}
}

func TestSelectVerifier_MisconfiguredDegradesToNoop(t *testing.T) {
	t.Setenv("OPENBOX_OWNERSHIP_VERIFY", "1")
	t.Setenv("OPENBOX_OWNERSHIP_API_URL", "")
	if _, ok := selectVerifier(false, discardLogger()).(gitaction.NoopVerifier); !ok {
		t.Fatal("a misconfigured verifier must degrade to NoopVerifier")
	}
}

// TestSelectVerifier_FlagOnBuildsRealVerifier the witness needs a real (fake)
// core to authenticate against: fakecore's own v3 identity is what its
// GET /api/v3/auth/validate names, so the configured OPENBOX_AGENT_ID must be
// fakecore's own agent id for the witness to match.
func TestSelectVerifier_FlagOnBuildsRealVerifier(t *testing.T) {
	fake := fakecore.New(t, fakecore.Script{})
	srv := memhttptest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()

	t.Setenv("OPENBOX_OWNERSHIP_VERIFY", "1")
	t.Setenv("OPENBOX_OWNERSHIP_API_URL", srv.URL) // loopback http allowed
	t.Setenv("OPENBOX_BASE_URL", fake.URL())
	t.Setenv("OPENBOX_API_KEY", fakecore.APIKey())
	t.Setenv("OPENBOX_WORKLOAD_PRIVATE_KEY", fakecore.WorkloadPrivateKey())
	t.Setenv("OPENBOX_AGENT_ID", fakecore.AgentID())
	t.Setenv("OPENBOX_ORG_API_KEY", "obx_key_test")

	v := selectVerifier(false, discardLogger())
	if _, ok := v.(gitaction.NoopVerifier); ok {
		t.Fatal("flag on with a usable config and a matching witness must build the real apiVerifier, not Noop")
	}
}

// TestSelectVerifier_WitnessMismatchDegradesToNoop an OPENBOX_AGENT_ID that
// names a different principal than the one the witnessed credential actually
// authenticates as must degrade to Noop, same as any other misconfiguration
// -- never build a verifier that would read another principal's sessions.
func TestSelectVerifier_WitnessMismatchDegradesToNoop(t *testing.T) {
	fake := fakecore.New(t, fakecore.Script{})
	srv := memhttptest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()

	t.Setenv("OPENBOX_OWNERSHIP_VERIFY", "1")
	t.Setenv("OPENBOX_OWNERSHIP_API_URL", srv.URL)
	t.Setenv("OPENBOX_BASE_URL", fake.URL())
	t.Setenv("OPENBOX_API_KEY", fakecore.APIKey())
	t.Setenv("OPENBOX_WORKLOAD_PRIVATE_KEY", fakecore.WorkloadPrivateKey())
	t.Setenv("OPENBOX_AGENT_ID", "22222222-2222-2222-2222-222222222222") // not fakecore's own agent id
	t.Setenv("OPENBOX_ORG_API_KEY", "obx_key_test")

	v := selectVerifier(false, discardLogger())
	if _, ok := v.(gitaction.NoopVerifier); !ok {
		t.Fatal("a witness naming a different agent than configured must degrade to NoopVerifier")
	}
}

func TestCLI_MissingCredsIsPreconditionError(t *testing.T) {
	dir, sha := initRepo(t)
	for _, k := range []string{
		"OPENBOX_BASE_URL", "OPENBOX_API_KEY", "OPENBOX_DID", "OPENBOX_SEED",
		"OPENBOX_AGENT_ID", "OPENBOX_WORKLOAD_PRIVATE_KEY",
	} {
		t.Setenv(k, "")
	}
	var out, errb bytes.Buffer
	code := run([]string{"--dir", dir, "--sha", sha}, &out, &errb)
	if code != 2 {
		t.Fatalf("exit = %d, want 2 (missing creds precondition)\nstderr:\n%s", code, errb.String())
	}
}

// TestGitActionReachesV3 the end-to-end proof: a v3 identity (API key +
// workload key) reaches fakecore's real v3 /evaluate route, carrying both
// v3 auth headers (the obx_ API key on Authorization, and the exchanged
// workload bearer), and no ownership verification (off by default) blocks
// the run.
func TestGitActionReachesV3(t *testing.T) {
	fake := fakecore.New(t, fakecore.Script{})
	dir, sha := initRepo(t)

	t.Setenv("OPENBOX_OWNERSHIP_VERIFY", "")
	t.Setenv("OPENBOX_BASE_URL", fake.URL())
	t.Setenv("OPENBOX_API_KEY", fakecore.APIKey())
	t.Setenv("OPENBOX_WORKLOAD_PRIVATE_KEY", fakecore.WorkloadPrivateKey())
	t.Setenv("OPENBOX_AGENT_ID", fakecore.AgentID())
	t.Setenv("OPENBOX_DID", fakecore.AttributionDID())

	var out, errb bytes.Buffer
	code := run([]string{"--dir", dir, "--sha", sha, "--repo", "o/r"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit = %d, want 0\nstderr:\n%s", code, errb.String())
	}

	inbox := fake.Inbox()
	if len(inbox) != 1 {
		t.Fatalf("fakecore inbox = %d entries, want 1", len(inbox))
	}
	h := inbox[0].Headers
	if h.Get("Authorization") == "" {
		t.Error("missing Authorization header (the obx_ API key)")
	}
	if h.Get("X-Openbox-Workload-Token") == "" {
		t.Error("missing X-Openbox-Workload-Token header (the exchanged workload bearer)")
	}
}

// TestGitActionRefusesMismatchedExportedDID a stale OPENBOX_DID left over
// from before a re-init (a new agent id, an old DID) must refuse to run
// rather than silently emit under the wrong attribution label.
func TestGitActionRefusesMismatchedExportedDID(t *testing.T) {
	fake := fakecore.New(t, fakecore.Script{})
	dir, sha := initRepo(t)

	t.Setenv("OPENBOX_OWNERSHIP_VERIFY", "")
	t.Setenv("OPENBOX_BASE_URL", fake.URL())
	t.Setenv("OPENBOX_API_KEY", fakecore.APIKey())
	t.Setenv("OPENBOX_WORKLOAD_PRIVATE_KEY", fakecore.WorkloadPrivateKey())
	t.Setenv("OPENBOX_AGENT_ID", fakecore.AgentID())
	t.Setenv("OPENBOX_DID", "did:aip:00000000-0000-0000-0000-000000000000") // stale/wrong

	var out, errb bytes.Buffer
	code := run([]string{"--dir", dir, "--sha", sha}, &out, &errb)
	if code != 2 {
		t.Fatalf("exit = %d, want 2 (mismatched OPENBOX_DID); stderr=%q", code, errb.String())
	}
	if len(fake.Inbox()) != 0 {
		t.Fatal("a mismatched OPENBOX_DID must refuse before ever emitting")
	}
}
