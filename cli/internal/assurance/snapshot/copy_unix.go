//go:build darwin || linux

package snapshot

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/artifact"
	"golang.org/x/sys/unix"
)

func copySnapshot(project *Project, destination string, selection selectedSource) (*Snapshot, error) {
	root, identity, err := openEmptyDestination(project, destination)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	source, device, err := openPinnedProject(project)
	if err != nil {
		return nil, err
	}
	defer source.Close()
	destinationDevice, err := directoryDevice(root)
	if err != nil {
		return nil, err
	}

	files := make([]File, 0)
	for _, entry := range selection.entries {
		if entry.Kind != KindRegular {
			continue
		}
		input, stat, err := openSelectedRegular(source, device, entry.Path)
		if err != nil {
			return nil, err
		}
		if stat.Size != entry.Size || fsMode(uint32(stat.Mode))&0o111 != entry.Mode&0o111 {
			_ = input.Close()
			return nil, fmt.Errorf("snapshot: source file %s changed after selection", entry.Path)
		}
		if project.afterSourceOpen != nil {
			if err := project.afterSourceOpen(entry.Path); err != nil {
				_ = input.Close()
				return nil, err
			}
		}
		parent, base, err := openDestinationParent(root, destinationDevice, entry.Path)
		if err != nil {
			_ = input.Close()
			return nil, err
		}
		outputFD, err := unix.Openat(int(parent.Fd()), base,
			unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
		if err != nil {
			closeDestinationParent(root, parent)
			_ = input.Close()
			return nil, fmt.Errorf("snapshot: create copied file %s: %w", entry.Path, err)
		}
		output := os.NewFile(uintptr(outputFD), entry.Path)
		hasher := sha256.New()
		written, copyErr := io.Copy(io.MultiWriter(output, hasher), io.LimitReader(input, stat.Size+1))
		var after unix.Stat_t
		statErr := unix.Fstat(int(input.Fd()), &after)
		inputCloseErr := input.Close()
		mode := os.FileMode(0o400)
		executable := entry.Mode&0o111 != 0
		if executable {
			mode = 0o500
		}
		chmodErr := unix.Fchmod(outputFD, uint32(mode.Perm()))
		syncErr := output.Sync()
		outputCloseErr := output.Close()
		parentSyncErr := parent.Sync()
		closeDestinationParent(root, parent)
		if err := errors.Join(copyErr, statErr, inputCloseErr, chmodErr, syncErr, outputCloseErr, parentSyncErr); err != nil {
			return nil, fmt.Errorf("snapshot: copy %s: %w", entry.Path, err)
		}
		if written != stat.Size || after.Size != stat.Size || after.Mode&unix.S_IFMT != unix.S_IFREG ||
			fsMode(uint32(after.Mode))&0o111 != entry.Mode&0o111 {
			return nil, fmt.Errorf("snapshot: source file %s changed while copying", entry.Path)
		}
		files = append(files, File{
			Path:       entry.Path,
			Digest:     digestFromHash(hasher.Sum(nil)),
			Size:       written,
			Executable: executable,
		})
	}
	manifest, digest, selectionDigest, total, err := normalizedIdentities(files, selection)
	if err != nil {
		return nil, err
	}
	return &Snapshot{
		root:            filepath.Clean(destination),
		rootIdentity:    identity,
		sourceIdentity:  project.identity,
		manifest:        manifest,
		digest:          digest,
		selectionDigest: selectionDigest,
		files:           files,
		totalBytes:      total,
		rules:           append([]Rule(nil), selection.rules...),
		omissions:       cloneOmissions(selection.omissions),
	}, nil
}

func cloneOmissions(omissions []Omission) []Omission {
	result := make([]Omission, len(omissions))
	for index, omission := range omissions {
		result[index] = omission
		result[index].Examples = append([]string(nil), omission.Examples...)
	}
	return result
}

func hashSelectedFiles(project *Project, entries []Entry) ([]File, error) {
	source, device, err := openPinnedProject(project)
	if err != nil {
		return nil, err
	}
	defer source.Close()
	files := make([]File, 0)
	for _, entry := range entries {
		if entry.Kind != KindRegular {
			continue
		}
		input, stat, err := openSelectedRegular(source, device, entry.Path)
		if err != nil {
			return nil, err
		}
		if stat.Size != entry.Size || fsMode(uint32(stat.Mode))&0o111 != entry.Mode&0o111 {
			_ = input.Close()
			return nil, fmt.Errorf("snapshot: source file %s changed after selection", entry.Path)
		}
		hasher := sha256.New()
		read, readErr := io.Copy(hasher, io.LimitReader(input, stat.Size+1))
		var after unix.Stat_t
		statErr := unix.Fstat(int(input.Fd()), &after)
		closeErr := input.Close()
		if err := errors.Join(readErr, statErr, closeErr); err != nil {
			return nil, err
		}
		if read != stat.Size || after.Size != stat.Size || after.Mode&unix.S_IFMT != unix.S_IFREG ||
			fsMode(uint32(after.Mode))&0o111 != entry.Mode&0o111 {
			return nil, fmt.Errorf("snapshot: source file %s changed while hashing", entry.Path)
		}
		files = append(files, File{
			Path:       entry.Path,
			Digest:     digestFromHash(hasher.Sum(nil)),
			Size:       read,
			Executable: entry.Mode&0o111 != 0,
		})
	}
	current, err := os.Lstat(project.root)
	if err != nil || !os.SameFile(project.identity, current) {
		if err != nil {
			return nil, fmt.Errorf("snapshot: stat project root after hashing: %w", err)
		}
		return nil, errors.New("snapshot: project root identity changed while hashing")
	}
	return files, nil
}

func openEmptyDestination(project *Project, destination string) (*os.File, os.FileInfo, error) {
	if !filepath.IsAbs(destination) {
		return nil, nil, errors.New("snapshot: destination must be an absolute path")
	}
	requested, err := os.Lstat(destination)
	if err != nil {
		return nil, nil, fmt.Errorf("snapshot: stat destination: %w", err)
	}
	if !requested.IsDir() || requested.Mode()&os.ModeSymlink != 0 || requested.Mode().Perm() != 0o700 {
		return nil, nil, errors.New("snapshot: destination must be a private 0700 directory, not a symlink")
	}
	resolved, err := canonicalExistingPath(destination)
	if err != nil {
		return nil, nil, err
	}
	relative, err := filepath.Rel(project.root, resolved)
	if err != nil {
		return nil, nil, err
	}
	if relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))) {
		return nil, nil, errors.New("snapshot: destination cannot be inside the source project")
	}
	parentFD, err := unix.Open(filepath.Dir(resolved), unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_DIRECTORY, 0)
	if err != nil {
		return nil, nil, err
	}
	parent := os.NewFile(uintptr(parentFD), filepath.Dir(resolved))
	defer parent.Close()
	parentDevice, err := directoryDevice(parent)
	if err != nil {
		return nil, nil, err
	}
	root, err := openChildDirectory(parent, filepath.Base(resolved), parentDevice)
	if errors.Is(err, unix.EXDEV) {
		return nil, nil, errors.New("snapshot: destination cannot be a mount boundary")
	}
	if err != nil {
		return nil, nil, err
	}
	identity, err := root.Stat()
	if err != nil || !os.SameFile(requested, identity) {
		_ = root.Close()
		if err != nil {
			return nil, nil, err
		}
		return nil, nil, errors.New("snapshot: destination identity changed")
	}
	var destinationStat unix.Stat_t
	if err := unix.Fstat(int(root.Fd()), &destinationStat); err != nil {
		_ = root.Close()
		return nil, nil, err
	}
	insideSource, err := projectContainsDirectoryIdentity(project, destinationStat)
	if err != nil {
		_ = root.Close()
		return nil, nil, err
	}
	if insideSource {
		_ = root.Close()
		return nil, nil, errors.New("snapshot: destination aliases a directory inside the source project")
	}
	children, err := root.ReadDir(-1)
	if err != nil {
		_ = root.Close()
		return nil, nil, err
	}
	if len(children) != 0 {
		_ = root.Close()
		return nil, nil, errors.New("snapshot: destination must be empty")
	}
	return root, identity, nil
}

func projectContainsDirectoryIdentity(project *Project, target unix.Stat_t) (bool, error) {
	root, device, err := openPinnedProject(project)
	if err != nil {
		return false, err
	}
	defer root.Close()
	return directoryTreeContainsIdentity(root, device, target)
}

func directoryTreeContainsIdentity(directory *os.File, device uint64, target unix.Stat_t) (bool, error) {
	var current unix.Stat_t
	if err := unix.Fstat(int(directory.Fd()), &current); err != nil {
		return false, err
	}
	if current.Dev == target.Dev && current.Ino == target.Ino {
		return true, nil
	}
	children, err := directory.ReadDir(-1)
	if err != nil {
		return false, err
	}
	for _, child := range children {
		var stat unix.Stat_t
		if err := unix.Fstatat(int(directory.Fd()), child.Name(), &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return false, err
		}
		if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
			continue
		}
		if stat.Dev == target.Dev && stat.Ino == target.Ino {
			return true, nil
		}
		nested, err := openChildDirectory(directory, child.Name(), device)
		if errors.Is(err, unix.EXDEV) {
			continue
		}
		if err != nil {
			return false, err
		}
		found, findErr := directoryTreeContainsIdentity(nested, device, target)
		closeErr := nested.Close()
		if err := errors.Join(findErr, closeErr); err != nil {
			return false, err
		}
		if found {
			return true, nil
		}
	}
	return false, nil
}

func openPinnedProject(project *Project) (*os.File, uint64, error) {
	fd, err := unix.Open(project.root, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_DIRECTORY, 0)
	if err != nil {
		return nil, 0, err
	}
	root := os.NewFile(uintptr(fd), project.root)
	identity, err := root.Stat()
	if err != nil || !os.SameFile(project.identity, identity) {
		_ = root.Close()
		if err != nil {
			return nil, 0, err
		}
		return nil, 0, errors.New("snapshot: project root identity changed")
	}
	device, err := directoryDevice(root)
	if err != nil {
		_ = root.Close()
		return nil, 0, err
	}
	return root, device, nil
}

func openSelectedRegular(root *os.File, device uint64, relative string) (*os.File, unix.Stat_t, error) {
	components := strings.Split(relative, "/")
	current := root
	for _, component := range components[:len(components)-1] {
		child, err := openChildDirectory(current, component, device)
		if err != nil {
			closeSourceDescendant(root, current)
			return nil, unix.Stat_t{}, fmt.Errorf("snapshot: open source component for %s: %w", relative, err)
		}
		if current != root {
			_ = current.Close()
		}
		current = child
	}
	file, err := openRegularFile(current, components[len(components)-1], device)
	closeSourceDescendant(root, current)
	if err != nil {
		return nil, unix.Stat_t{}, fmt.Errorf("snapshot: open selected regular file %s: %w", relative, err)
	}
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		_ = file.Close()
		return nil, unix.Stat_t{}, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		_ = file.Close()
		return nil, unix.Stat_t{}, fmt.Errorf("snapshot: selected file %s changed type", relative)
	}
	return file, stat, nil
}

func closeSourceDescendant(root, current *os.File) {
	if current != root {
		_ = current.Close()
	}
}

func openDestinationParent(root *os.File, device uint64, relative string) (*os.File, string, error) {
	components := strings.Split(relative, "/")
	current := root
	for _, component := range components[:len(components)-1] {
		if err := unix.Mkdirat(int(current.Fd()), component, 0o700); err != nil && !errors.Is(err, unix.EEXIST) {
			closeDestinationParent(root, current)
			return nil, "", fmt.Errorf("snapshot: create copied directory for %s: %w", relative, err)
		}
		child, err := openChildDirectory(current, component, device)
		if err != nil {
			closeDestinationParent(root, current)
			return nil, "", fmt.Errorf("snapshot: open copied directory for %s: %w", relative, err)
		}
		if current != root {
			_ = current.Close()
		}
		current = child
	}
	return current, components[len(components)-1], nil
}

func closeDestinationParent(root, parent *os.File) {
	if parent != root {
		_ = parent.Close()
	}
}

func sealSnapshot(snapshot *Snapshot) error {
	rootFD, err := unix.Open(snapshot.root, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_DIRECTORY, 0)
	if err != nil {
		return err
	}
	root := os.NewFile(uintptr(rootFD), snapshot.root)
	defer root.Close()
	identity, err := root.Stat()
	if err != nil || !os.SameFile(snapshot.rootIdentity, identity) {
		if err != nil {
			return err
		}
		return errors.New("snapshot: destination identity changed before sealing")
	}
	device, err := directoryDevice(root)
	if err != nil {
		return err
	}
	files := make(map[string]File, len(snapshot.files))
	directories := make(map[string]struct{})
	for _, file := range snapshot.files {
		files[file.Path] = file
		for parent := filepath.ToSlash(filepath.Dir(file.Path)); parent != "."; parent = filepath.ToSlash(filepath.Dir(parent)) {
			directories[parent] = struct{}{}
		}
	}
	seen := 0
	if err := sealSnapshotDirectory(root, device, "", files, directories, &seen); err != nil {
		return err
	}
	if seen != len(snapshot.files) {
		return errors.New("snapshot: copied tree is missing a manifested file")
	}
	current, err := os.Lstat(snapshot.root)
	if err != nil || !os.SameFile(snapshot.rootIdentity, current) {
		if err != nil {
			return fmt.Errorf("snapshot: stat destination after sealing: %w", err)
		}
		return errors.New("snapshot: destination identity changed while sealing")
	}
	return nil
}

func sealSnapshotDirectory(
	directory *os.File,
	device uint64,
	relative string,
	files map[string]File,
	directories map[string]struct{},
	seen *int,
) error {
	children, err := directory.ReadDir(-1)
	if err != nil {
		return err
	}
	for _, child := range children {
		var stat unix.Stat_t
		if err := unix.Fstatat(int(directory.Fd()), child.Name(), &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return err
		}
		switch stat.Mode & unix.S_IFMT {
		case unix.S_IFDIR:
			childRelative := pathJoin(relative, child.Name())
			if _, ok := directories[childRelative]; !ok {
				return fmt.Errorf("snapshot: copied tree contains unexpected directory %s", childRelative)
			}
			nested, err := openChildDirectory(directory, child.Name(), device)
			if err != nil {
				return err
			}
			nestedErr := sealSnapshotDirectory(nested, device, childRelative, files, directories, seen)
			closeErr := nested.Close()
			if err := errors.Join(nestedErr, closeErr); err != nil {
				return err
			}
		case unix.S_IFREG:
			childRelative := pathJoin(relative, child.Name())
			expected, ok := files[childRelative]
			if !ok {
				return fmt.Errorf("snapshot: copied tree contains unexpected file %s", childRelative)
			}
			file, err := openRegularFile(directory, child.Name(), device)
			if err != nil {
				return fmt.Errorf("snapshot: open copied file %s for sealing: %w", childRelative, err)
			}
			hasher := sha256.New()
			read, readErr := io.Copy(hasher, io.LimitReader(file, expected.Size+1))
			var opened unix.Stat_t
			statErr := unix.Fstat(int(file.Fd()), &opened)
			closeErr := file.Close()
			if err := errors.Join(readErr, statErr, closeErr); err != nil {
				return err
			}
			wantMode := os.FileMode(0o400)
			if expected.Executable {
				wantMode = 0o500
			}
			if opened.Mode&unix.S_IFMT != unix.S_IFREG || opened.Nlink != 1 ||
				fsMode(uint32(opened.Mode)).Perm() != wantMode || read != expected.Size ||
				digestFromHash(hasher.Sum(nil)) != expected.Digest {
				return fmt.Errorf("snapshot: copied file %s does not match its manifest", childRelative)
			}
			*seen++
		default:
			return errors.New("snapshot: copied tree contains a non-regular entry")
		}
	}
	if err := unix.Fchmod(int(directory.Fd()), 0o500); err != nil {
		return err
	}
	return directory.Sync()
}

func pathJoin(parent, name string) string {
	if parent == "" {
		return name
	}
	return parent + "/" + name
}

func digestFromHash(sum []byte) artifact.ContentDigest {
	var digest artifact.ContentDigest
	copy(digest[:], sum)
	return digest
}
