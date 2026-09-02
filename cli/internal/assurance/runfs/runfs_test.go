package runfs

// WritePrivateFile has its security-sensitive behavior covered alongside the
// broader workspace lifecycle tests in this package.

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

const canonicalManifest = `{"apiVersion":"openbox.audit-pack/v1","kind":"AuditPack"}`

func TestWritePrivateFileIsExclusiveHandleRelativeAndPrivate(t *testing.T) {
	root := filepath.Join(t.TempDir(), "execution")
	workspace, err := Create(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := workspace.WritePrivateFile("execution.json", []byte("{}")); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(root, "execution.json"))
	if err != nil || string(content) != "{}" {
		t.Fatalf("content=%q err=%v", content, err)
	}
	assertMode(t, filepath.Join(root, "execution.json"), 0o600)
	if err := workspace.WritePrivateFile("execution.json", []byte("replacement")); err == nil {
		t.Fatal("overwrote private file")
	}
	for _, name := range []string{"", "../escape", "nested/file", IncompleteMarkerName, ManifestName, finalizeLockName, cleanupMarkerName} {
		if err := workspace.WritePrivateFile(name, []byte("x")); err == nil {
			t.Fatalf("accepted name %q", name)
		}
	}

	moved := root + ".moved"
	if err := os.Rename(root, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), root); err != nil {
		t.Fatal(err)
	}
	if err := workspace.WritePrivateFile("after-replacement", []byte("x")); err == nil {
		t.Fatal("wrote through replaced output root")
	}
	if _, err := os.Lstat(filepath.Join(root, "after-replacement")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("replacement target changed: %v", err)
	}
}

func TestCreateAndFinalizeManifestLast(t *testing.T) {
	root := filepath.Join(t.TempDir(), "run-1")
	workspace, err := Create(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = removeOwnedTree(root, workspace.identity)
	})
	objectDir := filepath.Join(root, "objects", "sha256")
	if err := os.MkdirAll(objectDir, 0o700); err != nil {
		t.Fatal(err)
	}
	objectPath := filepath.Join(objectDir, "2958d416d08aa5a472d7b509036cb7eafd542add84527e66a145ea64cb4cdc75")
	if err := os.WriteFile(objectPath, []byte("object"), 0o600); err != nil {
		t.Fatal(err)
	}
	state, err := Inspect(root)
	if err != nil || state != StateIncomplete {
		t.Fatalf("initial state = %q, %v", state, err)
	}
	if _, err := os.Lstat(filepath.Join(root, ManifestName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("manifest exists before finalization: %v", err)
	}

	digest, err := workspace.Finalize([]byte(canonicalManifest))
	if err != nil {
		t.Fatal(err)
	}
	if got := digest.String(); got != "sha256:b6606159f9a8599ad6eb65d7343c4607bef64925263060d5e300bb23739c06a1" {
		t.Fatalf("manifest digest = %s", got)
	}
	state, err = Inspect(root)
	if err != nil || state != StateManifestCommitted {
		t.Fatalf("final state = %q, %v", state, err)
	}
	if _, err := os.Lstat(filepath.Join(root, IncompleteMarkerName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("incomplete marker remains: %v", err)
	}
	manifest, err := os.ReadFile(filepath.Join(root, ManifestName))
	if err != nil {
		t.Fatal(err)
	}
	if string(manifest) != canonicalManifest {
		t.Fatalf("manifest = %q", manifest)
	}
	assertMode(t, root, 0o500)
	assertMode(t, filepath.Join(root, ManifestName), 0o400)
	assertMode(t, filepath.Join(root, "objects"), 0o500)
	assertMode(t, objectDir, 0o500)
	assertMode(t, objectPath, 0o400)
	if file, err := os.OpenFile(objectPath, os.O_WRONLY, 0); err == nil {
		_ = file.Close()
		t.Fatal("expected finalized object write rejection")
	}

	if _, err := workspace.Finalize([]byte(`{}`)); err == nil {
		t.Fatal("expected finalized workspace rewrite rejection")
	}
	receipt, err := workspace.Cleanup()
	if err == nil || receipt.Before != StateManifestCommitted || receipt.Removed || !receipt.ExistsAfter {
		t.Fatalf("finalized cleanup = %+v, %v", receipt, err)
	}

}

func TestOpenInterruptedWorkspaceAndCleanup(t *testing.T) {
	root := filepath.Join(t.TempDir(), "run-interrupted")
	if _, err := Create(root); err != nil {
		t.Fatal(err)
	}
	recovered, err := OpenIncomplete(root)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := recovered.Cleanup()
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Before != StateIncomplete || !receipt.Removed || receipt.ExistsAfter {
		t.Fatalf("cleanup receipt = %+v", receipt)
	}
	state, err := Inspect(root)
	if err != nil || state != StateAbsent {
		t.Fatalf("cleaned state = %q, %v", state, err)
	}
	second, err := recovered.Cleanup()
	if err != nil || second.Before != StateAbsent || second.Removed || second.ExistsAfter {
		t.Fatalf("idempotent cleanup = %+v, %v", second, err)
	}
}

func TestFinalizeRejectsNonCanonicalAndExistingManifest(t *testing.T) {
	t.Run("non-canonical", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "run-noncanonical")
		workspace, err := Create(root)
		if err != nil {
			t.Fatal(err)
		}
		defer cleanupIncomplete(t, workspace)
		if _, err := workspace.Finalize([]byte(`{ "kind": "AuditPack" }`)); err == nil {
			t.Fatal("expected non-canonical manifest rejection")
		}
		if state, err := Inspect(root); err != nil || state != StateIncomplete {
			t.Fatalf("state = %q, %v", state, err)
		}
	})

	t.Run("existing manifest", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "run-existing")
		workspace, err := Create(root)
		if err != nil {
			t.Fatal(err)
		}
		defer cleanupIncomplete(t, workspace)
		manifestPath := filepath.Join(root, ManifestName)
		if err := os.WriteFile(manifestPath, []byte("sentinel"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := workspace.Finalize([]byte(canonicalManifest)); err == nil {
			t.Fatal("expected existing manifest rejection")
		}
		content, err := os.ReadFile(manifestPath)
		if err != nil || string(content) != "sentinel" {
			t.Fatalf("existing manifest changed: %q, %v", content, err)
		}
	})
}

func TestFinalizeRejectsUnsupportedPackEntry(t *testing.T) {
	root := filepath.Join(t.TempDir(), "run-extra-entry")
	workspace, err := Create(root)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanupIncomplete(t, workspace)
	if err := os.WriteFile(filepath.Join(root, "unreferenced-report.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.Finalize([]byte(canonicalManifest)); err == nil {
		t.Fatal("expected unsupported pack entry rejection")
	}
	if state, err := Inspect(root); err != nil || state != StateIncomplete {
		t.Fatalf("state = %q, %v", state, err)
	}
}

func TestManifestPublicationCannotReplaceRacingFile(t *testing.T) {
	root := filepath.Join(t.TempDir(), "run-racing-manifest")
	workspace, err := Create(root)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanupIncomplete(t, workspace)
	manifestPath := filepath.Join(root, ManifestName)
	workspace.beforePublish = func() error {
		return os.WriteFile(manifestPath, []byte("racing-sentinel"), 0o600)
	}
	if _, err := workspace.Finalize([]byte(canonicalManifest)); err == nil {
		t.Fatal("expected no-replace publication failure")
	}
	content, err := os.ReadFile(manifestPath)
	if err != nil || string(content) != "racing-sentinel" {
		t.Fatalf("racing manifest changed: %q, %v", content, err)
	}
}

func TestFinalizeRejectsExternalHardLinkWithoutMutatingIt(t *testing.T) {
	parent := t.TempDir()
	external := filepath.Join(parent, "external-source")
	if err := os.WriteFile(external, []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(parent, "run-hardlink")
	workspace, err := Create(root)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanupIncomplete(t, workspace)
	objectDir := filepath.Join(root, "objects", "sha256")
	if err := os.MkdirAll(objectDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(external, filepath.Join(objectDir, "41cf6794ba4200b839c53531555f0f3998df4cbb01a4d5cb0b94e3ca5e23947d")); err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.Finalize([]byte(canonicalManifest)); err == nil {
		t.Fatal("expected external hard-link rejection")
	}
	content, err := os.ReadFile(external)
	if err != nil || string(content) != "source" {
		t.Fatalf("external content changed: %q, %v", content, err)
	}
	assertMode(t, external, 0o600)
}

func TestFinalizeFailureRestoresIncompleteMarker(t *testing.T) {
	root := filepath.Join(t.TempDir(), "run-fault")
	workspace, err := Create(root)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	workspace.syncRoot = func(directory *os.File) error {
		calls++
		if calls == 2 {
			return errors.New("injected directory sync failure")
		}
		return syncOpenDirectory(directory)
	}
	if _, err := workspace.Finalize([]byte(canonicalManifest)); err == nil {
		t.Fatal("expected injected finalization failure")
	}
	state, err := Inspect(root)
	if err != nil || state != StateIncomplete {
		t.Fatalf("restored state = %q, %v", state, err)
	}
	recovered, err := OpenIncomplete(root)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := recovered.Cleanup()
	if err != nil || !receipt.Removed || receipt.ExistsAfter {
		t.Fatalf("recovered cleanup = %+v, %v", receipt, err)
	}
}

func TestInterruptedPublishedManifestRemainsRecoverable(t *testing.T) {
	root := filepath.Join(t.TempDir(), "run-published-interrupted")
	if _, err := Create(root); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(root, ManifestName)
	if err := os.WriteFile(manifestPath, []byte(canonicalManifest), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(manifestPath, 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, IncompleteMarkerName)); err != nil {
		t.Fatal(err)
	}
	state, err := Inspect(root)
	if err != nil || state != StateIncomplete {
		t.Fatalf("interrupted published state = %q, %v", state, err)
	}
	recovered, err := OpenIncomplete(root)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := recovered.Cleanup()
	if err != nil || !receipt.Removed || receipt.ExistsAfter {
		t.Fatalf("interrupted published cleanup = %+v, %v", receipt, err)
	}
}

func TestMarkerlessRecoveryNamesRequireExplicitOrphanCleanup(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".run.creating-deadbeefdeadbeef")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	state, err := Inspect(root)
	if err != nil || state != StateOrphaned {
		t.Fatalf("recovery state = %q, %v", state, err)
	}
	if recovered, err := OpenIncomplete(root); err == nil || recovered != nil {
		t.Fatal("markerless recovery name granted normal cleanup authority")
	}
	receipt, err := CleanupOrphan(root)
	if err != nil || !receipt.Removed || receipt.ExistsAfter {
		t.Fatalf("recovery cleanup = %+v, %v", receipt, err)
	}
}

func TestRecoveryLikeNameDoesNotAuthorizeDeletion(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".victim.creating-not-a-run")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(root, "preserve")
	if err := os.WriteFile(victim, []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	if state, err := Inspect(root); err == nil || state != StateInvalid {
		t.Fatalf("recovery-like directory = %q, %v", state, err)
	}
	if recovered, err := OpenIncomplete(root); err == nil || recovered != nil {
		t.Fatal("recovery-like name granted workspace authority")
	}
	if receipt, err := CleanupOrphan(root); err == nil || receipt.Removed {
		t.Fatalf("recovery-like orphan cleanup = %+v, %v", receipt, err)
	}
	content, err := os.ReadFile(victim)
	if err != nil || string(content) != "source" {
		t.Fatalf("unrelated content changed: %q, %v", content, err)
	}
}

func TestArbitraryEmptyDirectoryIsNotAnOrphan(t *testing.T) {
	root := filepath.Join(t.TempDir(), "unrelated-empty")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if state, err := Inspect(root); err == nil || state != StateInvalid {
		t.Fatalf("unrelated empty directory = %q, %v", state, err)
	}
	if receipt, err := CleanupOrphan(root); err == nil || receipt.Removed {
		t.Fatalf("unrelated empty cleanup = %+v, %v", receipt, err)
	}
	if _, err := os.Lstat(root); err != nil {
		t.Fatalf("unrelated empty directory changed: %v", err)
	}
}

func TestRecoveryNameWithValidMarkerUsesNormalCleanup(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".run.creating-deadbeefdeadbeef")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeExclusive(filepath.Join(root, IncompleteMarkerName), []byte(incompleteMarker), 0o600); err != nil {
		t.Fatal(err)
	}
	recovered, err := OpenIncomplete(root)
	if err != nil {
		t.Fatal(err)
	}
	if receipt, err := recovered.Cleanup(); err != nil || !receipt.Removed {
		t.Fatalf("sentinel-backed recovery cleanup = %+v, %v", receipt, err)
	}
}

func TestTruncatedCreationMarkerRequiresExplicitOrphanCleanup(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".run.creating-deadbeefdeadbeef")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := incompleteMarker[:len(incompleteMarker)/2]
	if err := writeExclusive(filepath.Join(root, IncompleteMarkerName), []byte(marker), 0o600); err != nil {
		t.Fatal(err)
	}
	if state, err := Inspect(root); err != nil || state != StateOrphaned {
		t.Fatalf("truncated-marker state = %q, %v", state, err)
	}
	if recovered, err := OpenIncomplete(root); err == nil || recovered != nil {
		t.Fatal("truncated marker granted normal cleanup authority")
	}
	if receipt, err := CleanupOrphan(root); err != nil || !receipt.Removed {
		t.Fatalf("truncated-marker cleanup = %+v, %v", receipt, err)
	}
}

func TestCorruptCreationMarkerDoesNotAuthorizeCleanup(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".run.creating-deadbeefdeadbeef")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeExclusive(filepath.Join(root, IncompleteMarkerName), []byte("not-openbox"), 0o600); err != nil {
		t.Fatal(err)
	}
	if state, err := Inspect(root); err == nil || state != StateInvalid {
		t.Fatalf("corrupt-marker state = %q, %v", state, err)
	}
	if receipt, err := CleanupOrphan(root); err == nil || receipt.Removed {
		t.Fatalf("corrupt-marker cleanup = %+v, %v", receipt, err)
	}
	if _, err := os.Lstat(root); err != nil {
		t.Fatalf("corrupt-marker root changed: %v", err)
	}
}

func TestPersistedSentinelAlwaysPreventsCommittedState(t *testing.T) {
	for _, sentinel := range []struct {
		name    string
		content string
	}{
		{IncompleteMarkerName, incompleteMarker},
		{finalizeLockName, finalizeLock},
	} {
		t.Run(sentinel.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "run-sealed-interrupted")
			if err := os.Mkdir(root, 0o700); err != nil {
				t.Fatal(err)
			}
			manifestPath := filepath.Join(root, ManifestName)
			if err := os.WriteFile(manifestPath, []byte(canonicalManifest), 0o400); err != nil {
				t.Fatal(err)
			}
			if err := writeExclusive(filepath.Join(root, sentinel.name), []byte(sentinel.content), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(root, 0o500); err != nil {
				t.Fatal(err)
			}
			state, err := Inspect(root)
			if err != nil || state != StateIncomplete {
				t.Fatalf("sealed interrupted state = %q, %v", state, err)
			}
			recovered, err := OpenIncomplete(root)
			if err != nil {
				t.Fatal(err)
			}
			receipt, err := recovered.Cleanup()
			if err != nil || !receipt.Removed || receipt.ExistsAfter {
				t.Fatalf("sealed interrupted cleanup = %+v, %v", receipt, err)
			}
		})
	}
}

func TestCommittedStateIsNotAuditPackValidation(t *testing.T) {
	root := filepath.Join(t.TempDir(), "lifecycle-only")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ManifestName), []byte(`null`), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(root, ManifestName), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o500); err != nil {
		t.Fatal(err)
	}
	state, err := Inspect(root)
	if err != nil || state != StateManifestCommitted {
		t.Fatalf("lifecycle state = %q, %v", state, err)
	}
	if recovered, err := OpenIncomplete(root); err == nil || recovered != nil {
		t.Fatal("filesystem-committed state must not be cleanup-authorized")
	}
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
}

func TestFinalizationLockPreventsConcurrentPublisher(t *testing.T) {
	root := filepath.Join(t.TempDir(), "run-locked")
	workspace, err := Create(root)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanupIncomplete(t, workspace)
	if err := writeExclusive(filepath.Join(root, finalizeLockName), []byte(finalizeLock), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.Finalize([]byte(canonicalManifest)); err == nil {
		t.Fatal("expected finalization lock rejection")
	}
	if _, err := os.Lstat(filepath.Join(root, ManifestName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("manifest published while locked: %v", err)
	}
	if err := os.Remove(filepath.Join(root, finalizeLockName)); err != nil {
		t.Fatal(err)
	}
}

func TestOwnershipAndPermissionBoundaries(t *testing.T) {
	if _, err := Create("relative/run"); err == nil {
		t.Fatal("expected relative root rejection")
	}
	if _, err := Create(filepath.Join(t.TempDir(), ".run.creating-deadbeefdeadbeef")); err == nil {
		t.Fatal("expected reserved recovery-name rejection")
	}
	if _, err := OpenIncomplete(string(filepath.Separator)); err == nil {
		t.Fatal("expected filesystem root rejection")
	}

	root := filepath.Join(t.TempDir(), "run-marker")
	workspace, err := Create(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(root, IncompleteMarkerName), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenIncomplete(root); err == nil {
		t.Fatal("expected public marker rejection")
	}
	if state, err := Inspect(root); err == nil || state != StateInvalid {
		t.Fatalf("tampered state = %q, %v", state, err)
	}
	if err := os.Chmod(filepath.Join(root, IncompleteMarkerName), 0o600); err != nil {
		t.Fatal(err)
	}
	cleanupIncomplete(t, workspace)
}

func TestCleanupRejectsReplacedOwnedPath(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "run-owned")
	workspace, err := Create(root)
	if err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(parent, "moved-owned")
	if err := os.Rename(root, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeExclusive(filepath.Join(root, IncompleteMarkerName), []byte(incompleteMarker), 0o600); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(root, "victim")
	if err := os.WriteFile(victim, []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}
	if receipt, err := workspace.Cleanup(); err == nil || receipt.Removed {
		t.Fatalf("replacement cleanup = %+v, %v", receipt, err)
	}
	content, err := os.ReadFile(victim)
	if err != nil || string(content) != "preserve" {
		t.Fatalf("replacement victim changed: %q, %v", content, err)
	}
	if err := removeOwnedTree(moved, workspace.identity); err != nil {
		t.Fatal(err)
	}
}

func TestHandleRelativeFinalizeDoesNotMutateReplacementRoot(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "run-finalize-owned")
	workspace, err := Create(root)
	if err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(parent, "moved-finalize-owned")
	victim := filepath.Join(root, "victim")
	workspace.beforePublish = func() error {
		if err := os.Rename(root, moved); err != nil {
			return err
		}
		if err := os.Mkdir(root, 0o700); err != nil {
			return err
		}
		return os.WriteFile(victim, []byte("preserve"), 0o600)
	}
	if _, err := workspace.Finalize([]byte(canonicalManifest)); err == nil {
		t.Fatal("expected changed root identity rejection")
	}
	content, err := os.ReadFile(victim)
	if err != nil || string(content) != "preserve" {
		t.Fatalf("replacement root changed: %q, %v", content, err)
	}
	if _, err := os.Lstat(filepath.Join(root, ManifestName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("manifest reached replacement root: %v", err)
	}
	if state, err := Inspect(moved); err != nil || state != StateIncomplete {
		t.Fatalf("moved owned root = %q, %v", state, err)
	}
	if err := removeOwnedTree(moved, workspace.identity); err != nil {
		t.Fatal(err)
	}
}

func TestHandleRelativeCleanupDoesNotDeleteReplacementRoot(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "run-cleanup-owned")
	workspace, err := Create(root)
	if err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(parent, "moved-cleanup-owned")
	victim := filepath.Join(root, "victim")
	workspace.beforeRemove = func() error {
		if err := os.Rename(root, moved); err != nil {
			return err
		}
		if err := os.Mkdir(root, 0o700); err != nil {
			return err
		}
		return os.WriteFile(victim, []byte("preserve"), 0o600)
	}
	if receipt, err := workspace.Cleanup(); err == nil || receipt.Removed {
		t.Fatalf("replacement cleanup = %+v, %v", receipt, err)
	}
	content, err := os.ReadFile(victim)
	if err != nil || string(content) != "preserve" {
		t.Fatalf("replacement root changed: %q, %v", content, err)
	}
	if state, err := Inspect(moved); err != nil || state != StateOrphaned {
		t.Fatalf("moved owned root = %q, %v", state, err)
	}
	if receipt, err := CleanupOrphan(moved); err != nil || !receipt.Removed {
		t.Fatalf("moved orphan cleanup = %+v, %v", receipt, err)
	}
}

func TestAbruptFinalizationAndCleanupRemainRecoverable(t *testing.T) {
	t.Run("finalization", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "run-finalize-crash")
		command := exec.Command(os.Args[0], "-test.run=^TestRunFSCrashHelper$")
		command.Env = append(os.Environ(), "OPENBOX_RUNFS_CRASH_MODE=finalize", "OPENBOX_RUNFS_CRASH_ROOT="+root)
		err := command.Run()
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 91 {
			t.Fatalf("helper exit = %v", err)
		}
		state, err := Inspect(root)
		if err != nil || state != StateIncomplete {
			t.Fatalf("post-crash state = %q, %v", state, err)
		}
		recovered, err := OpenIncomplete(root)
		if err != nil {
			t.Fatal(err)
		}
		if receipt, err := recovered.Cleanup(); err != nil || !receipt.Removed {
			t.Fatalf("post-crash cleanup = %+v, %v", receipt, err)
		}
	})

	t.Run("cleanup", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "run-cleanup-crash")
		command := exec.Command(os.Args[0], "-test.run=^TestRunFSCrashHelper$")
		command.Env = append(os.Environ(), "OPENBOX_RUNFS_CRASH_MODE=cleanup", "OPENBOX_RUNFS_CRASH_ROOT="+root)
		err := command.Run()
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 92 {
			t.Fatalf("helper exit = %v", err)
		}
		state, err := Inspect(root)
		if err != nil || state != StateIncomplete {
			t.Fatalf("cleanup crash state = %q, %v", state, err)
		}
		recovered, err := OpenIncomplete(root)
		if err != nil {
			t.Fatal(err)
		}
		if receipt, err := recovered.Cleanup(); err != nil || !receipt.Removed {
			t.Fatalf("cleanup crash recovery = %+v, %v", receipt, err)
		}
	})

	t.Run("cleanup orphan window", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "run-cleanup-orphan-crash")
		command := exec.Command(os.Args[0], "-test.run=^TestRunFSCrashHelper$")
		command.Env = append(os.Environ(), "OPENBOX_RUNFS_CRASH_MODE=cleanup_orphan", "OPENBOX_RUNFS_CRASH_ROOT="+root)
		err := command.Run()
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 93 {
			t.Fatalf("helper exit = %v", err)
		}
		state, err := Inspect(root)
		if err != nil || state != StateOrphaned {
			t.Fatalf("cleanup orphan crash state = %q, %v", state, err)
		}
		if recovered, err := OpenIncomplete(root); err == nil || recovered != nil {
			t.Fatal("cleanup orphan granted normal cleanup authority")
		}
		if receipt, err := CleanupOrphan(root); err != nil || !receipt.Removed {
			t.Fatalf("cleanup orphan recovery = %+v, %v", receipt, err)
		}
	})
}

func TestRunFSCrashHelper(t *testing.T) {
	mode := os.Getenv("OPENBOX_RUNFS_CRASH_MODE")
	if mode == "" {
		return
	}
	workspace, err := Create(os.Getenv("OPENBOX_RUNFS_CRASH_ROOT"))
	if err != nil {
		t.Fatal(err)
	}
	switch mode {
	case "finalize":
		workspace.syncRoot = func(*os.File) error {
			os.Exit(91)
			return nil
		}
		_, _ = workspace.Finalize([]byte(canonicalManifest))
	case "cleanup":
		workspace.beforeRemove = func() error {
			os.Exit(92)
			return nil
		}
		_, _ = workspace.Cleanup()
	case "cleanup_orphan":
		workspace.afterMarkerRemoval = func() error {
			os.Exit(93)
			return nil
		}
		_, _ = workspace.Cleanup()
	default:
		t.Fatalf("unknown crash mode %q", mode)
	}
	t.Fatal("crash hook did not exit")
}

func cleanupIncomplete(t *testing.T, workspace *Workspace) {
	t.Helper()
	if _, err := workspace.Cleanup(); err != nil {
		t.Fatal(err)
	}
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s mode = %04o, want %04o", filepath.Base(path), got, want)
	}
}
