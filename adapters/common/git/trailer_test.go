package git

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// writeMsg writes a commit-message file for message-level stamping tests.
func writeMsg(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "COMMIT_EDITMSG")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestStamp_AddsTrailer(t *testing.T) {
	g := Git{}
	msg := writeMsg(t, "add feature\n")
	if err := g.StampMessageFile(msg, []string{"sess-A"}); err != nil {
		t.Fatal(err)
	}
	got, err := g.ReadTrailers(msg)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"sess-A"}) {
		t.Fatalf("trailers = %v, want [sess-A]", got)
	}
}

// Re-firing the hook (the `--amend` case, S3 R2) must NOT duplicate an id.
func TestStamp_IdempotentOnReStamp(t *testing.T) {
	g := Git{}
	msg := writeMsg(t, "add feature\n")
	for i := 0; i < 3; i++ {
		if err := g.StampMessageFile(msg, []string{"sess-A"}); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := g.ReadTrailers(msg)
	if !reflect.DeepEqual(got, []string{"sess-A"}) {
		t.Fatalf("re-stamp duplicated: %v", got)
	}
}

// Distinct sessions => multiple lines (fan-in, S3 R3), order preserved.
func TestStamp_MultiSessionFanIn(t *testing.T) {
	g := Git{}
	msg := writeMsg(t, "shared work\n")
	if err := g.StampMessageFile(msg, []string{"sess-A", "sess-B"}); err != nil {
		t.Fatal(err)
	}
	// A later commit adds a third session on top of the existing two.
	if err := g.StampMessageFile(msg, []string{"sess-B", "sess-C"}); err != nil {
		t.Fatal(err)
	}
	got, _ := g.ReadTrailers(msg)
	want := []string{"sess-A", "sess-B", "sess-C"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("fan-in = %v, want %v", got, want)
	}
}

// The core squash-healing property (this repo's own finding): a squash leaves
// earlier session lines MID-BODY, where git's trailer parser cannot see them.
// StampMessageFile must harvest them into the trailing block so the full fan-in
// is resolvable via %(trailers) — exactly how SL-6 reads it (S3 R7).
func TestStamp_HealsSquashConcatenation(t *testing.T) {
	// Simulate the buffer a `git rebase -i` squash hands to prepare-commit-msg:
	// two source messages concatenated, each ending in its own trailer.
	squashed := "A subject\n\nOpenBox-Session: sess-A\n\nB subject\n\nOpenBox-Session: sess-B\n"
	msg := writeMsg(t, squashed)
	g := Git{}

	// Before healing, only the trailing block (sess-B) is parseable.
	pre, _ := g.ReadTrailers(msg)
	if !reflect.DeepEqual(pre, []string{"sess-B"}) {
		t.Fatalf("precondition: parser should see only sess-B, got %v", pre)
	}

	// Heal with no in-scope session (a human ran the squash): both must surface.
	if err := g.StampMessageFile(msg, nil); err != nil {
		t.Fatal(err)
	}
	got, _ := g.ReadTrailers(msg)
	want := []string{"sess-B", "sess-A"} // trailing block: existing sess-B, then harvested sess-A
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("healed fan-in = %v, want %v", got, want)
	}
}

// F1: a message with no real content (only git comments / blank lines) must be
// left UNTOUCHED, so git's own empty-message abort still fires — stamping it
// would create a junk trailer-only commit.
func TestStamp_EmptyMessageNotStamped(t *testing.T) {
	g := Git{}
	commentOnly := "\n# Please enter the commit message for your changes.\n# with '#' will be ignored.\n#\n"
	msg := writeMsg(t, commentOnly)
	before, _ := os.ReadFile(msg)
	if err := g.StampMessageFile(msg, []string{"sess-A"}); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(msg)
	if string(before) != string(after) {
		t.Fatalf("comment-only message was stamped (would become a junk commit):\n%s", after)
	}
}

func TestHasCommitContent(t *testing.T) {
	cases := []struct {
		name string
		msg  string
		has  bool
	}{
		{"real", "fix bug\n", true},
		{"comment_only", "# a\n#\n\n", false},
		{"blank_only", "\n\n \n", false},
		{"content_then_comment", "subject\n# comment\n", true},
		{"scissors_diff_ignored", "\n# ------------------------ >8 ------------------------\ndiff --git a b\n+code\n", false},
	}
	for _, c := range cases {
		if got := hasCommitContent([]byte(c.msg), "#"); got != c.has {
			t.Errorf("hasCommitContent(%q) = %v, want %v", c.msg, got, c.has)
		}
	}
}

func TestStamp_NoSessionLeavesMessageUntouched(t *testing.T) {
	g := Git{}
	msg := writeMsg(t, "plain human commit\n")
	before, _ := os.ReadFile(msg)
	if err := g.StampMessageFile(msg, nil); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(msg)
	if string(before) != string(after) {
		t.Fatalf("message changed with no session in scope:\n%s", after)
	}
}

func TestValidateSessionID(t *testing.T) {
	cases := []struct {
		name string
		id   string
		ok   bool
	}{
		{"uuid", "3f0a9c2e-1b7d-4e6a-9c2e-1b7d4e6a9c2e", true},
		{"empty", "", false},
		{"whitespace", "   ", false},
		{"newline_injection", "sess-A\nOpenBox-Session: sess-evil", false},
		{"carriage_return", "sess\rA", false},
		{"nul", "sess\x00A", false},
		{"internal_space", "my great feature", false}, // F5: prose is not an id
		{"tab", "a\tb", false},
		{"secret_shaped", "obx_livekey_deadbeef", false}, // INV-1: never a credential
		{"too_long", string(make([]byte, maxSessionIDLen+1)), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateSessionID(c.id)
			if (err == nil) != c.ok {
				t.Fatalf("validateSessionID(%q) err=%v, want ok=%v", c.id, err, c.ok)
			}
		})
	}
}

// A secret-shaped or malformed value must never reach the commit message.
func TestStamp_DropsInvalidNeverWritesSecret(t *testing.T) {
	g := Git{}
	msg := writeMsg(t, "work\n")
	if err := g.StampMessageFile(msg, []string{"obx_supersecret", "sess-good", ""}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(msg)
	if got := string(data); contains(got, "obx_") {
		t.Fatalf("secret leaked into commit message:\n%s", got)
	}
	got, _ := g.ReadTrailers(msg)
	if !reflect.DeepEqual(got, []string{"sess-good"}) {
		t.Fatalf("trailers = %v, want [sess-good]", got)
	}
}

func TestValidSessionIDs_DedupePreservesOrder(t *testing.T) {
	got := validSessionIDs([]string{"b", "a", "b", "obx_x", "a", "c"})
	want := []string{"b", "a", "c"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("validSessionIDs = %v, want %v", got, want)
	}
}

// F5: the harvest gate drops prose (whitespace) and secret-shaped values, so a
// column-0 "OpenBox-Session: some prose" body line can never be hoisted into a
// resolvable trailer.
func TestValidSessionIDs_DropsProseAndSecrets(t *testing.T) {
	got := validSessionIDs([]string{"good-id", "prose with spaces", "obx_secret", "also\tbad"})
	want := []string{"good-id"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("validSessionIDs = %v, want %v", got, want)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
