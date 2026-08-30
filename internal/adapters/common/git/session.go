package git

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Provider-independent session discovery. This package lives in
// internal/adapters/common/ precisely because "which session produced this
// commit" must not depend on any one tool.
const (
	EnvSession     = "OPENBOX_SESSION"
	EnvSessionFile = "OPENBOX_SESSION_FILE"

	// EnvCodexThreadID is the session (≡ thread) id Codex itself injects into
	// every tool/shell exec environment (codex-rs core/src/exec_env.rs @
	// rust-v0.145.0); so a Codex-run `git commit` sees it in the git-hook env
	// with no liveness registry at all.
	EnvCodexThreadID = "CODEX_THREAD_ID"
)

// SessionResolver reads the session id(s) in scope for a commit.
type SessionResolver struct {
	Getenv     func(string) string
	ReadFile   func(string) ([]byte, error)
	ReadDir    func(string) ([]os.DirEntry, error)
	Now        func() time.Time
	SessionDir string        // "" => DefaultSessionDir()
	TTL        time.Duration // 0 => env OPENBOX_SESSION_TTL or defaultSessionTTL
}

func (r SessionResolver) getenv(k string) string {
	if r.Getenv != nil {
		return r.Getenv(k)
	}
	return os.Getenv(k)
}

func (r SessionResolver) readFile(p string) ([]byte, error) {
	if r.ReadFile != nil {
		return r.ReadFile(p)
	}
	return os.ReadFile(p)
}

func (r SessionResolver) readDir(p string) ([]os.DirEntry, error) {
	if r.ReadDir != nil {
		return r.ReadDir(p)
	}
	return os.ReadDir(p)
}

func (r SessionResolver) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r SessionResolver) sessionDir() string {
	if r.SessionDir != "" {
		return r.SessionDir
	}
	return DefaultSessionDir()
}

func (r SessionResolver) ttl() time.Duration {
	if r.TTL > 0 {
		return r.TTL
	}
	if s := strings.TrimSpace(r.getenv(EnvSessionTTL)); s != "" {
		if secs, err := strconv.Atoi(s); err == nil && secs > 0 {
			return time.Duration(secs) * time.Second
		}
	}
	return defaultSessionTTL
}

// Resolve returns the session id(s) to attribute a commit in worktree to.
// Worktree is the commit's git top-level ("" if it could not be determined;
// then only the env-based tiers apply).
func (r SessionResolver) Resolve(worktree string) []string {
	//   - Inheritance: any process launched from within a Codex exec (a nested
	//     agent session, a long-lived shell) inherits the var, so its commits
	//     attribute to the enclosing Codex thread rather than to the inner
	//     session / registry tier; and Tier-0 outranks even an explicit
	//     OPENBOX_SESSION set inside that environment.
	//   - Suppression: a present-but-garbage value wins Tier-0 here and is then
	//     dropped by the sink's validation, with no fallback to the remaining
	//     tiers; the commit lands unattributed rather than mis-guessed
	//     (INV-6-safe, but an env-writing process can exploit it to suppress
	//     attribution).
	if id := strings.TrimSpace(r.getenv(EnvCodexThreadID)); id != "" {
		return []string{id}
	}
	if env := r.envSessions(); len(env) > 0 {
		return env
	}
	if worktree == "" {
		return nil
	}
	if id := r.resolveFromRegistry(worktree); id != "" {
		return []string{id}
	}
	return nil
}

// envSessions the file is best-effort (a read error is ignored; observe-only
// never blocks a commit).
func (r SessionResolver) envSessions() []string {
	var ids []string
	ids = append(ids, splitIDs(r.getenv(EnvSession))...)
	if path := strings.TrimSpace(r.getenv(EnvSessionFile)); path != "" {
		if data, err := r.readFile(path); err == nil {
			ids = append(ids, splitIDs(string(data))...)
		}
	}
	return dedupe(ids)
}

// resolveFromRegistry records older than the TTL (crashed sessions that never
// wrote SessionEnd) are ignored so a later human commit is not falsely
// attributed.
func (r SessionResolver) resolveFromRegistry(worktree string) string {
	entries, err := r.readDir(r.sessionDir())
	if err != nil {
		return ""
	}
	top := resolvePath(worktree)
	cutoff := r.now().Add(-r.ttl()).UnixNano()

	var best SessionRecord
	found := false
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := r.readFile(filepath.Join(r.sessionDir(), e.Name()))
		if err != nil {
			continue
		}
		var rec SessionRecord
		if json.Unmarshal(data, &rec) != nil || rec.SessionID == "" {
			continue
		}
		if rec.UpdatedAt < cutoff || !withinWorktree(rec.Cwd, top) {
			continue
		}
		if !found || rec.UpdatedAt > best.UpdatedAt {
			best, found = rec, true
		}
	}
	if found {
		return best.SessionID
	}
	return ""
}

func resolvePath(p string) string {
	if p == "" {
		return ""
	}
	if real, err := filepath.EvalSymlinks(p); err == nil {
		return real
	}
	return filepath.Clean(p)
}

func withinWorktree(cwd, top string) bool {
	if cwd == "" || top == "" {
		return false
	}
	c := resolvePath(cwd)
	return c == top || strings.HasPrefix(c, top+string(os.PathSeparator))
}

func splitIDs(s string) []string {
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return r == '\n' || r == '\r' || r == ',' || r == ' ' || r == '\t'
	})
	out := fields[:0]
	for _, f := range fields {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

func dedupe(ids []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}
