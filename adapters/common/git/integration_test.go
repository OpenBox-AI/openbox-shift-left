package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// hookBin is the compiled cmd/openbox-git-hook, built once for the whole matrix.
var hookBin string

func TestMain(m *testing.M) {
	if _, err := exec.LookPath("git"); err != nil {
		// No git → the real-git matrix can't run; skip the whole package cleanly.
		os.Exit(0)
	}
	// Before anything reads the environment: drop the ambient agent context
	// (see envscrub_test.go) so neither the in-process resolver nor the hook
	// child inherits the session of whoever is running the suite.
	unscrub := scrubAmbientSessionEnv()
	dir, err := os.MkdirTemp("", "obgit-bin")
	if err != nil {
		panic(err)
	}
	hookBin = filepath.Join(dir, "openbox-git-hook")
	build := exec.Command("go", "build", "-o", hookBin, "./cmd/openbox-git-hook")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		panic("build hook binary: " + err.Error())
	}
	code := m.Run()
	os.RemoveAll(dir)
	unscrub()
	os.Exit(code)
}

// repo is a throwaway git repo with the OpenBox hook installed.
type repo struct {
	t   *testing.T
	dir string
	g   Git
}

func newRepo(t *testing.T) *repo {
	t.Helper()
	dir := t.TempDir()
	r := &repo{t: t, dir: dir, g: Git{Dir: dir}}
	r.git(nil, "init", "-q", "-b", "main")
	r.git(nil, "config", "user.name", "Dev")
	r.git(nil, "config", "user.email", "dev@example.com")
	r.git(nil, "config", "commit.gpgsign", "false")

	hooksDir, err := r.g.HooksDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := InstallHook(hooksDir, HookConfig{Command: hookBin}); err != nil {
		t.Fatal(err)
	}
	return r
}

// git runs a git command in the repo. extraEnv entries (e.g. OPENBOX_SESSION=…)
// are appended to a hermetic base environment and reach the hook child process.
func (r *repo) git(extraEnv []string, args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", append([]string{"-C", r.dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
	)
	cmd.Env = append(cmd.Env, extraEnv...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// gitAllowFail runs git but returns the error instead of failing the test — for
// asserting that a command (e.g. an empty commit) is correctly rejected.
func (r *repo) gitAllowFail(extraEnv []string, args ...string) (string, error) {
	r.t.Helper()
	cmd := exec.Command("git", append([]string{"-C", r.dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_TERMINAL_PROMPT=0")
	cmd.Env = append(cmd.Env, extraEnv...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// headSessions resolves HEAD's sessions exactly as SL-6 will server-side
// (S3 R7): via %(trailers), the trailing trailer block only.
func (r *repo) headSessions() []string {
	r.t.Helper()
	out := r.git(nil, "log", "-1",
		"--format=%(trailers:key="+TrailerKey+",valueonly,separator=%x0A)")
	return splitIDs(out)
}

func sess(id string) []string { return []string{"OPENBOX_SESSION=" + id} }

// --- The S3 rewrite matrix (spike §2) --------------------------------------

func TestE2E_PlainCommitStamps(t *testing.T) {
	r := newRepo(t)
	r.git(sess("sess-A"), "commit", "--allow-empty", "-m", "work")
	if got := r.headSessions(); !reflect.DeepEqual(got, []string{"sess-A"}) {
		t.Fatalf("HEAD sessions = %v, want [sess-A]", got)
	}
}

func TestE2E_AmendIsIdempotent(t *testing.T) {
	r := newRepo(t)
	r.git(sess("sess-A"), "commit", "--allow-empty", "-m", "work")
	// Amend re-fires prepare-commit-msg (source "commit", S3 R2) → no duplicate.
	r.git(sess("sess-A"), "commit", "--amend", "--allow-empty", "--no-edit")
	if got := r.headSessions(); !reflect.DeepEqual(got, []string{"sess-A"}) {
		t.Fatalf("after amend, HEAD sessions = %v, want [sess-A]", got)
	}
}

func TestE2E_HumanCommitIsUnattributed(t *testing.T) {
	r := newRepo(t)
	// No OPENBOX_SESSION in scope → no trailer (SL-6 records unattributed; never
	// a wrong guess, INV-6).
	r.git(nil, "commit", "--allow-empty", "-m", "manual fix")
	if got := r.headSessions(); len(got) != 0 {
		t.Fatalf("human commit stamped %v, want none", got)
	}
}

func TestE2E_PlainRebasePreservesTrailer(t *testing.T) {
	r := newRepo(t)
	r.git(sess("sess-base"), "commit", "--allow-empty", "-m", "base")
	r.git(nil, "checkout", "-q", "-b", "feature")
	r.git(sess("sess-A"), "commit", "--allow-empty", "-m", "feature work")
	r.git(nil, "checkout", "-q", "main")
	r.git(sess("sess-main"), "commit", "--allow-empty", "-m", "main moves on")
	r.git(nil, "checkout", "-q", "feature")
	// Plain rebase copies the message verbatim → trailer survives (S3 §2).
	r.git(nil, "rebase", "main")
	if got := r.headSessions(); !reflect.DeepEqual(got, []string{"sess-A"}) {
		t.Fatalf("after rebase, HEAD sessions = %v, want [sess-A]", got)
	}
}

// Squash = the multi-session fan-in good path (S3 §2). Crucially it works even
// when the developer running the rebase has NO session of their own: the hook
// heals the mid-body session lines the squash concatenated into the trailing
// trailer block. This is the property that makes fan-in resolvable via
// %(trailers), which naive stamping does NOT provide (see
// TestStamp_HealsSquashConcatenation).
func TestE2E_SquashFansInAllSessions(t *testing.T) {
	r := newRepo(t)
	r.git(sess("sess-base"), "commit", "--allow-empty", "-m", "base")
	r.git(sess("sess-A"), "commit", "--allow-empty", "-m", "commit A")
	r.git(sess("sess-B"), "commit", "--allow-empty", "-m", "commit B")

	// Squash B into A, non-interactively, with NO session in scope for the rebase.
	seqEditor := "sed -i -e 2s/^pick/squash/"
	r.git([]string{
		"GIT_SEQUENCE_EDITOR=" + seqEditor,
		"GIT_EDITOR=true", // accept the (already-stamped) combined message
	}, "rebase", "-i", "HEAD~2")

	got := r.headSessions()
	want := map[string]bool{"sess-A": true, "sess-B": true}
	if len(got) != 2 || !want[got[0]] || !want[got[1]] {
		t.Fatalf("after squash, HEAD sessions = %v, want fan-in {sess-A, sess-B}", got)
	}
}

// Fixup is the KNOWN loss mode (S3 §2): the fixup commit's message — and thus
// its session — is discarded. This test PINS that loss so SL-6's downstream
// detection (trailer-stripped) has a defined behavior to key on; it is not a
// bug in the write side, which cannot recover a message git threw away.
func TestE2E_FixupDropsItsSession(t *testing.T) {
	r := newRepo(t)
	r.git(sess("sess-base"), "commit", "--allow-empty", "-m", "base")
	r.git(sess("sess-A"), "commit", "--allow-empty", "-m", "commit A")
	r.git(sess("sess-B"), "commit", "--allow-empty", "-m", "commit B")

	seqEditor := "sed -i -e 2s/^pick/fixup/"
	r.git([]string{
		"GIT_SEQUENCE_EDITOR=" + seqEditor,
		"GIT_EDITOR=true",
	}, "rebase", "-i", "HEAD~2")

	got := r.headSessions()
	if !reflect.DeepEqual(got, []string{"sess-A"}) {
		t.Fatalf("after fixup, HEAD sessions = %v, want [sess-A] (sess-B is the documented loss)", got)
	}
}

// A missing engine binary must NOT break the commit (fail-open). The script
// guards on the binary being resolvable; point it at a non-existent path.
func TestE2E_MissingBinaryDoesNotBreakCommit(t *testing.T) {
	r := newRepo(t)
	r.git(append(sess("sess-A"), "OPENBOX_GIT_HOOK=/nonexistent/openbox-git-hook"),
		"commit", "--allow-empty", "-m", "work")
	// Commit succeeded (git() would have failed the test otherwise); it is simply
	// unstamped.
	if got := r.headSessions(); len(got) != 0 {
		t.Fatalf("expected no trailer with missing binary, got %v", got)
	}
}

func TestE2E_NotesMirror(t *testing.T) {
	r := newRepo(t)
	r.git(sess("sess-A"), "commit", "--allow-empty", "-m", "work")
	if err := r.g.WriteNoteMirror("HEAD", []string{"sess-A", "sess-B"}); err != nil {
		t.Fatal(err)
	}
	got, err := r.g.ReadNoteMirror("HEAD")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"sess-A", "sess-B"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("note mirror = %v, want %v", got, want)
	}
	// Reading a commit with no note is a clean empty, not an error.
	r.git(nil, "commit", "--allow-empty", "-m", "second")
	if got, err := r.g.ReadNoteMirror("HEAD"); err != nil || len(got) != 0 {
		t.Fatalf("no-note read = %v, err=%v; want empty,nil", got, err)
	}
}

// End-to-end proof of the parallel-safe wiring WITHOUT any env: the adapter
// writes a session liveness record (simulated here), and a real `git commit`
// resolves it via the worktree-scoped registry. This is the loop SL5-WIRE-1
// makes real (the adapter writes the record from its hooks).
func TestE2E_RegistryAttributesCommit(t *testing.T) {
	r := newRepo(t)
	regDir := t.TempDir()
	top, err := r.g.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	// Simulate the adapter's PreToolUse touch just before the commit.
	if err := WriteSessionRecord(regDir, "sess-reg", top, time.Now()); err != nil {
		t.Fatal(err)
	}
	// NOTE: no OPENBOX_SESSION — only OPENBOX_SESSION_DIR so the hook finds the
	// registry. Attribution flows purely through the registry.
	r.git([]string{"OPENBOX_SESSION_DIR=" + regDir}, "commit", "--allow-empty", "-m", "agent work")
	if got := r.headSessions(); !reflect.DeepEqual(got, []string{"sess-reg"}) {
		t.Fatalf("registry attribution: HEAD sessions = %v, want [sess-reg]", got)
	}
}

// Two repos, two concurrent sessions, one shared registry: each commit is
// attributed to the session working in ITS worktree — never cross-attributed.
func TestE2E_RegistryParallelTwoRepos(t *testing.T) {
	regDir := t.TempDir()
	rA := newRepo(t)
	rB := newRepo(t)
	topA, _ := rA.g.Worktree()
	topB, _ := rB.g.Worktree()
	now := time.Now()
	WriteSessionRecord(regDir, "sess-A", topA, now)
	WriteSessionRecord(regDir, "sess-B", topB, now)

	env := []string{"OPENBOX_SESSION_DIR=" + regDir}
	rA.git(env, "commit", "--allow-empty", "-m", "work in A")
	rB.git(env, "commit", "--allow-empty", "-m", "work in B")

	if got := rA.headSessions(); !reflect.DeepEqual(got, []string{"sess-A"}) {
		t.Fatalf("repo A → %v, want [sess-A]", got)
	}
	if got := rB.headSessions(); !reflect.DeepEqual(got, []string{"sess-B"}) {
		t.Fatalf("repo B → %v, want [sess-B]", got)
	}
}

func TestE2E_HooksDirHonorsCoreHooksPath(t *testing.T) {
	r := newRepo(t)
	custom := filepath.Join(r.dir, "myhooks")
	r.git(nil, "config", "core.hooksPath", custom)
	got, err := r.g.HooksDir()
	if err != nil {
		t.Fatal(err)
	}
	if got != custom {
		t.Fatalf("HooksDir = %q, want %q", got, custom)
	}
	// SL5-SEC-2: the ambient path (HooksDirDefault) must IGNORE core.hooksPath.
	def, err := r.g.HooksDirDefault()
	if err != nil {
		t.Fatal(err)
	}
	if def == custom {
		t.Fatalf("HooksDirDefault must ignore core.hooksPath, got %q", def)
	}
	if !strings.HasSuffix(def, filepath.Join(".git", "hooks")) {
		t.Fatalf("HooksDirDefault = %q, want <git-common-dir>/hooks", def)
	}
}

// F1 end-to-end: a developer who saves an EMPTY message (comment-only template)
// to abort a commit must still abort — the hook must not stamp it into a junk
// trailer-only commit.
func TestE2E_EmptyMessageStillAborts(t *testing.T) {
	r := newRepo(t)
	r.git(sess("sess-A"), "commit", "--allow-empty", "-m", "initial")
	head0 := strings.TrimSpace(r.git(nil, "rev-parse", "HEAD"))

	// Editor accepts the template unchanged (comments only) → empty message.
	out, err := r.gitAllowFail(append(sess("sess-A"), "GIT_EDITOR=true"),
		"commit", "--allow-empty")
	if err == nil {
		t.Fatalf("empty-message commit should abort, but it succeeded:\n%s", out)
	}
	head1 := strings.TrimSpace(r.git(nil, "rev-parse", "HEAD"))
	if head1 != head0 {
		t.Fatalf("HEAD advanced on an aborted empty commit (junk commit): %s -> %s", head0, head1)
	}
}
