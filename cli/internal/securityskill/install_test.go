//go:build darwin || linux

package securityskill

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/artifact"
)

func TestManagedInstallFreshUnchangedUpdateConflictAndPreservation(t *testing.T) {
	home := t.TempDir()
	codexHome := filepath.Join(home, "codex-home")
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", codexHome)
	unrelated := filepath.Join(codexHome, "skills", "unrelated", "keep.txt")
	if err := os.MkdirAll(filepath.Dir(unrelated), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unrelated, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := Install("codex", true)
	if err != nil || result.Action != ActionInstalled {
		t.Fatalf("dry-run = %#v, %v", result, err)
	}
	if _, err := os.Lstat(result.Target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dry-run wrote target: %v", err)
	}
	result, err = Install("codex", false)
	if err != nil || result.Action != ActionInstalled {
		t.Fatalf("install = %#v, %v", result, err)
	}
	if _, err := validateManagedDirectory(result.Target); err != nil {
		t.Fatal(err)
	}
	result, err = Install("codex", false)
	if err != nil || result.Action != ActionUnchanged {
		t.Fatalf("unchanged = %#v, %v", result, err)
	}
	if got, _ := os.ReadFile(unrelated); string(got) != "keep" {
		t.Fatalf("unrelated skill changed: %q", got)
	}

	rewriteManagedVersion(t, result.Target, "0.9.0")
	result, err = Install("codex", false)
	if err != nil || result.Action != ActionUpdated {
		t.Fatalf("update = %#v, %v", result, err)
	}
	if installed, err := validateManagedDirectory(result.Target); err != nil || installed.Version != Version {
		t.Fatalf("updated target = %#v, %v", installed, err)
	}

	if err := os.RemoveAll(result.Target); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(result.Target, []byte("foreign"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err = Install("codex", false)
	if !errors.Is(err, ErrConflict) || result.Action != ActionConflict {
		t.Fatalf("conflict = %#v, %v", result, err)
	}
	if got, _ := os.ReadFile(result.Target); string(got) != "foreign" {
		t.Fatalf("foreign target changed: %q", got)
	}
}

func TestManagedInstallRollsBackOnlyPriorManagedTarget(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, "codex"))
	result, err := Install("codex", false)
	if err != nil {
		t.Fatal(err)
	}
	rewriteManagedVersion(t, result.Target, "0.9.0")
	transactionHook = func(phase, _, _ string) error {
		if phase == "after_backup" {
			return errors.New("injected interruption")
		}
		return nil
	}
	t.Cleanup(func() { transactionHook = nil })
	if _, err := Install("codex", false); err == nil {
		t.Fatal("interrupted update succeeded")
	}
	transactionHook = nil
	managed, err := validateManagedDirectory(result.Target)
	if err != nil || managed.Version != "0.9.0" {
		t.Fatalf("rollback target = %#v, %v", managed, err)
	}
}

func TestManagedInstallNoReplaceRejectsTargetRace(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, "codex"))
	transactionHook = func(phase, _, target string) error {
		if phase == "before_publish" {
			if err := os.Mkdir(target, 0o700); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(target, "foreign"), []byte("keep"), 0o600)
		}
		return nil
	}
	t.Cleanup(func() { transactionHook = nil })
	result, err := Install("codex", false)
	if err == nil || result.Action != ActionInstalled {
		t.Fatalf("race = %#v, %v", result, err)
	}
	if got, _ := os.ReadFile(filepath.Join(result.Target, "foreign")); string(got) != "keep" {
		t.Fatalf("racing target changed: %q", got)
	}
}

func TestManagedInstallPermissionsAndProviderPaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	claude, err := Install("claude-code", true)
	if err != nil || claude.Target != filepath.Join(home, ".claude", "skills", Name) {
		t.Fatalf("claude target = %#v, %v", claude, err)
	}
	isolatedClaude := filepath.Join(home, "isolated-claude")
	t.Setenv("CLAUDE_CONFIG_DIR", isolatedClaude)
	claude, err = Install("claude-code", true)
	if err != nil || claude.Target != filepath.Join(isolatedClaude, "skills", Name) {
		t.Fatalf("isolated claude target = %#v, %v", claude, err)
	}
	codex, err := Install("codex", true)
	if err != nil || codex.Target != filepath.Join(home, ".codex", "skills", Name) {
		t.Fatalf("codex target = %#v, %v", codex, err)
	}
	cursor, err := Install("cursor", false)
	if err != nil || cursor.Action != ActionManualRequired || cursor.Target != filepath.Join(".agents", "skills", Name) {
		t.Fatalf("cursor = %#v, %v", cursor, err)
	}

	t.Setenv("CODEX_HOME", filepath.Join(home, "isolated"))
	installed, err := Install("codex", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(installed.Target, "SKILL.md"), 0o644); err != nil {
		t.Fatal(err)
	}
	conflict, err := Install("codex", false)
	if !errors.Is(err, ErrConflict) || conflict.Action != ActionConflict {
		t.Fatalf("permission conflict = %#v, %v", conflict, err)
	}
}

func rewriteManagedVersion(t *testing.T, root, version string) {
	t.Helper()
	manifest, err := validateManagedDirectory(root)
	if err != nil {
		t.Fatal(err)
	}
	skillPath := filepath.Join(root, "SKILL.md")
	skill, err := os.ReadFile(skillPath)
	if err != nil {
		t.Fatal(err)
	}
	skill = append(skill, []byte("\nlegacy managed payload\n")...)
	if err := os.WriteFile(skillPath, skill, 0o600); err != nil {
		t.Fatal(err)
	}
	manifest.Version = version
	for index := range manifest.Files {
		path := filepath.Join(root, filepath.FromSlash(manifest.Files[index].Path))
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		manifest.Files[index].Bytes = len(content)
		manifest.Files[index].SHA256 = artifact.DigestBytes(content).String()
	}
	descriptors, err := artifact.CanonicalJSON(manifest.Files)
	if err != nil {
		t.Fatal(err)
	}
	manifest.Digest = artifact.DigestBytes(descriptors).String()
	content, err := artifact.CanonicalJSON(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, BundleManifestName), content, 0o600); err != nil {
		t.Fatal(err)
	}
}
