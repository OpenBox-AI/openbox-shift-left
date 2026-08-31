package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

// hookBin is the compiled `openbox`, built once for the whole matrix.
var hookBin string

func TestMain(m *testing.M) {
	if _, err := exec.LookPath("git"); err != nil {
		os.Exit(0)
	}
	unscrub := scrubAmbientSessionEnv()
	dir, err := os.MkdirTemp("", "obgit-bin")
	if err != nil {
		panic(err)
	}
	hookBin = filepath.Join(dir, "openbox")
	build := exec.Command("go", "build", "-o", hookBin, "github.com/openbox-ai/openbox-shift-left/cmd/openbox")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		panic("build hook binary: " + err.Error())
	}
	code := m.Run()
	os.RemoveAll(dir)
	unscrub()
	os.Exit(code)
}

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

func (r *repo) gitAllowFail(extraEnv []string, args ...string) (string, error) {
	r.t.Helper()
	cmd := exec.Command("git", append([]string{"-C", r.dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_TERMINAL_PROMPT=0")
	cmd.Env = append(cmd.Env, extraEnv...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (r *repo) headSessions() []string {
	r.t.Helper()
	out := r.git(nil, "log", "-1",
		"--format=%(trailers:key="+TrailerKey+",valueonly,separator=%x0A)")
	return splitIDs(out)
}

func sess(id string) []string { return []string{"OPENBOX_SESSION=" + id} }

func TestE2E_PlainCommitStamps(t *testing.T) {
	r := newRepo(t)
	r.git(sess("sess-A"), "commit", "--allow-empty", "-m", "work")
	if diff := cmp.Diff([]string{"sess-A"}, r.headSessions()); diff != "" {
		t.Fatalf("HEAD sessions (-want +got):\n%s", diff)
	}
}

func TestE2E_AmendIsIdempotent(t *testing.T) {
	r := newRepo(t)
	r.git(sess("sess-A"), "commit", "--allow-empty", "-m", "work")
	r.git(sess("sess-A"), "commit", "--amend", "--allow-empty", "--no-edit")
	if diff := cmp.Diff([]string{"sess-A"}, r.headSessions()); diff != "" {
		t.Fatalf("after amend, HEAD sessions (-want +got):\n%s", diff)
	}
}

func TestE2E_HumanCommitIsUnattributed(t *testing.T) {
	r := newRepo(t)
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
	r.git(nil, "rebase", "main")
	if diff := cmp.Diff([]string{"sess-A"}, r.headSessions()); diff != "" {
		t.Fatalf("after rebase, HEAD sessions (-want +got):\n%s", diff)
	}
}

// TestE2E_SquashFansInAllSessions squash = the multi-session fan-in good path
// (S3 §2).
func TestE2E_SquashFansInAllSessions(t *testing.T) {
	r := newRepo(t)
	r.git(sess("sess-base"), "commit", "--allow-empty", "-m", "base")
	r.git(sess("sess-A"), "commit", "--allow-empty", "-m", "commit A")
	r.git(sess("sess-B"), "commit", "--allow-empty", "-m", "commit B")

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

// TestE2E_FixupDropsItsSession fixup is the known loss mode (S3 §2): the fixup
// commit's message; and thus its session; is discarded.
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

	if diff := cmp.Diff([]string{"sess-A"}, r.headSessions()); diff != "" {
		t.Fatalf("after fixup, HEAD sessions (-want +got):\n%s\nsess-B is the documented loss", diff)
	}
}

// TestE2E_MissingBinaryDoesNotBreakCommit a missing engine binary must NOT
// break the commit (fail-open).
func TestE2E_MissingBinaryDoesNotBreakCommit(t *testing.T) {
	r := newRepo(t)
	r.git(append(sess("sess-A"), "OPENBOX_GIT_HOOK=/nonexistent/openbox-git-hook"),
		"commit", "--allow-empty", "-m", "work")
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
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("note mirror (-want +got):\n%s", diff)
	}
	r.git(nil, "commit", "--allow-empty", "-m", "second")
	if got, err := r.g.ReadNoteMirror("HEAD"); err != nil || len(got) != 0 {
		t.Fatalf("no-note read = %v, err=%v; want empty,nil", got, err)
	}
}

// TestE2E_RegistryAttributesCommit end-to-end proof of the parallel-safe
// wiring without any env: the adapter writes a session liveness record
// (simulated here), and a real `git commit` resolves it via the worktree-
// scoped registry.
func TestE2E_RegistryAttributesCommit(t *testing.T) {
	r := newRepo(t)
	regDir := t.TempDir()
	top, err := r.g.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteSessionRecord(regDir, "sess-reg", top, time.Now()); err != nil {
		t.Fatal(err)
	}
	r.git([]string{"OPENBOX_SESSION_DIR=" + regDir}, "commit", "--allow-empty", "-m", "agent work")
	if diff := cmp.Diff([]string{"sess-reg"}, r.headSessions()); diff != "" {
		t.Fatalf("registry attribution: HEAD sessions (-want +got):\n%s", diff)
	}
}

// TestE2E_RegistryParallelTwoRepos two repos, two concurrent sessions, one
// shared registry: each commit is attributed to the session working in ITS
// worktree; never cross-attributed.
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

	if diff := cmp.Diff([]string{"sess-A"}, rA.headSessions()); diff != "" {
		t.Fatalf("repo A (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{"sess-B"}, rB.headSessions()); diff != "" {
		t.Fatalf("repo B (-want +got):\n%s", diff)
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

// TestE2E_EmptyMessageStillAborts f1 end-to-end: a developer who saves an
// empty message (comment-only template) to abort a commit must still abort;
// the hook must not stamp it into a junk trailer-only commit.
func TestE2E_EmptyMessageStillAborts(t *testing.T) {
	r := newRepo(t)
	r.git(sess("sess-A"), "commit", "--allow-empty", "-m", "initial")
	head0 := strings.TrimSpace(r.git(nil, "rev-parse", "HEAD"))

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
