//go:build darwin || linux

package snapshot

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestDefaultExclusionMatrixRecordsPathsWithoutContents(t *testing.T) {
	parent := shortTempDir(t)
	root := filepath.Join(parent, "project")
	for _, directory := range []string{
		filepath.Join(root, "src"),
		filepath.Join(root, ".git"),
		filepath.Join(root, "node_modules", "dependency"),
		filepath.Join(root, ".cache"),
		filepath.Join(root, ".openbox", "audit", "run-1"),
		filepath.Join(root, ".openbox", "audit", "prior-run"),
		filepath.Join(root, "nested", ".openbox", "audit", "prior-run"),
		filepath.Join(root, ".openbox", "inspect", "prior-inspection"),
		filepath.Join(root, "nested", ".openbox", "inspect", "prior-inspection"),
		filepath.Join(root, ".openbox", "tmp"),
		filepath.Join(root, ".aws"),
		filepath.Join(root, "upper", ".GIT"),
		filepath.Join(root, "upper", ".CACHE"),
		filepath.Join(root, "upper", ".OPENBOX", "AUDIT", "prior-run"),
		filepath.Join(root, "upper", ".OPENBOX", "INSPECT", "prior-inspection"),
	} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	const secretValue = "OPENBOX_TEST_SECRET_VALUE_MUST_NOT_APPEAR"
	writeMode(t, filepath.Join(root, "src", "index.ts"), []byte("export const safe = true\n"), 0o644)
	writeMode(t, filepath.Join(root, ".git", "config"), []byte(secretValue), 0o600)
	writeMode(t, filepath.Join(root, "node_modules", "dependency", "index.js"), []byte(secretValue), 0o600)
	writeMode(t, filepath.Join(root, ".cache", "cached-token"), []byte(secretValue), 0o600)
	writeMode(t, filepath.Join(root, ".openbox", "audit", "run-1", "old-pack"), []byte(secretValue), 0o600)
	writeMode(t, filepath.Join(root, ".openbox", "audit", "prior-run", "old-pack"), []byte(secretValue), 0o600)
	writeMode(t, filepath.Join(root, "nested", ".openbox", "audit", "prior-run", "old-pack"), []byte(secretValue), 0o600)
	writeMode(t, filepath.Join(root, ".openbox", "inspect", "prior-inspection", "project-model.json"), []byte(secretValue), 0o600)
	writeMode(t, filepath.Join(root, "nested", ".openbox", "inspect", "prior-inspection", "sdk-coverage.json"), []byte(secretValue), 0o600)
	writeMode(t, filepath.Join(root, ".openbox", "tmp", "old-snapshot"), []byte(secretValue), 0o600)
	for _, name := range []string{".env", ".env.production", ".npmrc", "credentials.json", "id_rsa", "tls.pem"} {
		writeMode(t, filepath.Join(root, name), []byte(secretValue), 0o600)
	}
	writeMode(t, filepath.Join(root, secretValue+".pem"), []byte(secretValue), 0o600)
	writeMode(t, filepath.Join(root, ".aws", "credentials"), []byte(secretValue), 0o600)
	writeMode(t, filepath.Join(root, "upper", ".GIT", "config"), []byte(secretValue), 0o600)
	writeMode(t, filepath.Join(root, "upper", ".CACHE", "cached-token"), []byte(secretValue), 0o600)
	writeMode(t, filepath.Join(root, "upper", ".OPENBOX", "AUDIT", "prior-run", "old-pack"), []byte(secretValue), 0o600)
	writeMode(t, filepath.Join(root, "upper", ".OPENBOX", "INSPECT", "prior-inspection", "project-model.json"), []byte(secretValue), 0o600)
	for index := 0; index < 18; index++ {
		writeMode(t, filepath.Join(root, fmt.Sprintf(".env.%02d", index)), []byte(secretValue), 0o600)
	}
	outside := filepath.Join(parent, "outside.txt")
	writeMode(t, outside, []byte(secretValue), 0o600)
	if err := os.Symlink(outside, filepath.Join(root, "external-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(parent, "missing"), filepath.Join(root, "broken-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("src/index.ts", filepath.Join(root, "internal-link")); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(root, "events.pipe"), 0o600); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", filepath.Join(root, "events.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	project, err := Resolve(root, Boundaries{
		AuditOutput: filepath.Join(root, ".openbox", "audit", "run-1"),
		TempParent:  filepath.Join(root, ".openbox", "tmp"),
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := copyToPrivateDirectory(t, project, filepath.Join(parent, "snapshot"))
	repeated := copyToPrivateDirectory(t, project, filepath.Join(parent, "snapshot-repeated"))
	if snapshot.SelectionDigest() != repeated.SelectionDigest() ||
		!reflect.DeepEqual(snapshot.SelectionRules(), repeated.SelectionRules()) ||
		!reflect.DeepEqual(snapshot.Omissions(), repeated.Omissions()) {
		t.Fatal("repeated exclusion metadata is not deterministic")
	}
	files := snapshot.Files()
	if len(files) != 1 {
		t.Fatalf("copied files = %#v", files)
	}
	if got, want := files, []File{{
		Path:       "src/index.ts",
		Digest:     files[0].Digest,
		Size:       int64(len("export const safe = true\n")),
		Executable: false,
	}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("copied files = %#v", got)
	}
	for _, omitted := range []string{
		".git", "node_modules", ".cache", ".env", ".npmrc", "credentials.json",
		"id_rsa", "tls.pem", ".aws/credentials", ".openbox/audit/run-1", ".openbox/audit/prior-run", "nested/.openbox/audit/prior-run",
		".openbox/inspect/prior-inspection", "nested/.openbox/inspect/prior-inspection", ".openbox/tmp",
		"upper/.GIT", "upper/.CACHE", "upper/.OPENBOX/AUDIT", "upper/.OPENBOX/INSPECT",
		"external-link", "broken-link", "internal-link", "events.pipe", "events.sock",
	} {
		if _, err := os.Lstat(filepath.Join(snapshot.Root(), filepath.FromSlash(omitted))); !os.IsNotExist(err) {
			t.Fatalf("omitted path %q exists in snapshot: %v", omitted, err)
		}
	}

	ruleIDs := make(map[string]struct{})
	for _, rule := range snapshot.SelectionRules() {
		if _, duplicate := ruleIDs[rule.ID]; duplicate {
			t.Fatalf("duplicate rule ID %q", rule.ID)
		}
		ruleIDs[rule.ID] = struct{}{}
	}
	for _, required := range []string{
		"builtin-vcs", "builtin-audit-output", "builtin-inspection-output", "builtin-cache", "builtin-secret", "builtin-socket", "builtin-fifo",
		"builtin-device", "builtin-external-symlink", "builtin-external-mount",
		"exclude-audit-output", "exclude-run-temp",
	} {
		if _, ok := ruleIDs[required]; !ok {
			t.Fatalf("missing selection rule %q", required)
		}
	}

	omissions := make(map[string]Omission)
	for _, omission := range snapshot.Omissions() {
		if omission.Count <= 0 || len(omission.Examples) > 16 ||
			omission.ExamplesTruncated != (int64(len(omission.Examples)) < omission.Count) {
			t.Fatalf("invalid omission = %#v", omission)
		}
		if _, duplicate := omissions[omission.RuleID]; duplicate {
			t.Fatalf("duplicate omission rule %q", omission.RuleID)
		}
		omissions[omission.RuleID] = omission
	}
	wantClasses := map[string]PathClass{
		"builtin-vcs":               PathClassVCS,
		"builtin-audit-output":      PathClassAuditOutput,
		"builtin-inspection-output": PathClassAuditOutput,
		"builtin-cache":             PathClassCache,
		"builtin-secret":            PathClassSecret,
		"builtin-socket":            PathClassSocket,
		"builtin-fifo":              PathClassFIFO,
		"builtin-external-symlink":  PathClassExternalSymlink,
		"exclude-run-temp":          PathClassIgnored,
	}
	for ruleID, wantClass := range wantClasses {
		if got, ok := omissions[ruleID]; !ok || got.PathClass != wantClass {
			t.Fatalf("omission %s = %#v, want class %s", ruleID, got, wantClass)
		}
	}
	if secret := omissions["builtin-secret"]; secret.Count != 26 || !secret.ExamplesTruncated || len(secret.Examples) != 0 {
		t.Fatalf("secret omission = %#v", secret)
	}

	metadata, err := json.Marshal(struct {
		Manifest  json.RawMessage `json:"manifest"`
		Rules     []Rule          `json:"rules"`
		Omissions []Omission      `json:"omissions"`
	}{Manifest: snapshot.Manifest(), Rules: snapshot.SelectionRules(), Omissions: snapshot.Omissions()})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(metadata), secretValue) {
		t.Fatalf("secret value leaked into snapshot metadata: %s", metadata)
	}
}

func TestSpecialKindExclusionVocabularyIsClosed(t *testing.T) {
	for _, test := range []struct {
		kind  Kind
		class PathClass
		rule  string
	}{
		{kind: KindDevice, class: PathClassDevice, rule: "builtin-device"},
		{kind: KindOther, class: PathClassDevice, rule: "builtin-device"},
		{kind: KindExternalMount, class: PathClassIgnored, rule: "builtin-external-mount"},
	} {
		match, ok := matchEntry(Entry{Path: "fixture", Kind: test.kind})
		if !ok || match.class != test.class || match.ruleID != test.rule {
			t.Fatalf("kind %s match = %#v, %t", test.kind, match, ok)
		}
	}
}
