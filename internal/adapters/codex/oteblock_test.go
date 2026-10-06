package codex

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testEndpoint = "http://127.0.0.1:4318/v1/logs"

func TestWriteOtelCreatesTheOwnedBlock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := WriteOtel(path, testEndpoint); err != nil {
		t.Fatalf("WriteOtel: %v", err)
	}
	if !HasOwnedOtel(path) {
		t.Fatal("HasOwnedOtel = false right after WriteOtel wrote the block")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	for _, want := range []string{
		otelEnvironmentMarker, testEndpoint, otelExporterProtocol, "otlp-http",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("config.toml does not contain %q:\n%s", want, body)
		}
	}
}

// TestWriteOtelIsByteIdempotent: re-running init produces a byte-identical
// config.toml.
func TestWriteOtelIsByteIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := WriteOtel(path, testEndpoint); err != nil {
		t.Fatalf("first WriteOtel: %v", err)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteOtel(path, testEndpoint); err != nil {
		t.Fatalf("second WriteOtel: %v", err)
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatalf("re-running WriteOtel changed the file:\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
}

// TestWriteOtelLeavesForeignKeysAtAnyDepthUntouched is the ownership-aware
// merge's whole point: a substring scan could misidentify a foreign block and
// delete a developer's own configuration.
func TestWriteOtelLeavesForeignKeysAtAnyDepthUntouched(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	seed := `
model = "o3"

[history]
persistence = "save-all"

[projects."/some/repo"]
trust_level = "trusted"
`
	if err := os.WriteFile(path, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteOtel(path, testEndpoint); err != nil {
		t.Fatalf("WriteOtel: %v", err)
	}
	doc, err := readTOMLDoc(path)
	if err != nil {
		t.Fatal(err)
	}
	if doc["model"] != "o3" {
		t.Errorf("top-level foreign key model = %v, want o3", doc["model"])
	}
	history, _ := doc["history"].(map[string]any)
	if history["persistence"] != "save-all" {
		t.Errorf("foreign [history] table not preserved: %v", doc["history"])
	}
	projects, _ := doc["projects"].(map[string]any)
	repo, _ := projects["/some/repo"].(map[string]any)
	if repo["trust_level"] != "trusted" {
		t.Errorf("foreign nested [projects.\"/some/repo\"] table not preserved: %v", doc["projects"])
	}
	if !HasOwnedOtel(path) {
		t.Error("the owned [otel] block was not written alongside the foreign keys")
	}
}

// TestWriteOtelRefusesAForeignOtelBlock mirrors the hooks.json ownership
// model: an [otel] table that exists but carries no OpenBox marker is
// somebody else's, and this must refuse rather than guess.
func TestWriteOtelRefusesAForeignOtelBlock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	seed := `
[otel]
environment = "my-own-otel-setup"

[otel.exporter.otlp-http]
endpoint = "https://my-observability-vendor.example/v1/logs"
`
	if err := os.WriteFile(path, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteOtel(path, testEndpoint); err == nil {
		t.Fatal("WriteOtel silently overwrote a foreign [otel] block")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "my-observability-vendor.example") {
		t.Error("the foreign [otel] block was modified despite the refusal")
	}
}

// TestRemoveOtelLeavesAForeignBlockIntact is the uninstall-side mirror: the
// unconditional sweep must never delete a block it does not own.
func TestRemoveOtelLeavesAForeignBlockIntact(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	seed := `
[otel]
environment = "my-own-otel-setup"
`
	if err := os.WriteFile(path, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}
	removed, err := RemoveOtel(path)
	if err != nil {
		t.Fatalf("RemoveOtel: %v", err)
	}
	if removed {
		t.Fatal("RemoveOtel reported removing a foreign block")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "my-own-otel-setup") {
		t.Error("the foreign [otel] block was deleted")
	}
}

// TestRemoveOtelRemovesOnlyTheOwnedBlock pins the uninstall contract: remove
// what OpenBox wrote and nothing else in config.toml.
func TestRemoveOtelRemovesOnlyTheOwnedBlock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	seed := "model = \"o3\"\n"
	if err := os.WriteFile(path, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteOtel(path, testEndpoint); err != nil {
		t.Fatalf("WriteOtel: %v", err)
	}
	removed, err := RemoveOtel(path)
	if err != nil {
		t.Fatalf("RemoveOtel: %v", err)
	}
	if !removed {
		t.Fatal("RemoveOtel reported no removal for a block it owns")
	}
	if HasOwnedOtel(path) {
		t.Error("the owned [otel] block survived RemoveOtel")
	}
	doc, err := readTOMLDoc(path)
	if err != nil {
		t.Fatal(err)
	}
	if doc["model"] != "o3" {
		t.Errorf("the foreign top-level key was lost by removal: %v", doc)
	}
}

// TestRemoveOtelOnAnAbsentFileIsSuccess mirrors RemoveHooks: an absent file
// is success, because uninstall walks every surface unconditionally.
func TestRemoveOtelOnAnAbsentFileIsSuccess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist", "config.toml")
	removed, err := RemoveOtel(path)
	if err != nil {
		t.Fatalf("RemoveOtel on an absent file: %v", err)
	}
	if removed {
		t.Fatal("RemoveOtel reported removing something from a file that never existed")
	}
}

// TestConfigTOMLPathHonoursCodexHome mirrors defaultHooksPath's own test.
func TestConfigTOMLPathHonoursCodexHome(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CODEX_HOME", dir)
	want := filepath.Join(dir, "config.toml")
	if got := ConfigTOMLPath(); got != want {
		t.Errorf("ConfigTOMLPath() = %q, want %q", got, want)
	}
}

// TestWriteOtelRefusesUnparsableTOML mirrors writeHooks' refusal of
// unparsable JSON: never clobber a file this parser cannot understand.
func TestWriteOtelRefusesUnparsableTOML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("this is not [ valid toml"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteOtel(path, testEndpoint); err == nil {
		t.Fatal("WriteOtel silently modified unparsable TOML")
	}
}

// handFormattedConfig is a config.toml a developer actually annotated:
// comments, blank-line grouping and key ordering that a whole-document
// canonical re-encode would silently discard. It ends with a trailing
// newline, the shape every file this installer ever writes has.
const handFormattedConfig = `# my Codex config -- do not remove the comments below
model = "o3" # pinned; o4 regressed on our repo

# trust the repos I actually work in
[projects."/home/dev/work/repo-a"]
trust_level = "trusted"

[projects."/home/dev/work/repo-b"]
trust_level = "trusted"

[history]
persistence = "save-all"
`

// TestWriteOtelPreservesCommentsAndFormattingByteForByte is the Medium
// finding this fixes: go-toml/v2 has no comment or ordering model, so a
// whole-document decode/re-encode silently drops every comment and
// reorders every key. WriteOtel must splice its own block in and out of the
// raw bytes instead, leaving everything outside that block byte-for-byte
// untouched.
func TestWriteOtelPreservesCommentsAndFormattingByteForByte(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(handFormattedConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteOtel(path, testEndpoint); err != nil {
		t.Fatalf("WriteOtel: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	if !strings.HasPrefix(body, handFormattedConfig) {
		t.Fatalf("WriteOtel did not leave the original bytes untouched at the head of the file:\n%s", body)
	}
	for _, want := range []string{
		"# my Codex config -- do not remove the comments below",
		"# pinned; o4 regressed on our repo",
		"# trust the repos I actually work in",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("a comment was dropped by WriteOtel: missing %q\n%s", want, body)
		}
	}
	if !HasOwnedOtel(path) {
		t.Error("the owned [otel] block was not written alongside the preserved formatting")
	}
}

// TestWriteOtelThenRemoveOtelRoundTripsToTheOriginalBytes is the round-trip
// half: writing then removing the block must restore the file to EXACTLY
// what it was before, not merely to something that reparses the same way.
func TestWriteOtelThenRemoveOtelRoundTripsToTheOriginalBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(handFormattedConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteOtel(path, testEndpoint); err != nil {
		t.Fatalf("WriteOtel: %v", err)
	}
	removed, err := RemoveOtel(path)
	if err != nil {
		t.Fatalf("RemoveOtel: %v", err)
	}
	if !removed {
		t.Fatal("RemoveOtel reported no removal for a block it just wrote")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != handFormattedConfig {
		t.Fatalf("write then remove did not round-trip to the original bytes:\n--- want ---\n%s\n--- got ---\n%s", handFormattedConfig, string(raw))
	}
}

// TestWriteOtelRerunIsByteIdempotentOnAHandFormattedFile is
// TestWriteOtelIsByteIdempotent against a file with comments and foreign
// formatting, so the splice path (not just the fresh-file path) is pinned.
func TestWriteOtelRerunIsByteIdempotentOnAHandFormattedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(handFormattedConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteOtel(path, testEndpoint); err != nil {
		t.Fatalf("first WriteOtel: %v", err)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteOtel(path, testEndpoint); err != nil {
		t.Fatalf("second WriteOtel: %v", err)
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatalf("re-running WriteOtel on a hand-formatted file changed it:\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
}
