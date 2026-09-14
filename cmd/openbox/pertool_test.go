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

// TestDoctorListsEveryPerToolIdentity doctor is the in-product answer to "why
// did governance stop", and under the no-fallback rule the answer is usually
// "this tool has no store". One row per tool, each naming its own file and its
// own DID.
//
// The two DIDs differing is the load-bearing assertion: a bind held across the
// loop, or a config pin frozen on the first read, would report the first
// tool's identity for every row and look entirely reasonable.
func TestDoctorListsEveryPerToolIdentity(t *testing.T) {
	home := isolateHomeUnbound(t)
	t.Setenv(devconfig.EnvDID, "")
	t.Setenv(devconfig.EnvAPIKeyDirect, "")
	t.Setenv(devconfig.EnvAgentPrivateKey, "")
	t.Setenv(envManagedSettingsPath, filepath.Join(t.TempDir(), "absent.json"))
	seedToolCredentials(t, "claude-code", testDIDFor(t, "claude-code"))
	seedToolCredentials(t, "codex", testDIDFor(t, "codex"))

	out, code := runDoctorHere(t)
	if code != exitOK {
		t.Fatalf("doctor exit = %d:\n%s", code, out)
	}
	for _, tool := range []string{"claude-code", "codex"} {
		if !strings.Contains(out, filepath.Join(home, tool, "dev.json")) {
			t.Errorf("doctor does not name %s's own config:\n%s", tool, out)
		}
		if !strings.Contains(out, testDIDFor(t, tool)) {
			t.Errorf("doctor does not report %s's DID:\n%s", tool, out)
		}
	}
	// And the org row, which carries coordinates and no identity at all.
	if !strings.Contains(out, "org") {
		t.Errorf("doctor does not report the org-level config:\n%s", out)
	}
}

// TestDoctorNamesTheSourceOfTheIdentityInEffect an exported variable outranks
// every file doctor just printed, for every tool at once. Without this line a
// reader comparing a dashboard to a dev.json is comparing the wrong two
// things.
func TestDoctorNamesTheSourceOfTheIdentityInEffect(t *testing.T) {
	isolateHomeUnbound(t)
	t.Setenv(envManagedSettingsPath, filepath.Join(t.TempDir(), "absent.json"))
	seedToolCredentials(t, "claude-code", testDIDFor(t, "claude-code"))

	t.Run("a file answers", func(t *testing.T) {
		t.Setenv(devconfig.EnvDID, "")
		out, _ := runDoctorHere(t)
		if !strings.Contains(out, "each tool's own files") {
			t.Errorf("doctor does not say the files are in effect:\n%s", out)
		}
	})

	t.Run("an env var shadows every file", func(t *testing.T) {
		const exported = "did:aip:99999999-9999-9999-9999-999999999999"
		t.Setenv(devconfig.EnvDID, exported)
		out, _ := runDoctorHere(t)
		if !strings.Contains(out, devconfig.EnvDID) || !strings.Contains(out, "environment") {
			t.Errorf("doctor does not name the variable that wins:\n%s", out)
		}
		if !strings.Contains(out, exported) {
			t.Errorf("doctor does not report the DID actually in effect:\n%s", out)
		}
	})
}

// TestDoctorReportsPerStoreReachability one store working and one absent is a
// normal machine, and a report that stopped at the first would hide whichever
// one is broken.
func TestDoctorReportsPerStoreReachability(t *testing.T) {
	isolateHomeUnbound(t)
	t.Setenv(devconfig.EnvDID, "")
	t.Setenv(devconfig.EnvAPIKeyDirect, "")
	t.Setenv(devconfig.EnvAgentPrivateKey, "")
	t.Setenv("OPENBOX_ED25519_SEED", "")
	t.Setenv(envManagedSettingsPath, filepath.Join(t.TempDir(), "absent.json"))
	// Only claude-code exists.
	seedToolCredentials(t, "claude-code", testDIDFor(t, "claude-code"))

	out, code := runDoctorHere(t)
	if code != exitOK {
		t.Fatalf("doctor exit = %d:\n%s", code, out)
	}
	if !strings.Contains(out, "codex") || !strings.Contains(out, "NOT CHECKED") {
		t.Errorf("doctor does not report the absent codex store:\n%s", out)
	}
	if !strings.Contains(out, "openbox init --provider codex") {
		t.Errorf("the absent store does not name its remedy:\n%s", out)
	}
	// The absent one did not suppress the present one.
	if !strings.Contains(out, testDIDFor(t, "claude-code")) {
		t.Errorf("an absent store suppressed the store that exists:\n%s", out)
	}
}

// TestUninstallRemovesEveryPerToolCredential `uninstall` promises it reverses
// all of it, "including the credentials". With identity per tool that is three
// files on a two-tool machine, and a command that deleted one would leave two
// live signing seeds behind a report of success.
func TestUninstallRemovesEveryPerToolCredential(t *testing.T) {
	home := isolateHomeUnbound(t)
	seedToolCredentials(t, "claude-code", testDIDFor(t, "claude-code"))
	seedToolCredentials(t, "codex", testDIDFor(t, "codex"))
	orgEnv, err := devconfig.OrgEnvFilePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := devconfig.WriteEnvFile(orgEnv, map[string]string{
		devconfig.EnvControlToken: "obx" + "_key_org"}); err != nil {
		t.Fatal(err)
	}

	a, out, errb := testApp(nil)
	if code := a.run([]string{"uninstall"}); code != exitOK {
		t.Fatalf("uninstall exit = %d; stderr=%q", code, errb.String())
	}

	for _, path := range []string{orgEnv,
		filepath.Join(home, "claude-code", ".env"),
		filepath.Join(home, "codex", ".env")} {
		if !strings.Contains(out.String(), path) {
			t.Errorf("the inventory does not list %s; the print is the only warning before deletion:\n%s", path, out.String())
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("%s survived uninstall (err=%v)", path, err)
		}
	}
	// And the emptied directories, so nothing reads as a configured tool.
	for _, tool := range []string{"claude-code", "codex"} {
		if _, err := os.Stat(filepath.Join(home, tool)); !os.IsNotExist(err) {
			t.Errorf("%s's identity directory survived (err=%v)", tool, err)
		}
	}
}

// TestFlushGatePassesWhenAnyStoreHasCredentials the flush delivers each tool's
// spool under that tool's own identity, so one usable store makes flushing
// worth attempting. Asking about a single store would print "flushing SKIPPED"
// and destroy a backlog that could have gone.
func TestFlushGatePassesWhenAnyStoreHasCredentials(t *testing.T) {
	isolateHomeUnbound(t)
	spool := t.TempDir()
	t.Setenv("OPENBOX_SPOOL_DIR", spool)
	if err := os.WriteFile(filepath.Join(spool, "pending.jsonl"),
		[]byte(`{"event_type":"ToolCall","session_id":"s1"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Credentials in codex's store only; claude-code has none.
	seedToolCredentials(t, "codex", testDIDFor(t, "codex"))

	a, out, errb := testApp(nil)
	if code := a.run([]string{"uninstall"}); code != exitOK {
		t.Fatalf("uninstall exit = %d; stderr=%q", code, errb.String())
	}
	if strings.Contains(out.String(), "flushing SKIPPED") {
		t.Errorf("the flush was skipped although a store has credentials:\n%s", out.String())
	}
}

// TestTheFlushGateStillRefusesOnNothing the other half: with no store
// anywhere, the command must say what it is about to destroy rather than
// pretend it delivered.
func TestTheFlushGateStillRefusesOnNothing(t *testing.T) {
	isolateHomeUnbound(t)
	clearAgentEnv(t)
	spool := t.TempDir()
	t.Setenv("OPENBOX_SPOOL_DIR", spool)
	if err := os.WriteFile(filepath.Join(spool, "pending.jsonl"),
		[]byte(`{"event_type":"ToolCall","session_id":"s1"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	a, out, errb := testApp(nil)
	if code := a.run([]string{"uninstall"}); code != exitOK {
		t.Fatalf("uninstall exit = %d; stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "flushing SKIPPED") {
		t.Errorf("with no credentials anywhere the flush must say so:\n%s", out.String())
	}
}

// TestAuthRunsUnbound `auth` is an org command: it has no provider on argv and
// binds nothing, which is exactly the shape the unbound gate refuses. Every
// other auth case runs under a fixture that binds, so without this one a stray
// EnvFilePath() left in auth.go would pass the whole suite and fail on the
// first real `openbox auth`.
func TestAuthRunsUnbound(t *testing.T) {
	home := isolateHomeUnbound(t)
	requireUnbound(t)

	a, _, errb := testApp(nil)
	scriptedAuth(t, a, "https://api.internal", "", testOrgToken)
	if code := a.run([]string{"auth"}); code != exitOK {
		t.Fatalf("auth exit = %d with nothing bound; stderr=%q", code, errb.String())
	}
	if got := devconfig.BoundProvider(); got != "" {
		t.Errorf("auth left %q bound; it acts for the organization, not a tool", got)
	}
	kv, err := devconfig.ParseEnvFile(filepath.Join(home, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	if kv[devconfig.EnvControlToken] != testOrgToken {
		t.Errorf("auth did not write the org token: %v", kv)
	}
}

// TestUninstallRunsUnbound the same guard for `uninstall`, which enumerates
// every store by name and must never resolve one through a bind.
func TestUninstallRunsUnbound(t *testing.T) {
	isolateHomeUnbound(t)
	requireUnbound(t)
	seedToolCredentials(t, "claude-code", testDIDFor(t, "claude-code"))

	a, _, errb := testApp(nil)
	if code := a.run([]string{"uninstall"}); code != exitOK {
		t.Fatalf("uninstall exit = %d with nothing bound; stderr=%q", code, errb.String())
	}
	if got := devconfig.BoundProvider(); got != "" {
		t.Errorf("uninstall left %q bound", got)
	}
}

// TestUninstallRemovesAnInterruptedCredentialWrite the credential write is
// atomic, and the way it is atomic leaves a hazard: WriteEnvFile writes the
// whole body into a 0600 `.env-*.tmp` beside the target and then renames. Kill
// the process in between -- a laptop lid, an OOM, a Ctrl-C -- and a readable
// copy of the API key and signing seed stays in the store directory under a
// name nothing inventories.
//
// `uninstall` then prints that the keys and seeds "cannot be re-retrieved"
// while one of them is still sitting there. That is the command's central
// promise, so the residue has to go with the file it was becoming.
func TestUninstallRemovesAnInterruptedCredentialWrite(t *testing.T) {
	home := isolateHomeUnbound(t)
	requireUnbound(t)
	seedToolCredentials(t, "claude-code", testDIDFor(t, "claude-code"))

	// Exactly what an interrupted WriteEnvFile leaves behind.
	residue := filepath.Join(home, "claude-code", ".env-1234567890.tmp")
	if err := os.WriteFile(residue, []byte("OPENBOX_API_KEY='${OPENBOX_REDACTED_SECRET_ASSIGNMENT}'\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	a, out, errb := testApp(nil)
	if code := a.run([]string{"uninstall"}); code != exitOK {
		t.Fatalf("uninstall exit = %d; stderr=%q", code, errb.String())
	}
	if _, err := os.Stat(residue); !os.IsNotExist(err) {
		t.Errorf("an interrupted credential write survived uninstall (err=%v)\n%s", err, out.String())
	}
	if _, err := os.Stat(filepath.Join(home, "claude-code")); !os.IsNotExist(err) {
		t.Errorf("the store directory survived because of the residue in it")
	}
}

// TestUninstallRemovesAnIdentityDirWithNoCredentialFile the dir sweep used to
// live inside the credential step, behind its "nothing to delete" guard, so a
// store holding only a dev.json was never reached. That state is reachable:
// adopt writes the config first on purpose, so an interrupted adopt leaves
// exactly this, and so does deleting a .env by hand to rotate a key.
func TestUninstallRemovesAnIdentityDirWithNoCredentialFile(t *testing.T) {
	home := isolateHomeUnbound(t)
	requireUnbound(t)
	// A config and no credential file ANYWHERE. With one present the credential
	// step runs and sweeps directories on its way out, which is why this case
	// has to be the machine that has none: that is the branch the sweep sits
	// behind.
	if err := devconfig.WriteConfig(filepath.Join(home, "claude-code", "dev.json"),
		devconfig.Update{DID: testDIDFor(t, "claude-code")}); err != nil {
		t.Fatal(err)
	}

	a, _, errb := testApp(nil)
	if code := a.run([]string{"uninstall"}); code != exitOK {
		t.Fatalf("uninstall exit = %d; stderr=%q", code, errb.String())
	}
	if _, err := os.Stat(filepath.Join(home, "claude-code")); !os.IsNotExist(err) {
		t.Errorf("a store holding only a config survived uninstall (err=%v)", err)
	}
}

// TestUninstallReportsAnIdentityDirItCouldNotClear the direction of error here
// is over-keep, so a directory holding something this run did not account for
// stays. What must not happen is it staying silently: every other residue this
// command cannot clear is named, and a store directory left behind after
// "Done" reads as a finished uninstall.
func TestUninstallReportsAnIdentityDirItCouldNotClear(t *testing.T) {
	home := isolateHomeUnbound(t)
	requireUnbound(t)
	seedToolCredentials(t, "claude-code", testDIDFor(t, "claude-code"))
	stranger := filepath.Join(home, "claude-code", "notes.txt")
	if err := os.WriteFile(stranger, []byte("not ours\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	a, out, errb := testApp(nil)
	if code := a.run([]string{"uninstall"}); code != exitOK {
		t.Fatalf("uninstall exit = %d; stderr=%q", code, errb.String())
	}
	if _, err := os.Stat(stranger); err != nil {
		t.Errorf("uninstall deleted a file it does not own: %v", err)
	}
	dir := filepath.Join(home, "claude-code")
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("the directory holding it was removed anyway: %v", err)
	}
	// Not a bare Contains: the inventory line for `<dir>/.env` carries the
	// directory path as a prefix, so that would pass without a word being said
	// about what was kept.
	var said bool
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.Contains(line, "kept") && strings.Contains(line, dir) {
			said = true
		}
	}
	if !said {
		t.Errorf("uninstall kept %s and did not say so:\n%s", dir, out.String())
	}
}

// TestDoctorReportsOrgToToolURLDrift `auth` writes the organization's URLs
// once and `init` copies them into each tool's own config, so a re-run of
// `auth` that corrects a URL has no effect on an already installed tool until
// that tool re-runs `init`. Nothing about that is visible: the tool keeps
// posting to the old core, which answers 401, and a 401 never spends a
// delivery attempt -- so the spool grows and no message anywhere names a URL.
//
// doctor is the only place both values are in hand at once.
func TestDoctorReportsOrgToToolURLDrift(t *testing.T) {
	home := isolateHomeUnbound(t)
	requireUnbound(t)
	t.Setenv(devconfig.EnvDID, "")
	t.Setenv(devconfig.EnvBaseURL, "")
	t.Setenv(devconfig.EnvBackendURL, "")
	t.Setenv(envManagedSettingsPath, filepath.Join(t.TempDir(), "absent.json"))

	if err := devconfig.WriteConfig(filepath.Join(home, "dev.json"), devconfig.Update{
		BackendURL: "https://api.corrected", BaseURL: "https://core.corrected"}); err != nil {
		t.Fatal(err)
	}
	if err := devconfig.WriteConfig(filepath.Join(home, "claude-code", "dev.json"), devconfig.Update{
		DID: testDIDFor(t, "claude-code"), BackendURL: "https://api.stale", BaseURL: "https://core.stale"}); err != nil {
		t.Fatal(err)
	}
	// codex agrees with the org, so only one row may be flagged.
	if err := devconfig.WriteConfig(filepath.Join(home, "codex", "dev.json"), devconfig.Update{
		DID: testDIDFor(t, "codex"), BackendURL: "https://api.corrected", BaseURL: "https://core.corrected"}); err != nil {
		t.Fatal(err)
	}

	out, code := runDoctorHere(t)
	if code != exitOK {
		t.Fatalf("doctor exit = %d:\n%s", code, out)
	}
	if !strings.Contains(out, "https://core.stale") || !strings.Contains(out, "https://core.corrected") {
		t.Errorf("doctor does not show both sides of the drift:\n%s", out)
	}
	if !strings.Contains(out, "openbox init --provider claude-code") {
		t.Errorf("doctor names the drift without the remedy:\n%s", out)
	}
	// A tool that matches must not be flagged, or the line becomes noise
	// everybody learns to skip.
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "codex") && strings.Contains(line, "differs from org") {
			t.Errorf("doctor flagged a tool whose URLs match the org:\n%s", line)
		}
	}
}

// TestUninstallRemovesAnInterruptedOrgTokenWrite the same hazard at the file
// that matters most. `auth` writes the organization control token through the
// same atomic path, so an interrupted run leaves a readable copy of it in
// ~/.openbox/ -- and that credential can create and rotate agents across the
// whole organization, which is a strictly larger blast radius than any one
// tool's seed.
//
// The org directory is Home() itself, which uninstall never removes, so the
// residue cannot be swept as a side effect of removing a directory. It has to
// be named.
func TestUninstallRemovesAnInterruptedOrgTokenWrite(t *testing.T) {
	home := isolateHomeUnbound(t)
	requireUnbound(t)
	seedToolCredentials(t, "claude-code", testDIDFor(t, "claude-code"))

	residue := filepath.Join(home, ".env-9876543210.tmp")
	if err := os.WriteFile(residue, []byte("OPENBOX_CONTROL_TOKEN='${OPENBOX_REDACTED_SECRET_ASSIGNMENT}'\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	a, out, errb := testApp(nil)
	if code := a.run([]string{"uninstall"}); code != exitOK {
		t.Fatalf("uninstall exit = %d; stderr=%q", code, errb.String())
	}
	if _, err := os.Stat(residue); !os.IsNotExist(err) {
		t.Errorf("an interrupted organization-token write survived uninstall (err=%v)\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), residue) {
		t.Errorf("the inventory did not list it before deleting it:\n%s", out.String())
	}
}
