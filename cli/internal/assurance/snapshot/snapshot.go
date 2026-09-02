// Package snapshot resolves and inventories project source without executing it.
package snapshot

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// Boundaries are volatile run paths that must never become source candidates.
type Boundaries struct {
	AuditOutput string
	TempParent  string
}

// Kind is the no-follow filesystem classification observed during selection.
type Kind string

const (
	KindRegular         Kind = "regular"
	KindInternalSymlink Kind = "internal_symlink"
	KindExternalSymlink Kind = "external_symlink"
	KindBrokenSymlink   Kind = "broken_symlink"
	KindSocket          Kind = "socket"
	KindFIFO            Kind = "fifo"
	KindDevice          Kind = "device"
	KindExternalMount   Kind = "external_mount"
	KindOther           Kind = "other"
)

// Entry is a snapshot-relative source candidate. LinkTarget is populated only
// for an internal symlink and is itself normalized relative to the project.
type Entry struct {
	Path       string
	Kind       Kind
	Mode       fs.FileMode
	Size       int64
	LinkTarget string
}

// Project pins one resolved source root and run-owned boundaries excluded from
// traversal. Absolute paths remain volatile and are not artifact identities.
type Project struct {
	root            string
	identity        os.FileInfo
	excluded        map[string]boundaryExclusion
	afterSourceOpen func(string) error
	afterCopy       func() error
}

// Resolve resolves a project root without accepting a filesystem root, home
// directory, audit-output root, or run-temp root as the project itself.
func Resolve(requested string, boundaries Boundaries) (*Project, error) {
	if requested == "" {
		requested = "."
	}
	root, err := canonicalExistingPath(requested)
	if err != nil {
		return nil, fmt.Errorf("snapshot: resolve project root: %w", err)
	}
	info, err := os.Lstat(root)
	if err != nil {
		return nil, fmt.Errorf("snapshot: stat project root: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("snapshot: project root is not a resolved directory")
	}
	volumeRoot := filepath.VolumeName(root) + string(filepath.Separator)
	if root == volumeRoot {
		return nil, errors.New("snapshot: filesystem root cannot be a project root")
	}
	if home, homeErr := os.UserHomeDir(); homeErr == nil {
		resolvedHome, resolveErr := canonicalExistingPath(home)
		if resolveErr == nil && samePathOrFile(root, resolvedHome) {
			return nil, errors.New("snapshot: home directory cannot be a project root")
		}
	}

	project := &Project{root: root, identity: info, excluded: make(map[string]boundaryExclusion)}
	for _, boundary := range []struct {
		name   string
		path   string
		class  PathClass
		ruleID string
	}{
		{name: "audit output", path: boundaries.AuditOutput, class: PathClassAuditOutput, ruleID: "exclude-audit-output"},
		{name: "run temp parent", path: boundaries.TempParent, class: PathClassIgnored, ruleID: "exclude-run-temp"},
	} {
		if boundary.path == "" {
			continue
		}
		resolved, resolveErr := canonicalPotentialPath(boundary.path)
		if resolveErr != nil {
			return nil, fmt.Errorf("snapshot: resolve %s: %w", boundary.name, resolveErr)
		}
		if samePathOrFile(root, resolved) {
			return nil, fmt.Errorf("snapshot: project root cannot be the %s", boundary.name)
		}
		rootWithinBoundary, withinErr := filepath.Rel(resolved, root)
		if withinErr != nil {
			return nil, fmt.Errorf("snapshot: relate project to %s: %w", boundary.name, withinErr)
		}
		if rootWithinBoundary != ".." && !strings.HasPrefix(rootWithinBoundary, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("snapshot: project root is inside the %s", boundary.name)
		}
		relative, relErr := filepath.Rel(root, resolved)
		if relErr != nil {
			return nil, fmt.Errorf("snapshot: relate %s to project: %w", boundary.name, relErr)
		}
		if relative != "." && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			normalized, normalizeErr := normalizeRelative(relative)
			if normalizeErr != nil {
				return nil, fmt.Errorf("snapshot: normalize %s: %w", boundary.name, normalizeErr)
			}
			if _, exists := project.excluded[normalized]; exists {
				return nil, fmt.Errorf("snapshot: %s overlaps another run-owned boundary", boundary.name)
			}
			project.excluded[normalized] = boundaryExclusion{class: boundary.class, ruleID: boundary.ruleID}
		}
	}
	return project, nil
}

// Root returns the volatile resolved absolute project path.
func (project *Project) Root() string {
	return project.root
}

// Select returns the deterministic pre-policy inventory used by passive
// inspection, excluding only declared run-owned boundaries. It deliberately
// retains classified secrets and special entries as paths and metadata, never
// contents; callers must use Copy for the closed default executable selection.
func (project *Project) Select() ([]Entry, error) {
	return selectEntries(project)
}

func canonicalExistingPath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	return filepath.Clean(resolved), nil
}

func canonicalPotentialPath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	absolute = filepath.Clean(absolute)
	current := absolute
	var missing []string
	for {
		if _, statErr := os.Lstat(current); statErr == nil {
			break
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return "", statErr
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", errors.New("no existing ancestor")
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
	resolved, err := filepath.EvalSymlinks(current)
	if err != nil {
		return "", err
	}
	for index := len(missing) - 1; index >= 0; index-- {
		resolved = filepath.Join(resolved, missing[index])
	}
	return filepath.Clean(resolved), nil
}

func samePathOrFile(left, right string) bool {
	if filepath.Clean(left) == filepath.Clean(right) {
		return true
	}
	leftInfo, leftErr := os.Lstat(left)
	rightInfo, rightErr := os.Lstat(right)
	return leftErr == nil && rightErr == nil && os.SameFile(leftInfo, rightInfo)
}

func normalizeRelative(relative string) (string, error) {
	clean := filepath.Clean(relative)
	if clean == "." || clean == ".." || filepath.IsAbs(clean) || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || hasDrivePrefix(clean) {
		return "", errors.New("path escapes the project root")
	}
	normalized := filepath.ToSlash(clean)
	if !utf8.ValidString(normalized) || utf8.RuneCountInString(normalized) > 4096 || strings.Contains(normalized, "\\") {
		return "", errors.New("path is not representable by the v1 relative-path contract")
	}
	for _, character := range normalized {
		if character <= 0x1f || character == 0x7f {
			return "", errors.New("path contains a control character")
		}
	}
	return normalized, nil
}

func hasDrivePrefix(path string) bool {
	return len(path) >= 2 && ((path[0] >= 'A' && path[0] <= 'Z') || (path[0] >= 'a' && path[0] <= 'z')) && path[1] == ':'
}
