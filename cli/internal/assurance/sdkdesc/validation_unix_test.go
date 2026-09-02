//go:build darwin || linux

package sdkdesc

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/inspect"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/snapshot"
)

func TestValidateManifestsRequiresOneQualifiedPackageLock(t *testing.T) {
	initializations := []Initialization{qualifiedInitialization()}
	source := SourceAttestation{Commit: MastraSourceCommit, ArchiveSHA256: MastraArchiveSHA256}
	tests := []struct {
		name  string
		files map[string][]byte
		code  string
	}{
		{name: "synthetic root lookalike", files: map[string][]byte{
			"package.json":      []byte(`{"name":"@openbox-ai/openbox-mastra-sdk","version":"1.0.0"}`),
			"package-lock.json": packageLockFixture(t, true, nil),
		}, code: "source_drift"},
		{name: "missing lock", files: map[string][]byte{
			"package.json": []byte(`{"name":"fixture"}`),
		}, code: "ambiguous_package_lock"},
		{name: "multiple locks", files: map[string][]byte{
			"package.json":                []byte(`{"name":"@openbox-ai/openbox-mastra-sdk","version":"1.0.0"}`),
			"package-lock.json":           packageLockFixture(t, false, nil),
			"workspace/package-lock.json": packageLockFixture(t, false, nil),
		}, code: "ambiguous_package_lock"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manifests := readFixtureManifests(t, test.files)
			result := ValidateManifests(manifests, source, initializations)
			if result.Status != NotRunnable || !hasProblem(result, test.code) {
				t.Fatalf("result = %#v, want not_runnable with %q", result, test.code)
			}
		})
	}
}

func TestValidateManifestsSelectedMastraMVPClone(t *testing.T) {
	projectRoot := os.Getenv("OPENBOX_MASTRA_SDK_ROOT")
	if projectRoot == "" {
		t.Skip("set OPENBOX_MASTRA_SDK_ROOT to the qualified local clone for the MVP integration")
	}
	destinationParent := t.TempDir()
	destination := filepath.Join(destinationParent, "snapshot")
	if err := os.Mkdir(destination, 0o700); err != nil {
		t.Fatal(err)
	}
	project, err := snapshot.Resolve(projectRoot, snapshot.Boundaries{
		AuditOutput: filepath.Join(projectRoot, ".openbox", "audit", "current"),
		TempParent:  destinationParent,
	})
	if err != nil {
		t.Fatal(err)
	}
	copied, err := project.Copy(destination)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { makeFixtureWritable(destination) })
	manifests, err := inspect.ReadManifests(copied)
	if err != nil {
		t.Fatal(err)
	}
	result := ValidateManifests(
		manifests,
		SourceAttestation{Commit: MastraSourceCommit, ArchiveSHA256: MastraArchiveSHA256},
		[]Initialization{qualifiedInitialization()},
	)
	if result.Status != Compatible || len(result.Problems) != 0 {
		t.Fatalf("qualified local clone rejected: %#v", result)
	}
}

func readFixtureManifests(t *testing.T, files map[string][]byte) []inspect.Manifest {
	t.Helper()
	projectRoot := t.TempDir()
	for relative, content := range files {
		full := filepath.Join(projectRoot, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	destinationParent := t.TempDir()
	destination := filepath.Join(destinationParent, "snapshot")
	if err := os.Mkdir(destination, 0o700); err != nil {
		t.Fatal(err)
	}
	project, err := snapshot.Resolve(projectRoot, snapshot.Boundaries{
		AuditOutput: filepath.Join(projectRoot, ".openbox", "audit", "current"),
		TempParent:  destinationParent,
	})
	if err != nil {
		t.Fatal(err)
	}
	copied, err := project.Copy(destination)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { makeFixtureWritable(destination) })
	manifests, err := inspect.ReadManifests(copied)
	if err != nil {
		t.Fatal(err)
	}
	return manifests
}

func makeFixtureWritable(root string) {
	_ = filepath.Walk(root, func(current string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if info.IsDir() {
			_ = os.Chmod(current, 0o700)
		} else {
			_ = os.Chmod(current, 0o600)
		}
		return nil
	})
}
