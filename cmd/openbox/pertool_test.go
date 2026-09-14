package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	obgit "github.com/openbox-ai/openbox-shift-left/internal/adapters/common/git"
	"github.com/openbox-ai/openbox-shift-left/internal/provider"
)

// perToolHookEnv is setHookEnv's isolation without the two things that would
// make a per-tool assertion vacuous: an exported DID (which outranks every
// store) and an OPENBOX_CONFIG pin (which points every store at one file).
func perToolHookEnv(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	spool := filepath.Join(dir, "spool")
	t.Setenv(devconfig.EnvDID, "")
	t.Setenv("OPENBOX_SPOOL_DIR", spool)
	t.Setenv("OPENBOX_SESSION_DIR", filepath.Join(dir, "sessions"))
	t.Setenv(devconfig.EnvEnforcementFile, filepath.Join(dir, "enforcements.jsonl"))
	t.Setenv(devconfig.EnvPendingApprovalDir, filepath.Join(dir, "pending-approvals"))
	t.Setenv("OPENBOX_ADVISORY_FILE", filepath.Join(dir, "advisories.jsonl"))
	t.Setenv(devconfig.EnvContentCapture, "0")
	return spool
}

// TestTwoToolsTwoDIDs is what the whole per-tool split is for: two governed
// tools on one machine, each spooling under its own agent identity. It is also
// the one case a shared-store bug cannot survive, because the two DIDs would
// have to be the same value.
func TestTwoToolsTwoDIDs(t *testing.T) {
	home := isolateHomeOnly(t)
	// A pin here would put both tools' dev.json at one path and this case would
	// pass against the override while proving nothing. Assert it rather than
	// trust the fixture, so a later fixture change fails here instead of going
	// quiet.
	if got := os.Getenv(devconfig.EnvConfigPath); got != "" {
		t.Fatalf("OPENBOX_CONFIG = %q; this case is vacuous under the pin", got)
	}
	perToolHookEnv(t)
	seedCredentials(t, "claude-code", "codex")

	for _, tool := range []string{"claude-code", "codex"} {
		spool := filepath.Join(t.TempDir(), "spool")
		t.Setenv("OPENBOX_SPOOL_DIR", spool)

		a, out, errb := testApp(nil)
		a.stdin = strings.NewReader(`{"hook_event_name":"SessionStart","session_id":"s-` + tool + `","cwd":"/r"}`)
		if code := a.run([]string{"hook", tool, "SessionStart"}); code != exitOK {
			t.Fatalf("hook %s exit = %d; stderr=%q", tool, code, errb.String())
		}
		if out.Len() != 0 {
			t.Fatalf("hook %s stdout must be empty, got %q", tool, out.String())
		}

		raw, err := os.ReadFile(filepath.Join(spool, onlySpoolFile(t, spool)))
		if err != nil {
			t.Fatalf("read spool for %s: %v", tool, err)
		}
		want := testDIDFor(t, tool)
		if !strings.Contains(string(raw), want) {
			t.Errorf("the %s event does not carry %s's own DID %s:\n%s", tool, tool, want, raw)
		}
		for _, other := range []string{"claude-code", "codex"} {
			if other == tool {
				continue
			}
			if strings.Contains(string(raw), testDIDFor(t, other)) {
				t.Errorf("the %s event carries %s's DID; the two stores are not separate:\n%s", tool, other, raw)
			}
		}
	}

	// And the stores really are two files, not one file read twice.
	for _, tool := range []string{"claude-code", "codex"} {
		if _, err := os.Stat(filepath.Join(home, tool, ".env")); err != nil {
			t.Errorf("no credential file for %s: %v", tool, err)
		}
	}
}

// gatedReader blocks its first Read until released, which is how a test
// observes process state while a command is still inside it. Reading
// BoundProvider() after the command returns would see the restored binding and
// prove nothing.
type gatedReader struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	sent    bool
	payload string
}

func newGatedReader(payload string) *gatedReader {
	return &gatedReader{entered: make(chan struct{}), release: make(chan struct{}), payload: payload}
}

func (g *gatedReader) Read(p []byte) (int, error) {
	g.once.Do(func() { close(g.entered) })
	<-g.release
	if g.sent {
		return 0, io.EOF
	}
	g.sent = true
	return copy(p, g.payload), nil
}

// TestRewakeBindsItsProvider the approval watcher runs as its own process with
// the provider on argv, and it takes a config Pin as its first act. Binding has
// to happen before that Pin -- a Pin freezes the resolved config across a path
// change, so a bind inside one would be read through the previous tool's
// snapshot.
func TestRewakeBindsItsProvider(t *testing.T) {
	isolateHomeOnly(t)
	perToolHookEnv(t)
	seedCredentials(t, "claude-code")
	// Bind something else first, so "claude-code is bound" cannot be the
	// fixture's own doing.
	bindProviderForTest(t, "codex")

	// Unparseable on purpose: the watcher returns as soon as it reads, so the
	// case never depends on the control plane being reachable.
	stdin := newGatedReader("not a hook event")
	a, _, errb := testApp(nil)
	a.stdin = stdin

	done := make(chan int, 1)
	go func() { done <- a.run([]string{"rewake", "claude-code"}) }()

	select {
	case <-stdin.entered:
	case code := <-done:
		t.Fatalf("rewake returned %d before reading its payload; stderr=%q", code, errb.String())
	case <-time.After(10 * time.Second):
		t.Fatalf("rewake never reached its payload read; stderr=%q", errb.String())
	}
	if got := devconfig.BoundProvider(); got != "claude-code" {
		t.Errorf("rewake claude-code runs with %q bound; it must act for the provider on its argv", got)
	}

	close(stdin.release)
	select {
	case code := <-done:
		if code != exitOK && code != 2 {
			t.Fatalf("rewake exit = %d; stderr=%q", code, errb.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("rewake did not return after its payload")
	}
	if got := devconfig.BoundProvider(); got != "codex" {
		t.Errorf("rewake left %q bound after returning, want the restored codex", got)
	}
}

// TestHookWithNoIdentityWarnsAndContinues (owner ruling V3.) Under the
// no-legacy-fallback rule an upgraded machine has no per-tool store until
// `init` runs again, and the first thing the developer would see is governance
// quietly doing nothing. One line naming the fix is the whole remedy -- and it
// must not block the tool call, because a hook that failed closed on a missing
// credential would stop the developer working.
func TestHookWithNoIdentityWarnsAndContinues(t *testing.T) {
	isolateHomeOnly(t)
	spool := perToolHookEnv(t)
	// Deliberately no store for either tool.

	a, out, errb := testApp(nil)
	a.stdin = strings.NewReader(`{"hook_event_name":"PreToolUse","session_id":"s1","cwd":"/r","tool_name":"Bash","tool_input":{"command":"ls"}}`)
	code := a.run([]string{"hook", "claude-code", "PreToolUse"})

	if code != exitOK {
		t.Fatalf("hook exit = %d, want 0: a missing identity must never block a tool call", code)
	}
	if out.Len() != 0 {
		t.Fatalf("stdout must stay empty, got %q", out.String())
	}
	got := errb.String()
	if !strings.Contains(got, "init --provider claude-code") {
		t.Errorf("the warning does not name the fix; stderr was:\n%s", got)
	}
	if n := strings.Count(got, "init --provider"); n != 1 {
		t.Errorf("want exactly one warning line, got %d:\n%s", n, got)
	}
	// It is a developer-facing notice, not an event: nothing about it may reach
	// the spool.
	entries, err := os.ReadDir(spool)
	if err == nil {
		for _, e := range entries {
			raw, _ := os.ReadFile(filepath.Join(spool, e.Name()))
			if strings.Contains(string(raw), "init --provider") {
				t.Errorf("the warning reached the spool:\n%s", raw)
			}
		}
	}
}

// TestHookWithAnIdentityDoesNotWarn the other half: a machine that is set up
// must not see the notice on every tool call.
func TestHookWithAnIdentityDoesNotWarn(t *testing.T) {
	isolateHomeOnly(t)
	perToolHookEnv(t)
	seedCredentials(t)

	a, _, errb := testApp(nil)
	a.stdin = strings.NewReader(`{"hook_event_name":"PreToolUse","session_id":"s1","cwd":"/r","tool_name":"Bash","tool_input":{"command":"ls"}}`)
	if code := a.run([]string{"hook", "claude-code", "PreToolUse"}); code != exitOK {
		t.Fatalf("hook exit = %d; stderr=%q", code, errb.String())
	}
	if strings.Contains(errb.String(), "init --provider") {
		t.Errorf("a configured machine was told to run init:\n%s", errb.String())
	}
}

// TestAttestProviderMarkerRule decides which private key signs a commit
// attestation, so a wrong answer mis-attributes authorship. There is
// deliberately no "otherwise claude-code" arm: a human commit from a plain
// shell would then sign as the tool.
func TestAttestProviderMarkerRule(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
		want string
	}{
		{"codex thread", map[string]string{obgit.EnvCodexThreadID: "th-1"}, "codex"},
		{"claude code", map[string]string{"CLAUDECODE": "1"}, "claude-code"},
		{"claude code entrypoint", map[string]string{"CLAUDE_CODE_ENTRYPOINT": "cli"}, "claude-code"},
		{"neither", map[string]string{}, ""},
		{"empty values are not markers", map[string]string{obgit.EnvCodexThreadID: "", "CLAUDECODE": ""}, ""},
		{"codex wins", map[string]string{obgit.EnvCodexThreadID: "th-1", "CLAUDECODE": "1"}, "codex"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := tc.env
			if got := attestProvider(func(k string) string { return env[k] }); got != tc.want {
				t.Fatalf("attestProvider = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestAttestBindsFromToolMarkers the rule above, through the real hook: the
// tool whose marker is set is the tool whose signing key ends up on the
// attestation, and no marker means no attestation rather than a guess.
func TestAttestBindsFromToolMarkers(t *testing.T) {
	if testing.Short() {
		t.Skip("drives a real git repo; skipped in -short")
	}
	for _, tc := range []struct {
		name   string
		marker map[string]string
		want   string
	}{
		{"codex", map[string]string{obgit.EnvCodexThreadID: "th-1"}, "codex"},
		{"claude-code", map[string]string{"CLAUDECODE": "1"}, "claude-code"},
		{"no marker", map[string]string{}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Unbound for the no-marker case: a commit hook starts life not
			// knowing which tool it is acting for, and a fixture that bound one
			// would answer the question this case exists to ask.
			if tc.want == "" {
				isolateHomeUnbound(t)
			} else {
				isolateHomeOnly(t)
			}
			perToolHookEnv(t)
			seedCredentials(t, "claude-code", "codex")
			repo := gitRepoWithACommit(t)

			a, _, errb := testApp(tc.marker)
			if code := a.run([]string{"hook", "git", "post-commit"}); code != exitOK {
				t.Fatalf("hook git post-commit exit = %d; stderr=%q", code, errb.String())
			}

			note := attestationNote(t, repo)
			if tc.want == "" {
				if note != "" {
					t.Fatalf("an unmarked commit was attested anyway:\n%s", note)
				}
				if !strings.Contains(errb.String(), "attestation skipped") {
					t.Errorf("no attestation and no explanation; stderr was:\n%s", errb.String())
				}
				return
			}
			var att struct {
				DID string `json:"did"`
			}
			if err := json.Unmarshal([]byte(note), &att); err != nil {
				t.Fatalf("parse attestation note %q: %v", note, err)
			}
			if want := testDIDFor(t, tc.want); att.DID != want {
				t.Fatalf("attestation signed as %q, want %s's DID %q", att.DID, tc.want, want)
			}
		})
	}
}

// gitRepoWithACommit builds a repo with one session-attributed commit and
// leaves the process inside it, which is how a post-commit hook runs.
func gitRepoWithACommit(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		c := exec.Command("git", append([]string{"-C", repo}, args...)...)
		c.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")
	git("config", "user.email", "t@example.com")
	git("config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-q", "-m", "subject")
	// The post-commit hook attributes from the session resolver, not the
	// trailer; the override tier is the one that works without a registry.
	t.Setenv("OPENBOX_SESSION", "sess-attest")

	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(repo); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })
	return repo
}

func attestationNote(t *testing.T, repo string) string {
	t.Helper()
	c := exec.Command("git", "-C", repo, "notes", "--ref", "refs/notes/openbox-attest", "show", "HEAD")
	c.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
	out, err := c.CombinedOutput()
	if err != nil {
		return "" // no note on this commit
	}
	return string(out)
}

// TestGatewayLaneBindsItsOwnTool a lane daemon takes its provider from a
// compile-time constant rather than argv, so it is the one bind site the
// environment cannot redirect -- and the one that would silently stamp another
// tool's identity onto every model call if it were missed.
func TestGatewayLaneBindsItsOwnTool(t *testing.T) {
	isolateHomeOnly(t)
	t.Setenv(devconfig.EnvDID, "")
	t.Setenv("OPENBOX_REALTIME", "0")
	t.Setenv("OPENBOX_SPOOL_DIR", t.TempDir())
	seedCredentials(t, "claude-code", "codex")
	// Bind the other tool first, so the assertion below cannot be satisfied by
	// the fixture's own binding.
	bindProviderForTest(t, "codex")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan net.Addr, 1)
	done := make(chan int, 1)

	a, _, errb := testApp(nil)
	a.gatewayCtx = ctx
	a.gatewayReady = func(addr net.Addr) { ready <- addr }
	go func() {
		done <- a.runGateway([]string{"--addr", "127.0.0.1:0", "--upstream", "https://upstream.invalid", "--elected"})
	}()

	select {
	case <-ready:
	case <-time.After(10 * time.Second):
		t.Fatalf("gateway never bound a listener; stderr: %s", errb.String())
	}
	if got := devconfig.BoundProvider(); got != gatewaySpoolProvider {
		t.Errorf("the running gateway has %q bound, want its own constant %q", got, gatewaySpoolProvider)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("runGateway did not return after its context ended")
	}
	if got := devconfig.BoundProvider(); got != "codex" {
		t.Errorf("the lane left %q bound after returning; a daemon binds for its own run, not the process's life", got)
	}
}

// TestIdentityDirsComeFromSupported the per-tool layout has exactly the tools
// the provider registry has: no legacy root a read could fall back to, and no
// hardcoded name a third adapter would have to hunt down.
func TestIdentityDirsComeFromSupported(t *testing.T) {
	home := isolateHomeOnly(t)
	supported := provider.Supported()
	if len(supported) == 0 {
		t.Fatal("provider.Supported() is empty; every assertion below would pass vacuously")
	}
	for _, name := range supported {
		release, err := devconfig.BindProvider(name)
		if err != nil {
			t.Fatalf("BindProvider(%s) rejected a supported provider: %v", name, err)
		}
		env, err := devconfig.EnvFilePath()
		if err != nil {
			t.Fatalf("EnvFilePath() bound to %s: %v", name, err)
		}
		cfg, err := devconfig.DevConfigWritePath()
		if err != nil {
			t.Fatalf("DevConfigWritePath() bound to %s: %v", name, err)
		}
		release()

		if want := filepath.Join(home, name, ".env"); env != want {
			t.Errorf("%s credential file = %q, want %q", name, env, want)
		}
		if want := filepath.Join(home, name, "dev.json"); cfg != want {
			t.Errorf("%s dev config = %q, want %q", name, cfg, want)
		}
	}
}

// TestGatewayLaneStampsTheFileDID closes the gap the bind test above cannot
// reach. Every other lane capture test exports OPENBOX_AGENT_DID, which
// outranks every store -- so until this case there was nothing anywhere
// proving a relayed model call carries the DID from the tool's own file rather
// than one the test handed it. Two stores are seeded distinctly, neither is
// exported, and the record has to carry the lane's own tool's DID.
func TestGatewayLaneStampsTheFileDID(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Request-Id", "req_file_did")
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"type":"message","role":"assistant","content":[]}`)
	}))
	defer upstream.Close()

	isolateHomeOnly(t)
	spool := t.TempDir()
	t.Setenv("OPENBOX_SPOOL_DIR", spool)
	t.Setenv("OPENBOX_REALTIME", "0")
	// The whole point: no exported DID, so the file is the only source.
	t.Setenv(devconfig.EnvDID, "")
	seedCredentials(t, "claude-code", "codex")
	// Bind the other tool first, so the claude-code DID below cannot be the
	// fixture's own doing.
	bindProviderForTest(t, "codex")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan net.Addr, 1)
	done := make(chan int, 1)
	a, _, errb := testApp(nil)
	a.gatewayCtx = ctx
	a.gatewayReady = func(addr net.Addr) { ready <- addr }
	go func() {
		done <- a.runGateway([]string{"--addr", "127.0.0.1:0", "--upstream", upstream.URL, "--elected"})
	}()

	var addr net.Addr
	select {
	case addr = <-ready:
	case <-time.After(10 * time.Second):
		t.Fatalf("gateway never bound a listener; stderr: %s", errb.String())
	}

	req, _ := http.NewRequest(http.MethodPost, "http://"+addr.String()+"/v1/messages",
		strings.NewReader(`{"model":"claude-opus-4"}`))
	req.Header.Set("X-Claude-Code-Session-Id", "file-did-session")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("relay: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("runGateway did not return")
	}

	raw := readSpoolTree(t, spool)
	if raw == "" {
		t.Fatal("the relayed call spooled nothing")
	}
	if want := testDIDFor(t, gatewaySpoolProvider); !strings.Contains(raw, want) {
		t.Errorf("the spooled record does not carry %s's file DID %s:\n%s", gatewaySpoolProvider, want, raw)
	}
	if other := testDIDFor(t, "codex"); strings.Contains(raw, other) {
		t.Errorf("the gateway lane stamped codex's DID:\n%s", raw)
	}
}

// readSpoolTree concatenates every spooled line under dir; the lane picks its
// own subdirectory, so naming one here would couple this case to that choice.
func readSpoolTree(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		b.Write(raw)
		return nil
	})
	if err != nil {
		t.Fatalf("walk spool: %v", err)
	}
	return b.String()
}
