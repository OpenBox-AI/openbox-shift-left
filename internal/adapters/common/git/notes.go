package git

import (
	"fmt"
	"strings"
)

// NotesRef is the git-notes ref for the OpenBox session mirror. It is
// deliberately namespaced so it never collides with the default commit notes.
const NotesRef = "refs/notes/openbox"

// WriteNoteMirror records the session id(s) for a commit as a git note under
// NotesRef.
func (g Git) WriteNoteMirror(rev string, sessions []string) error {
	if rev == "" {
		rev = "HEAD"
	}
	ids := validSessionIDs(sessions)
	if len(ids) == 0 {
		return nil
	}
	msg := TrailerKey + ": " + strings.Join(ids, "\n"+TrailerKey+": ")
	if _, err := g.run("notes", "--ref", NotesRef, "add", "-f", "-m", msg, rev); err != nil {
		return fmt.Errorf("write note mirror: %w", err)
	}
	return nil
}

// ReadNoteMirror returns the session ids recorded in the NotesRef note for a
// commit (empty if none). For local inspection/tests only; never
// authoritative.
func (g Git) ReadNoteMirror(rev string) ([]string, error) {
	if rev == "" {
		rev = "HEAD"
	}
	out, _, err := g.runLimited(MaxNoteBytes, "notes", "--ref", NotesRef, "show", rev)
	if err != nil {
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
