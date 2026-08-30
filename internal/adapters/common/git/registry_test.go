package git

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func at(t0 time.Time) func() time.Time { return func() time.Time { return t0 } }

func TestRegistry_WriteReadRemove(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(1_700_000_000, 0)
	if err := WriteSessionRecord(dir, "sess-A", "/repo/a", now); err != nil {
		t.Fatal(err)
	}
	r := SessionResolver{SessionDir: dir, Now: at(now)}
	if got := r.Resolve("/repo/a"); !reflect.DeepEqual(got, []string{"sess-A"}) {
		t.Fatalf("Resolve = %v, want [sess-A]", got)
	}
	if err := RemoveSessionRecord(dir, "sess-A"); err != nil {
		t.Fatal(err)
	}
	if got := r.Resolve("/repo/a"); len(got) != 0 {
		t.Fatalf("after remove, Resolve = %v, want empty", got)
	}
	if err := RemoveSessionRecord(dir, "sess-A"); err != nil {
		t.Fatalf("remove absent: %v", err)
	}
}

// TestRegistry_ParallelSessionsDifferentWorktrees the parallel-sessions
// requirement: two sessions in different worktrees each resolve to their own;
// never cross-attributed.
func TestRegistry_ParallelSessionsDifferentWorktrees(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(1_700_000_000, 0)
	WriteSessionRecord(dir, "sess-A", "/work/repoA/sub", now)
	WriteSessionRecord(dir, "sess-B", "/work/repoB", now)
	r := SessionResolver{SessionDir: dir, Now: at(now)}

	if got := r.Resolve("/work/repoA"); !reflect.DeepEqual(got, []string{"sess-A"}) {
		t.Fatalf("repoA → %v, want [sess-A]", got)
	}
	if got := r.Resolve("/work/repoB"); !reflect.DeepEqual(got, []string{"sess-B"}) {
		t.Fatalf("repoB → %v, want [sess-B]", got)
	}
	if got := r.Resolve("/work/repoC"); len(got) != 0 {
		t.Fatalf("repoC → %v, want empty", got)
	}
}

// TestRegistry_SameWorktreeMostRecentWins two sessions in the same worktree
// resolve to the most-recently-updated one (the committing session refreshed
// on its PreToolUse ms before the commit).
func TestRegistry_SameWorktreeMostRecentWins(t *testing.T) {
	dir := t.TempDir()
	base := time.Unix(1_700_000_000, 0)
	WriteSessionRecord(dir, "sess-old", "/repo", base)
	WriteSessionRecord(dir, "sess-fresh", "/repo", base.Add(30*time.Second))
	r := SessionResolver{SessionDir: dir, Now: at(base.Add(time.Minute))}
	if got := r.Resolve("/repo"); !reflect.DeepEqual(got, []string{"sess-fresh"}) {
		t.Fatalf("same-worktree → %v, want [sess-fresh]", got)
	}
}

// TestRegistry_StaleRecordIgnored a record older than the TTL (a crashed
// session that never wrote SessionEnd) is ignored, so a much-later human
// commit is not falsely attributed to it.
func TestRegistry_StaleRecordIgnored(t *testing.T) {
	dir := t.TempDir()
	old := time.Unix(1_700_000_000, 0)
	WriteSessionRecord(dir, "sess-crashed", "/repo", old)
	r := SessionResolver{
		SessionDir: dir,
		Now:        at(old.Add(24 * time.Hour)), // well past the 8h default TTL
	}
	if got := r.Resolve("/repo"); len(got) != 0 {
		t.Fatalf("stale record attributed %v, want empty", got)
	}
}

// TestRegistry_EnvOverrideWins the explicit env override beats the registry
// (CI / non-Claude-Code providers).
func TestRegistry_EnvOverrideWins(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(1_700_000_000, 0)
	WriteSessionRecord(dir, "sess-registry", "/repo", now)
	r := SessionResolver{
		SessionDir: dir,
		Now:        at(now),
		Getenv: func(k string) string {
			if k == EnvSession {
				return "sess-explicit"
			}
			return ""
		},
	}
	if got := r.Resolve("/repo"); !reflect.DeepEqual(got, []string{"sess-explicit"}) {
		t.Fatalf("override → %v, want [sess-explicit]", got)
	}
}

func TestRegistry_TTLFromEnv(t *testing.T) {
	dir := t.TempDir()
	base := time.Unix(1_700_000_000, 0)
	WriteSessionRecord(dir, "sess-A", "/repo", base)
	r := SessionResolver{
		SessionDir: dir,
		Now:        at(base.Add(2 * time.Minute)),
		Getenv: func(k string) string {
			if k == EnvSessionTTL {
				return "60"
			}
			return ""
		}, // 60s TTL
	}
	if got := r.Resolve("/repo"); len(got) != 0 {
		t.Fatalf("env TTL not honored: %v, want empty (record 2m old > 60s)", got)
	}
}

// TestRegistry_WriteRejectsInvalidID sL5-SEC-3: an invalid/secret-shaped id
// must never be persisted to the registry (validate at source, not only at the
// trailer sink). Skips silently.
func TestRegistry_WriteRejectsInvalidID(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(1_700_000_000, 0)
	for _, bad := range []string{"", "obx_secret", "has space", "a\nb"} {
		if err := WriteSessionRecord(dir, bad, "/repo", now); err != nil {
			t.Fatalf("WriteSessionRecord(%q) err=%v (should skip silently)", bad, err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err == nil {
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".json") {
				t.Fatalf("invalid id created a record: %s", e.Name())
			}
		}
	}
}

// TestRegistry_SymlinkedCwd f3: a session cwd recorded via a symlinked path
// still matches a worktree given by its real path (macOS /tmp -> /private/tmp,
// etc.).
func TestRegistry_SymlinkedCwd(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skip("symlinks unsupported")
	}
	dir := t.TempDir()
	now := time.Unix(1_700_000_000, 0)
	WriteSessionRecord(dir, "sess-A", link, now) // recorded via the symlink
	r := SessionResolver{SessionDir: dir, Now: at(now)}
	if got := r.Resolve(real); !reflect.DeepEqual(got, []string{"sess-A"}) { // resolved via the real path
		t.Fatalf("symlinked cwd not matched: %v, want [sess-A]", got)
	}
}

// TestRegistry_SubSecondRecency f4: two records in the same wall-clock second
// are disambiguated by nanosecond precision, so the genuinely-later session
// wins the recency tiebreak.
func TestRegistry_SubSecondRecency(t *testing.T) {
	dir := t.TempDir()
	base := time.Unix(1_700_000_000, 0)
	WriteSessionRecord(dir, "sess-old", "/repo", base)
	WriteSessionRecord(dir, "sess-new", "/repo", base.Add(5*time.Millisecond))
	r := SessionResolver{SessionDir: dir, Now: at(base.Add(time.Second))}
	if got := r.Resolve("/repo"); !reflect.DeepEqual(got, []string{"sess-new"}) {
		t.Fatalf("sub-second recency: %v, want [sess-new]", got)
	}
}

func TestWithinWorktree(t *testing.T) {
	top := filepath.Clean("/work/repo")
	cases := []struct {
		cwd string
		in  bool
	}{
		{"/work/repo", true},
		{"/work/repo/pkg/sub", true},
		{"/work/repo/", true},
		{"/work/repository", false}, // prefix-of-string but not a path child
		{"/work", false},
		{"", false},
	}
	for _, c := range cases {
		if got := withinWorktree(c.cwd, top); got != c.in {
			t.Fatalf("withinWorktree(%q) = %v, want %v", c.cwd, got, c.in)
		}
	}
}
