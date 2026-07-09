package git

import (
	"fmt"
	"strings"
)

// NotesRef is the git-notes ref for the OpenBox session mirror. It is
// deliberately namespaced so it never collides with the default commit notes.
const NotesRef = "refs/notes/openbox"

// WriteNoteMirror records the session id(s) for a commit as a git note under
// NotesRef (S3 R5). This is an OPTIONAL, explicitly NON-AUTHORITATIVE local
// breadcrumb: notes are keyed by SHA and are orphaned by any history rewrite
// (which mints a new SHA), and are not pushed/fetched by default — so they never
// survive a PR->GitHub-squash. The commit-message trailer remains the single
// source of truth (see doc.go); the note is only a convenience for local
// inspection. Best-effort: a failure is returned for the caller to log, never to
// break anything.
//
// It runs in a `post-commit` context (the SHA exists only after the commit),
// unlike trailer stamping which runs pre-commit in `prepare-commit-msg`.
func (g Git) WriteNoteMirror(rev string, sessions []string) error {
	if rev == "" {
		rev = "HEAD"
	}
	ids := validSessionIDs(sessions)
	if len(ids) == 0 {
		return nil
	}
	msg := TrailerKey + ": " + strings.Join(ids, "\n"+TrailerKey+": ")
	// -f overwrites an existing note on the same SHA (idempotent under re-fire).
	if _, err := g.run("notes", "--ref", NotesRef, "add", "-f", "-m", msg, rev); err != nil {
		return fmt.Errorf("write note mirror: %w", err)
	}
	return nil
}

// ReadNoteMirror returns the session ids recorded in the NotesRef note for a
// commit (empty if none). For local inspection/tests only — never authoritative.
func (g Git) ReadNoteMirror(rev string) ([]string, error) {
	if rev == "" {
		rev = "HEAD"
	}
	out, err := g.run("notes", "--ref", NotesRef, "show", rev)
	if err != nil {
		// No note for this object is the normal, non-error case.
		return nil, nil
	}
	var ids []string
	seen := map[string]bool{}
	prefix := TrailerKey + ":"
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		v := strings.TrimSpace(strings.TrimPrefix(line, prefix))
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		ids = append(ids, v)
	}
	return ids, nil
}
