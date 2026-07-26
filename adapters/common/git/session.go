package git

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Provider-independent session discovery.
//
// SL-5 lives in adapters/common/ precisely because "which session produced this
// commit" must not depend on any one tool. Resolution has two tiers:
//
//  1. EXPLICIT OVERRIDE — OPENBOX_SESSION / OPENBOX_SESSION_FILE. A provider or
//     CI job that CAN inject the id into the git environment (or a human) wins
//     outright. One or more ids => genuine fan-in.
//  2. REGISTRY (the parallel-safe default for tools that cannot inject env, like
//     Claude Code) — the adapter writes a per-session liveness record and the
//     resolver attributes the commit to the most-recently-updated session whose
//     cwd lies within the commit's git worktree (see registry.go).
//
// Missing on both tiers => no attribution (the commit is left unstamped, which
// SL-6 records as unattributed — never a wrong guess, INV-6).
const (
	EnvSession     = "OPENBOX_SESSION"
	EnvSessionFile = "OPENBOX_SESSION_FILE"

	// EnvCodexThreadID is the session (≡ thread) id Codex itself injects into
	// EVERY tool/shell exec environment (codex-rs core/src/exec_env.rs @
	// rust-v0.145.0; spike S5 2026-07-23 addendum #2) — so a Codex-run
	// `git commit` sees it in the git-hook env with no liveness registry at
	// all. STORY-SL7-A AC-8: it is the HIGHEST-precedence source — the tool
	// itself asserting "this exec belongs to session X" outranks both the
	// operator override and registry recency. There is no CODEX_SESSION_ID.
	EnvCodexThreadID = "CODEX_THREAD_ID"
)

// SessionResolver reads the session id(s) in scope for a commit. Every external
// dependency is an injectable field so tests need no real environment, clock, or
// filesystem; the zero value reads the real ones.
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
// worktree is the commit's git top-level ("" if it could not be determined —
// then only the env-based tiers apply).
func (r SessionResolver) Resolve(worktree string) []string {
	// Tier 0 (STORY-SL7-A, additive): the provider-injected CODEX_THREAD_ID.
	// Codex stamps it into the exec env of the very process running `git
	// commit`, so it is authoritative for THIS commit — highest precedence.
	// Validation (length/newline/secret-shape) still happens at the trailer
	// sink (validateSessionID), same as every other tier. A Claude Code
	// session never SETS this var, so the CC resolution below is untouched.
	//
	// Known, ACCEPTED edges (G3 SL7-A F-3 — documented for SL7-A, revisit at
	// SL7-B), both consequences of "highest precedence" being env-carried:
	//   - INHERITANCE: any process LAUNCHED FROM WITHIN a Codex exec (a nested
	//     agent session, a long-lived shell) inherits the var, so its commits
	//     attribute to the enclosing Codex thread rather than to the inner
	//     session / registry tier — and Tier-0 outranks even an explicit
	//     OPENBOX_SESSION set inside that environment. Arguably transitively
	//     correct; the SL-15 OwnershipVerifier still downgrades a claim the
	//     server can't bind to the caller.
	//   - SUPPRESSION: a present-but-garbage value wins Tier-0 here and is then
	//     dropped by the sink's validation, with NO fallback to the remaining
	//     tiers — the commit lands unattributed rather than mis-guessed
	//     (INV-6-safe, but an env-writing process can exploit it to suppress
	//     attribution; G_SEC SL7-A F5).
	if id := strings.TrimSpace(r.getenv(EnvCodexThreadID)); id != "" {
		return []string{id}
	}
	// Tier 1: explicit override (env / file). Wins outright, supports fan-in.
	if env := r.envSessions(); len(env) > 0 {
		return env
	}
	// Tier 2: the freshest live session working within this worktree.
	if worktree == "" {
		return nil
	}
	if id := r.resolveFromRegistry(worktree); id != "" {
		return []string{id}
	}
	return nil
}

// envSessions reads the explicit-override tier: OPENBOX_SESSION plus an optional
// OPENBOX_SESSION_FILE, unioned and deduped. The file is best-effort (a read
// error is ignored — observe-only never blocks a commit).
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

// resolveFromRegistry returns the most-recently-updated live session whose cwd
// is within worktree, or "" if none. Records older than the TTL (crashed
// sessions that never wrote SessionEnd) are ignored so a later human commit is
// not falsely attributed. A commit attributes to a SINGLE session — genuine
// multi-session fan-in comes from squash healing (see StampMessageFile), not
// from parallel liveness.
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

// resolvePath canonicalizes a path for comparison, resolving symlinks
// best-effort (F3): a tool's recorded cwd may be under a symlinked path (e.g.
// macOS /tmp -> /private/tmp) while `rev-parse --show-toplevel` returns the real
// path, which would otherwise mismatch and drop attribution. Falls back to Clean
// when the path does not exist (e.g. injected test paths).
func resolvePath(p string) string {
	if p == "" {
		return ""
	}
	if real, err := filepath.EvalSymlinks(p); err == nil {
		return real
	}
	return filepath.Clean(p)
}

// withinWorktree reports whether cwd is the worktree top or a path beneath it.
// Both sides are symlink-resolved (F3) so equivalent real paths match.
func withinWorktree(cwd, top string) bool {
	if cwd == "" || top == "" {
		return false
	}
	c := resolvePath(cwd)
	return c == top || strings.HasPrefix(c, top+string(os.PathSeparator))
}

// splitIDs parses ids separated by newlines, commas, or whitespace, trimming
// each and dropping blanks.
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
