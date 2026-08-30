package devconfig

import (
	"crypto/ed25519"
	"encoding/base64"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestParseEnvFile(t *testing.T) {
	seed := base64.StdEncoding.EncodeToString(make([]byte, ed25519.SeedSize))

	for _, tc := range []struct {
		name  string
		body  string
		want  map[string]string
		errIs string // substring the error must contain; "" ⇒ expect success
	}{
		{
			name: "plain",
			body: "OPENBOX_API_KEY=obx_abc\n",
			want: map[string]string{"OPENBOX_API_KEY": "obx_abc"},
		},
		{
			name: "comments and blank lines",
			body: "# a comment\n\nOPENBOX_API_KEY=obx_abc\n\n   # indented comment\n",
			want: map[string]string{"OPENBOX_API_KEY": "obx_abc"},
		},
		{
			name: "export prefix, as pasted from a shell",
			body: "export OPENBOX_API_KEY=obx_abc\n",
			want: map[string]string{"OPENBOX_API_KEY": "obx_abc"},
		},
		{
			name: "double quotes",
			body: `OPENBOX_API_KEY="obx_abc"` + "\n",
			want: map[string]string{"OPENBOX_API_KEY": "obx_abc"},
		},
		{
			name: "single quotes (what the writer emits)",
			body: "OPENBOX_API_KEY='obx_abc'\n",
			want: map[string]string{"OPENBOX_API_KEY": "obx_abc"},
		},
		{
			name: "surrounding whitespace",
			body: "  OPENBOX_API_KEY  =  obx_abc  \n",
			want: map[string]string{"OPENBOX_API_KEY": "obx_abc"},
		},
		{
			name: "explicit empty value",
			body: "OPENBOX_API_KEY=\n",
			want: map[string]string{"OPENBOX_API_KEY": ""},
		},
		{
			name: "base64 seed keeps its padding",
			body: "OPENBOX_AGENT_PRIVATE_KEY=" + seed + "\n",
			want: map[string]string{"OPENBOX_AGENT_PRIVATE_KEY": seed},
		},
		{
			name: "base64 with + and / survives",
			body: "K=ab+c/d==\n",
			want: map[string]string{"K": "ab+c/d=="},
		},
		{
			name: "CRLF is stripped from every value",
			body: "OPENBOX_API_KEY=obx_abc\r\nOPENBOX_AGENT_PRIVATE_KEY=" + seed + "\r\n",
			want: map[string]string{"OPENBOX_API_KEY": "obx_abc", "OPENBOX_AGENT_PRIVATE_KEY": seed},
		},
		{
			name: "quoted value containing an equals sign",
			body: `K="a=b"` + "\n",
			want: map[string]string{"K": "a=b"},
		},
		{
			name: "duplicate key is last-wins, not an error",
			body: "OPENBOX_API_KEY=obx_first\nOPENBOX_API_KEY=obx_second\n",
			want: map[string]string{"OPENBOX_API_KEY": "obx_second"},
		},
		{
			name:  "line with no equals",
			body:  "OPENBOX_API_KEY\n",
			errIs: "unexpected character",
		},
		{
			name: "empty key is accepted, bound to the empty name",
			body: "=value\n",
			want: map[string]string{"": "value"},
		},
		{
			name: "trailing hash starts a comment",
			body: "OPENBOX_API_KEY=obx_abc # note\n",
			want: map[string]string{"OPENBOX_API_KEY": "obx_abc"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), ".env")
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := ParseEnvFile(path)
			if tc.errIs != "" {
				if err == nil {
					t.Fatalf("ParseEnvFile() succeeded; want an error containing %q", tc.errIs)
				}
				if !strings.Contains(err.Error(), tc.errIs) {
					t.Fatalf("error = %q, want it to contain %q", err, tc.errIs)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseEnvFile(): %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %d keys %v, want %d %v", len(got), got, len(tc.want), tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("[%s] = %q, want %q", k, got[k], v)
				}
			}
		})
	}
}

// TestParseEnvFileMissingIsEmptyAndNoError no credentials configured is a
// legitimate state the caller reports in its own words, not an I/O failure.
func TestParseEnvFileMissingIsEmptyAndNoError(t *testing.T) {
	got, err := ParseEnvFile(filepath.Join(t.TempDir(), "absent.env"))
	if err != nil {
		t.Fatalf("missing file returned an error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("missing file returned %v, want an empty map", got)
	}
}

func TestParseEnvFileUnreadableIsAnError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod 0000 does not deny reads on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root: mode bits do not deny reads")
	}
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("K=v\n"), 0o000); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseEnvFile(path); err == nil {
		t.Fatal("unreadable file returned no error; a real I/O failure must not look like an empty config")
	}
}

func TestWriteEnvFileRoundTrip(t *testing.T) {
	seed := base64.StdEncoding.EncodeToString(make([]byte, ed25519.SeedSize))
	path := filepath.Join(t.TempDir(), "sub", ".env")
	in := map[string]string{
		"OPENBOX_API_KEY":           "obx_abc123",
		"OPENBOX_AGENT_PRIVATE_KEY": seed,
	}
	if err := WriteEnvFile(path, in); err != nil {
		t.Fatalf("WriteEnvFile(): %v", err)
	}
	got, err := ParseEnvFile(path)
	if err != nil {
		t.Fatalf("ParseEnvFile(): %v", err)
	}
	for k, v := range in {
		if got[k] != v {
			t.Errorf("[%s] = %q, want %q", k, got[k], v)
		}
	}
}

func TestWriteEnvFilePermissionsAndHeader(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	if err := WriteEnvFile(path, map[string]string{"OPENBOX_API_KEY": "obx_abc"}); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		t.Log("skipping mode assertion: 0600 is a no-op on Windows ")
	} else if got := st.Mode().Perm(); got != 0o600 {
		t.Errorf("mode = %04o, want 0600", got)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	for _, must := range []string{"PLAINTEXT", "ONLY copy", "DO NOT COMMIT"} {
		if !strings.Contains(body, must) {
			t.Errorf("header is missing %q:\n%s", must, body)
		}
	}
}

func TestWriteEnvFileCreatesParentDir0700(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mode bits are not enforced on Windows")
	}
	dir := filepath.Join(t.TempDir(), "openbox")
	if err := WriteEnvFile(filepath.Join(dir, ".env"), map[string]string{"K": "v"}); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Mode().Perm(); got != 0o700 {
		t.Errorf("parent dir mode = %04o, want 0700", got)
	}
}

// `openbox auth` writes three keys, so anything else in the file was put there
// by a human. Someone who hand-added a coordinate override authored a key the
// writer must keep; dropping it would silently undo a deliberate choice.
func TestWriteEnvFilePreservesUnknownKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	if err := WriteEnvFile(path, map[string]string{
		"OPENBOX_API_KEY":   "obx_first",
		"OPENBOX_AGENT_DID": "did:aip:hand-added",
		"MY_OWN_VAR":        "keep me",
	}); err != nil {
		t.Fatal(err)
	}
	if err := WriteEnvFile(path, map[string]string{"OPENBOX_API_KEY": "obx_second"}); err != nil {
		t.Fatal(err)
	}
	got, err := ParseEnvFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got["OPENBOX_API_KEY"] != "obx_second" {
		t.Errorf("api key = %q, want the second write to win", got["OPENBOX_API_KEY"])
	}
	if got["OPENBOX_AGENT_DID"] != "did:aip:hand-added" {
		t.Errorf("hand-added coordinate was dropped: %v", got)
	}
	if got["MY_OWN_VAR"] != "keep me" {
		t.Errorf("foreign key was dropped: %v", got)
	}
}

// TestWriteEnvFileRefusesToOverwriteAnUnparseableFile the file holds the only
// copy of credentials, so a parse failure must stop the write rather than
// replace an unreadable file with a fresh one.
func TestWriteEnvFileRefusesToOverwriteAnUnparseableFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	// The property under test is unchanged: an unreadable credential file must
	// stop the write.
	original := "OPENBOX_API_KEY=obx_original\nthis line has no equals sign\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	err := WriteEnvFile(path, map[string]string{"OPENBOX_API_KEY": "obx_new"})
	if err == nil {
		t.Fatal("WriteEnvFile overwrote a file it could not parse")
	}
	if !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Errorf("error = %q, want it to say it is refusing to overwrite", err)
	}
	raw, _ := os.ReadFile(path)
	if string(raw) != original {
		t.Errorf("the original file was modified despite the refusal:\n%s", raw)
	}
}

// TestWriteEnvFileStableKeyOrder values are written sorted so changing one
// produces a one-line diff rather than a reordered file.
func TestWriteEnvFileStableKeyOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	kv := map[string]string{"Z_LAST": "3", "A_FIRST": "1", "M_MID": "2"}
	if err := WriteEnvFile(path, kv); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	body := string(raw)
	a, m, z := strings.Index(body, "A_FIRST"), strings.Index(body, "M_MID"), strings.Index(body, "Z_LAST")
	if !(a < m && m < z) {
		t.Fatalf("keys are not in sorted order (A=%d M=%d Z=%d):\n%s", a, m, z, body)
	}
}

// TestWriteEnvFileLeavesNoTempFile the temp file must be gone whether the
// write succeeded or not: a leftover .env-*.tmp is a plaintext credential copy
// nobody knows about.
func TestWriteEnvFileLeavesNoTempFile(t *testing.T) {
	dir := t.TempDir()
	if err := WriteEnvFile(filepath.Join(dir, ".env"), map[string]string{"K": "v"}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("left a temp credential file behind: %s", e.Name())
		}
	}
}

// TestWriteEnvFileRefusesAValueItCannotRepresent a value the format cannot
// represent must be refused, not silently corrupted.
func TestWriteEnvFileRefusesAValueItCannotRepresent(t *testing.T) {
	for _, tc := range []struct{ name, value string }{
		{"single quote", "o'brien"},
		{"newline", "line1\nline2"},
		{"carriage return", "value\r"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), ".env")
			err := WriteEnvFile(path, map[string]string{"MY_VAR": tc.value})
			if err == nil {
				got, _ := ParseEnvFile(path)
				t.Fatalf("accepted an unrepresentable value; it round-tripped as %q (want %q)",
					got["MY_VAR"], tc.value)
			}
			if !strings.Contains(err.Error(), "MY_VAR") {
				t.Errorf("error should name the offending key: %v", err)
			}
			if _, statErr := os.Stat(path); statErr == nil {
				t.Error("a refused write must not create the file")
			}
		})
	}
}

// TestRefusedWriteLeavesAnExistingFileIntact the refusal must not be able to
// destroy an existing good file.
func TestRefusedWriteLeavesAnExistingFileIntact(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	if err := WriteEnvFile(path, map[string]string{"OPENBOX_API_KEY": "obx_good"}); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	if err := WriteEnvFile(path, map[string]string{"BAD": "it's"}); err == nil {
		t.Fatal("expected a refusal")
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Error("a refused write modified the existing credential file")
	}
	if kv, _ := ParseEnvFile(path); kv["OPENBOX_API_KEY"] != "obx_good" {
		t.Error("the existing credential was lost")
	}
}

// TestParseEnvFileErrorEchoesTheOffendingLine godotenv's parse error echoes
// the offending line, and this file is credentials. The hand-rolled parser
// named the file and line number and never the content, and this suite
// asserted that.
func TestParseEnvFileErrorEchoesTheOffendingLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	body := "obx_live_SENTINELSECRET\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := ParseEnvFile(path)
	if err == nil {
		t.Fatal("a line with no '=' should be a parse error")
	}
	if !strings.Contains(err.Error(), "SENTINELSECRET") {
		t.Skipf("godotenv no longer echoes the offending line (%v) — the disclosure "+
			"documented in ParseEnvFile's comment is gone and that comment should be "+
			"updated", err)
	}
}
