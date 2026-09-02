package securityskill

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/artifact"
)

type Action string

const (
	ActionInstalled      Action = "installed"
	ActionUnchanged      Action = "unchanged"
	ActionUpdated        Action = "updated"
	ActionConflict       Action = "conflict"
	ActionManualRequired Action = "manual_required"
)

var ErrConflict = errors.New("security skill: target conflicts with an unmanaged or malformed skill")

type InstallResult struct {
	Target         string
	Action         Action
	Version        string
	Digest         string
	RepositoryPath string
	ConflictReason string
}

var transactionHook func(phase, stage, target string) error

// Install plans or applies one provider-selected managed-directory transaction.
// Cursor is always manual_required and never writes.
func Install(provider string, dryRun bool) (InstallResult, error) {
	manifest, files, err := Load()
	if err != nil {
		return InstallResult{}, err
	}
	target, err := targetForProvider(provider)
	if err != nil {
		return InstallResult{}, err
	}
	result := InstallResult{Target: target, Version: manifest.Version, Digest: manifest.Digest, RepositoryPath: RepositoryPath}
	if provider == "cursor" {
		result.Action = ActionManualRequired
		return result, nil
	}

	action, reason := inspectAction(target, manifest)
	result.Action, result.ConflictReason = action, reason
	if dryRun || action == ActionUnchanged {
		return result, nil
	}
	if action == ActionConflict {
		return result, fmt.Errorf("%w: %s", ErrConflict, reason)
	}
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		return result, errors.New("security skill: managed installation is supported on Darwin and Linux only")
	}
	parent := filepath.Dir(target)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return result, fmt.Errorf("security skill: create target parent: %w", err)
	}
	parentInfo, err := os.Lstat(parent)
	if err != nil || !parentInfo.IsDir() || parentInfo.Mode()&os.ModeSymlink != 0 {
		return result, errors.New("security skill: target parent is not a real directory")
	}
	stage, err := os.MkdirTemp(parent, ".openbox-security-evaluation.stage-")
	if err != nil {
		return result, fmt.Errorf("security skill: create same-parent staging: %w", err)
	}
	stagePresent := true
	defer func() {
		if stagePresent {
			_ = os.RemoveAll(stage)
		}
	}()
	if err := writeStagedBundle(stage, files); err != nil {
		return result, err
	}
	if transactionHook != nil {
		if err := transactionHook("before_publish", stage, target); err != nil {
			return result, err
		}
	}

	switch action {
	case ActionInstalled:
		if err := renameNoReplace(stage, target); err != nil {
			return result, fmt.Errorf("security skill: publish fresh managed target: %w", err)
		}
		stagePresent = false
	case ActionUpdated:
		backup, err := reserveBackupName(parent)
		if err != nil {
			return result, err
		}
		if err := renameNoReplace(target, backup); err != nil {
			return result, fmt.Errorf("security skill: move prior managed target aside: %w", err)
		}
		rollback := func(cause error) error {
			if _, statErr := os.Lstat(target); errors.Is(statErr, os.ErrNotExist) {
				if restoreErr := renameNoReplace(backup, target); restoreErr == nil {
					return cause
				} else {
					return errors.Join(cause, fmt.Errorf("security skill: rollback prior managed target from %s: %w", backup, restoreErr))
				}
			}
			return errors.Join(cause, fmt.Errorf("security skill: rollback preserved prior managed target at %s because target was replaced", backup))
		}
		if transactionHook != nil {
			if err := transactionHook("after_backup", stage, target); err != nil {
				return result, rollback(err)
			}
		}
		if err := renameNoReplace(stage, target); err != nil {
			return result, rollback(fmt.Errorf("security skill: publish updated managed target: %w", err))
		}
		stagePresent = false
		if _, err := validateManagedDirectory(target); err != nil {
			return result, errors.Join(err, fmt.Errorf("security skill: prior managed target retained at %s", backup))
		}
		if err := os.RemoveAll(backup); err != nil {
			return result, fmt.Errorf("security skill: remove replaced managed target: %w", err)
		}
	default:
		return result, fmt.Errorf("security skill: unsupported install action %q", action)
	}
	if err := syncDirectory(parent); err != nil {
		return result, fmt.Errorf("security skill: sync target parent: %w", err)
	}
	installed, err := validateManagedDirectory(target)
	if err != nil || installed.Digest != manifest.Digest || installed.Version != manifest.Version {
		return result, errors.Join(errors.New("security skill: installed target failed final verification"), err)
	}
	return result, nil
}

func targetForProvider(provider string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = os.Getenv("HOME")
	}
	if home == "" {
		return "", errors.New("security skill: user home is unavailable")
	}
	switch provider {
	case "claude-code":
		claudeConfig := os.Getenv("CLAUDE_CONFIG_DIR")
		if claudeConfig == "" {
			claudeConfig = filepath.Join(home, ".claude")
		}
		if !filepath.IsAbs(claudeConfig) {
			return "", errors.New("security skill: CLAUDE_CONFIG_DIR must be absolute")
		}
		return filepath.Join(claudeConfig, "skills", Name), nil
	case "codex":
		codexHome := os.Getenv("CODEX_HOME")
		if codexHome == "" {
			codexHome = filepath.Join(home, ".codex")
		}
		return filepath.Join(codexHome, "skills", Name), nil
	case "cursor":
		return filepath.Join(".agents", "skills", Name), nil
	default:
		return "", fmt.Errorf("security skill: unknown provider %q", provider)
	}
}

func inspectAction(target string, current Manifest) (Action, string) {
	info, err := os.Lstat(target)
	if errors.Is(err, os.ErrNotExist) {
		return ActionInstalled, ""
	}
	if err != nil {
		return ActionConflict, err.Error()
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return ActionConflict, "target is not a managed directory"
	}
	existing, err := validateManagedDirectory(target)
	if err != nil {
		return ActionConflict, err.Error()
	}
	comparison, err := compareSemver(existing.Version, current.Version)
	if err != nil {
		return ActionConflict, err.Error()
	}
	switch {
	case comparison == 0 && existing.Digest == current.Digest:
		return ActionUnchanged, ""
	case comparison < 0:
		return ActionUpdated, ""
	case comparison == 0:
		return ActionConflict, "managed target has the current version with a different digest"
	default:
		return ActionConflict, "managed target is newer than the embedded bundle"
	}
}

func validateManagedDirectory(root string) (Manifest, error) {
	var manifest Manifest
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 {
		return manifest, errors.New("security skill: managed target root must be a mode-0700 real directory")
	}
	manifestPath := filepath.Join(root, BundleManifestName)
	manifestBytes, err := readManagedFile(manifestPath, 0o600, 1<<20)
	if err != nil {
		return manifest, err
	}
	if _, err := artifact.CanonicalizeJSON(manifestBytes); err != nil {
		return manifest, errors.New("security skill: managed bundle manifest is invalid JSON")
	}
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return manifest, err
	}
	if manifest.Schema != BundleSchema || manifest.Name != Name || len(manifest.Files) != len(payloadPaths) {
		return manifest, errors.New("security skill: managed bundle identity is invalid")
	}
	wantPaths := append([]string(nil), payloadPaths[:]...)
	for index, descriptor := range manifest.Files {
		if descriptor.Path != wantPaths[index] || descriptor.Bytes < 0 || descriptor.Bytes > MaxCandidateBytes || descriptor.SHA256 == "" {
			return manifest, errors.New("security skill: managed bundle descriptor set is invalid")
		}
		mode := os.FileMode(0o600)
		if descriptor.Path == "scripts/publish-candidate.sh" {
			mode = 0o700
		}
		content, err := readManagedFile(filepath.Join(root, filepath.FromSlash(descriptor.Path)), mode, int64(descriptor.Bytes))
		if err != nil || len(content) != descriptor.Bytes || artifact.DigestBytes(content).String() != descriptor.SHA256 {
			return manifest, errors.New("security skill: managed bundle payload does not match its descriptor")
		}
	}
	for _, directory := range []string{"references", "scripts"} {
		entry, err := os.Lstat(filepath.Join(root, directory))
		if err != nil || !entry.IsDir() || entry.Mode()&os.ModeSymlink != 0 || entry.Mode().Perm() != 0o700 {
			return manifest, errors.New("security skill: managed bundle directory permissions are invalid")
		}
	}
	wantEntries := map[string]bool{
		BundleManifestName: true, "SKILL.md": true, "references": true, "scripts": true,
		"references/candidate.schema.json": true, "references/evidence-authority.md": true,
		"references/standards.json": true, "scripts/publish-candidate.sh": true,
	}
	seen := make(map[string]bool, len(wantEntries))
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if !wantEntries[relative] || entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("security skill: unexpected managed entry %s", relative)
		}
		seen[relative] = true
		return nil
	})
	if err != nil || len(seen) != len(wantEntries) {
		return manifest, errors.Join(errors.New("security skill: managed bundle file set is not exact"), err)
	}
	descriptorBytes, err := artifact.CanonicalJSON(manifest.Files)
	if err != nil || artifact.DigestBytes(descriptorBytes).String() != manifest.Digest {
		return manifest, errors.New("security skill: managed bundle digest is invalid")
	}
	return manifest, nil
}

func readManagedFile(path string, mode os.FileMode, maximum int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != mode || !singleLink(info) || info.Size() < 0 || info.Size() > maximum {
		if err != nil {
			return nil, fmt.Errorf("security skill: %s is not an exact managed file: %w", filepath.Base(path), err)
		}
		return nil, fmt.Errorf("security skill: %s is not an exact managed file (mode %04o, regular %t, single-link %t, bytes %d, maximum %d)", filepath.Base(path), info.Mode().Perm(), info.Mode().IsRegular(), singleLink(info), info.Size(), maximum)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	content, readErr := io.ReadAll(io.LimitReader(file, maximum+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		return nil, errors.Join(readErr, closeErr)
	}
	if int64(len(content)) != info.Size() {
		return nil, errors.New("security skill: managed file changed while reading")
	}
	after, err := os.Lstat(path)
	if err != nil || !os.SameFile(info, after) || after.Mode().Perm() != mode || !singleLink(after) || after.Size() != info.Size() {
		return nil, errors.New("security skill: managed file identity changed while reading")
	}
	return content, nil
}

func writeStagedBundle(stage string, files map[string][]byte) error {
	if err := os.Chmod(stage, 0o700); err != nil {
		return err
	}
	for _, directory := range []string{"references", "scripts"} {
		if err := os.Mkdir(filepath.Join(stage, directory), 0o700); err != nil {
			return err
		}
	}
	for _, path := range append(append([]string(nil), payloadPaths[:]...), BundleManifestName) {
		mode := os.FileMode(0o600)
		if path == "scripts/publish-candidate.sh" {
			mode = 0o700
		}
		if err := writeExclusiveFile(filepath.Join(stage, filepath.FromSlash(path)), files[path], mode); err != nil {
			return err
		}
	}
	return syncDirectory(stage)
}

func writeExclusiveFile(path string, content []byte, mode os.FileMode) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	remove := true
	defer func() {
		_ = file.Close()
		if remove {
			_ = os.Remove(path)
		}
	}()
	if err := file.Chmod(mode); err != nil {
		return err
	}
	if _, err := file.Write(content); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	remove = false
	return nil
}

func reserveBackupName(parent string) (string, error) {
	placeholder, err := os.CreateTemp(parent, ".openbox-security-evaluation.rollback-")
	if err != nil {
		return "", err
	}
	name := placeholder.Name()
	if err := placeholder.Close(); err != nil {
		return "", err
	}
	if err := os.Remove(name); err != nil {
		return "", err
	}
	return name, nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil && !errors.Is(err, os.ErrInvalid) {
		return err
	}
	return nil
}

func compareSemver(left, right string) (int, error) {
	parse := func(value string) ([3]int, error) {
		var result [3]int
		parts := strings.Split(value, ".")
		if len(parts) != 3 {
			return result, fmt.Errorf("security skill: invalid semantic version %q", value)
		}
		for index, part := range parts {
			parsed, err := strconv.Atoi(part)
			if err != nil || parsed < 0 || (len(part) > 1 && part[0] == '0') {
				return result, fmt.Errorf("security skill: invalid semantic version %q", value)
			}
			result[index] = parsed
		}
		return result, nil
	}
	l, err := parse(left)
	if err != nil {
		return 0, err
	}
	r, err := parse(right)
	if err != nil {
		return 0, err
	}
	for index := range l {
		if l[index] < r[index] {
			return -1, nil
		}
		if l[index] > r[index] {
			return 1, nil
		}
	}
	return 0, nil
}
