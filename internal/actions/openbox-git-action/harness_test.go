package gitaction

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestMain the resolver runs against real git (that is the whole point of a
// server-side resolver; it must agree with git's own trailer parsing).
func TestMain(m *testing.M) {
	if _, err := exec.LookPath("git"); err != nil {
		os.Exit(0)
	}
	os.Exit(m.Run())
}

type testRepo struct {
	t   *testing.T
	dir string
}

func newTestRepo(t *testing.T) *testRepo {
	t.Helper()
	dir := t.TempDir()
	r := &testRepo{t: t, dir: dir}
	r.git("init", "-q", "-b", "main")
	r.git("config", "user.name", "Dev")
	r.git("config", "user.email", "dev@example.com")
	r.git("config", "commit.gpgsign", "false")
	return r
}

func (r *testRepo) git(args ...string) string {
	r.t.Helper()
	out, err := r.gitErr(args...)
	if err != nil {
		r.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return out
}

func (r *testRepo) gitErr(args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", r.dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
	)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (r *testRepo) commit(msg string) string {
	r.t.Helper()
	f := filepath.Join(r.t.TempDir(), "msg")
	if err := os.WriteFile(f, []byte(msg), 0o600); err != nil {
		r.t.Fatal(err)
	}
	r.git("commit", "--allow-empty", "--cleanup=verbatim", "-F", f)
	return r.head()
}

func (r *testRepo) head() string {
	return strings.TrimSpace(r.git("rev-parse", "HEAD"))
}

func trailerMsg(subject string, sessions ...string) string {
	var b strings.Builder
	b.WriteString(subject)
	b.WriteString("\n\n")
	for _, s := range sessions {
		b.WriteString(trailerKey + ": " + s + "\n")
	}
	return b.String()
}

func (r *testRepo) resolver(v OwnershipVerifier) *Resolver {
	return NewResolver(r.dir, v)
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }

func ids(claims []SessionClaim) map[string]bool {
	m := map[string]bool{}
	for _, c := range claims {
		m[c.SessionID] = true
	}
	return m
}
