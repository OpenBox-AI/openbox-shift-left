//go:build darwin || linux

package runfs

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/artifact"
	"golang.org/x/sys/unix"
)

var cleanupOrphanAttribute = []byte("openbox project-assurance run v1: cleanup orphan\n")

func openOwnedRoot(path string, identity os.FileInfo) (*os.File, error) {
	parent, root, err := openOwnedRootAndParent(path, identity)
	if err != nil {
		return nil, err
	}
	if err := parent.Close(); err != nil {
		_ = root.Close()
		return nil, err
	}
	return root, nil
}

func openOwnedRootAndParent(path string, identity os.FileInfo) (*os.File, *os.File, error) {
	parent, err := openDirectoryNoFollow(filepath.Dir(path))
	if err != nil {
		return nil, nil, err
	}
	device, err := directoryDevice(parent)
	if err != nil {
		_ = parent.Close()
		return nil, nil, err
	}
	root, err := openChildDirectory(parent, filepath.Base(path), device)
	if err != nil {
		_ = parent.Close()
		return nil, nil, err
	}
	info, err := root.Stat()
	if err != nil || !os.SameFile(identity, info) {
		_ = root.Close()
		_ = parent.Close()
		if err != nil {
			return nil, nil, err
		}
		return nil, nil, errors.New("runfs: owned workspace identity changed")
	}
	return parent, root, nil
}

func sealOwnedContents(root *os.File, identity os.FileInfo, rootFiles map[string][]byte) error {
	info, err := root.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(identity, info) {
		return errors.New("runfs: seal target identity changed")
	}
	device, err := directoryDevice(root)
	if err != nil {
		return err
	}
	if err := sealDirectory(root, device, "", rootFiles); err != nil {
		return fmt.Errorf("runfs: seal pack contents: %w", err)
	}
	return nil
}

func sealDirectory(directory *os.File, device uint64, relative string, rootFiles map[string][]byte) error {
	entries, err := directory.ReadDir(-1)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		childRelative := entry.Name()
		if relative != "" {
			childRelative = relative + "/" + entry.Name()
		}
		if relative == "" && (entry.Name() == IncompleteMarkerName || entry.Name() == finalizeLockName) {
			continue
		}
		var stat unix.Stat_t
		if err := unix.Fstatat(int(directory.Fd()), entry.Name(), &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return err
		}
		switch stat.Mode & unix.S_IFMT {
		case unix.S_IFDIR:
			if childRelative != "objects" && childRelative != "objects/sha256" {
				return fmt.Errorf("unsupported pack directory %s", childRelative)
			}
			child, err := openChildDirectory(directory, entry.Name(), device)
			if err != nil {
				return fmt.Errorf("open pack directory %s without crossing a mount: %w", childRelative, err)
			}
			sealErr := sealDirectory(child, device, childRelative, rootFiles)
			if sealErr == nil {
				sealErr = unix.Fchmod(int(child.Fd()), 0o500)
			}
			if sealErr == nil {
				sealErr = unix.Fsync(int(child.Fd()))
			}
			closeErr := child.Close()
			if sealErr != nil || closeErr != nil {
				return errors.Join(sealErr, closeErr)
			}
		case unix.S_IFREG:
			_, observationFile := rootFiles[entry.Name()]
			if (relative != "objects/sha256" || !validObjectName(entry.Name())) && !(relative == "" && observationFile) {
				return fmt.Errorf("unsupported pack file %s", childRelative)
			}
			if stat.Nlink != 1 {
				return fmt.Errorf("pack file %s has %d hard links, want 1", childRelative, stat.Nlink)
			}
			fileFD, err := unix.Openat(int(directory.Fd()), entry.Name(),
				unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
			if err != nil {
				return err
			}
			file := os.NewFile(uintptr(fileFD), entry.Name())
			var openedStat unix.Stat_t
			statErr := unix.Fstat(fileFD, &openedStat)
			if statErr == nil && (openedStat.Nlink != 1 || openedStat.Mode&unix.S_IFMT != unix.S_IFREG) {
				statErr = errors.New("pack file identity changed while sealing")
			}
			if statErr == nil {
				statErr = unix.Fchmod(fileFD, 0o400)
			}
			if statErr == nil {
				statErr = unix.Fsync(fileFD)
			}
			closeErr := file.Close()
			if statErr != nil || closeErr != nil {
				return errors.Join(statErr, closeErr)
			}
		default:
			return fmt.Errorf("unsupported pack entry %s", childRelative)
		}
	}
	if err := unix.Fsync(int(directory.Fd())); err != nil && err != unix.EINVAL && err != unix.ENOTSUP {
		return err
	}
	return nil
}

func verifyObservationFilesAt(root *os.File, payloads map[string][]byte) error {
	if _, err := root.Seek(0, 0); err != nil {
		return err
	}
	entries, err := readDirectoryBounded(root, len(observationPayloadNames)+2)
	if err != nil {
		return err
	}
	wantEntries := len(observationPayloadNames) + 2 // incomplete marker and finalizing lock
	if len(entries) != wantEntries {
		return fmt.Errorf("observation root has %d entries, want %d", len(entries), wantEntries)
	}
	for _, name := range observationPayloadNames {
		content, _, err := readRegularAt(root, name, 0o400, int64(len(payloads[name])+1))
		if err != nil {
			return err
		}
		if !bytes.Equal(content, payloads[name]) {
			return fmt.Errorf("observation payload %s changed before manifest publication", name)
		}
	}
	return nil
}

func validObjectName(name string) bool {
	if len(name) != 64 {
		return false
	}
	return strings.IndexFunc(name, func(character rune) bool {
		return !('0' <= character && character <= '9') && !('a' <= character && character <= 'f')
	}) == -1
}

func openDirectoryNoFollow(path string) (*os.File, error) {
	return openNoFollow(path, unix.O_DIRECTORY)
}

func openNoFollow(path string, extraFlags int) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|extraFlags, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}

func directoryDevice(directory *os.File) (uint64, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(int(directory.Fd()), &stat); err != nil {
		return 0, err
	}
	return uint64(stat.Dev), nil
}

func writeExclusiveAt(directory *os.File, name string, content []byte, mode os.FileMode) error {
	fd, err := unix.Openat(int(directory.Fd()), name,
		unix.O_CREAT|unix.O_EXCL|unix.O_WRONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, uint32(mode.Perm()))
	if err != nil {
		return fmt.Errorf("runfs: create %s: %w", name, err)
	}
	file := os.NewFile(uintptr(fd), name)
	remove := true
	defer func() {
		_ = file.Close()
		if remove {
			_ = unix.Unlinkat(int(directory.Fd()), name, 0)
		}
	}()
	if err := unix.Fchmod(fd, uint32(mode.Perm())); err != nil {
		return fmt.Errorf("runfs: chmod %s: %w", name, err)
	}
	if _, err := file.Write(content); err != nil {
		return fmt.Errorf("runfs: write %s: %w", name, err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("runfs: sync %s: %w", name, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("runfs: close %s: %w", name, err)
	}
	remove = false
	return nil
}

func writePackObjectsAt(root *os.File, objects map[string][]byte) error {
	device, err := directoryDevice(root)
	if err != nil {
		return err
	}
	objectDirectory, err := openOrCreatePackDirectory(root, "objects", device)
	if err != nil {
		return err
	}
	defer objectDirectory.Close()
	digestDirectory, err := openOrCreatePackDirectory(objectDirectory, "sha256", device)
	if err != nil {
		return err
	}
	defer digestDirectory.Close()

	names := make([]string, 0, len(objects))
	for name := range objects {
		if !validObjectName(name) {
			return fmt.Errorf("invalid object name %q", name)
		}
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := writeExclusiveAt(digestDirectory, name, objects[name], 0o600); err != nil {
			return err
		}
	}
	if err := syncOpenDirectory(digestDirectory); err != nil {
		return err
	}
	if err := syncOpenDirectory(objectDirectory); err != nil {
		return err
	}
	return syncOpenDirectory(root)
}

func verifyPackObjectsAt(root *os.File, objects map[string][]byte) error {
	device, err := directoryDevice(root)
	if err != nil {
		return err
	}
	objectDirectory, err := openChildDirectory(root, "objects", device)
	if err != nil {
		return fmt.Errorf("open objects directory: %w", err)
	}
	defer objectDirectory.Close()
	digestDirectory, err := openChildDirectory(objectDirectory, "sha256", device)
	if err != nil {
		return fmt.Errorf("open sha256 directory: %w", err)
	}
	defer digestDirectory.Close()
	entries, err := digestDirectory.ReadDir(-1)
	if err != nil {
		return err
	}
	if len(entries) != len(objects) {
		return fmt.Errorf("stored object count is %d, want %d", len(entries), len(objects))
	}
	for _, entry := range entries {
		expected, exists := objects[entry.Name()]
		if !exists {
			return fmt.Errorf("unreferenced object %s", entry.Name())
		}
		var namedStat unix.Stat_t
		if err := unix.Fstatat(int(digestDirectory.Fd()), entry.Name(), &namedStat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return err
		}
		if namedStat.Mode&unix.S_IFMT != unix.S_IFREG || namedStat.Nlink != 1 {
			return fmt.Errorf("object %s is not a single-link regular file", entry.Name())
		}
		fd, err := unix.Openat(int(digestDirectory.Fd()), entry.Name(), unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
		if err != nil {
			return err
		}
		file := os.NewFile(uintptr(fd), entry.Name())
		var openedStat unix.Stat_t
		statErr := unix.Fstat(fd, &openedStat)
		if statErr == nil && (openedStat.Dev != namedStat.Dev || openedStat.Ino != namedStat.Ino || openedStat.Mode&unix.S_IFMT != unix.S_IFREG || openedStat.Nlink != 1) {
			statErr = errors.New("object identity changed while verifying")
		}
		var content []byte
		if statErr == nil {
			content, statErr = io.ReadAll(io.LimitReader(file, int64(len(expected))+1))
		}
		closeErr := file.Close()
		if statErr != nil || closeErr != nil {
			return errors.Join(statErr, closeErr)
		}
		if !bytes.Equal(content, expected) {
			return fmt.Errorf("object %s bytes do not match assembled content", entry.Name())
		}
	}
	return nil
}

func readCommittedPackAt(root *os.File) (*artifact.ManifestIndex, map[artifact.Role][]byte, error) {
	return readCommittedPackAtWithHook(root, nil)
}

func readCommittedPackAtWithHook(root *os.File, afterRead func() error) (*artifact.ManifestIndex, map[artifact.Role][]byte, error) {
	rootEntries, err := readDirectoryBounded(root, 2)
	if err != nil {
		return nil, nil, err
	}
	if len(rootEntries) != 2 || !namedEntries(rootEntries, ManifestName, "objects") {
		return nil, nil, errors.New("pack root does not have the exact manifest and objects entries")
	}
	manifest, manifestStamp, err := readRegularAt(root, ManifestName, 0o400, maxVerifiedManifestBytes)
	if err != nil {
		return nil, nil, fmt.Errorf("read manifest: %w", err)
	}
	index, err := artifact.ParseManifestIndex(manifest)
	if err != nil {
		return nil, nil, err
	}
	device, err := directoryDevice(root)
	if err != nil {
		return nil, nil, err
	}
	objectsDirectory, err := openChildDirectory(root, "objects", device)
	if err != nil {
		return nil, nil, fmt.Errorf("open objects directory: %w", err)
	}
	defer objectsDirectory.Close()
	if err := requireDirectoryMode(objectsDirectory, 0o500); err != nil {
		return nil, nil, err
	}
	objectEntries, err := readDirectoryBounded(objectsDirectory, 1)
	if err != nil {
		return nil, nil, err
	}
	if len(objectEntries) != 1 || objectEntries[0].Name() != "sha256" {
		return nil, nil, errors.New("objects directory does not have the exact sha256 entry")
	}
	digestDirectory, err := openChildDirectory(objectsDirectory, "sha256", device)
	if err != nil {
		return nil, nil, fmt.Errorf("open sha256 directory: %w", err)
	}
	defer digestDirectory.Close()
	if err := requireDirectoryMode(digestDirectory, 0o500); err != nil {
		return nil, nil, err
	}

	referencesByCID := make(map[artifact.ContentDigest][]artifact.ManifestReference)
	for _, reference := range index.References() {
		if reference.Bytes() > maxVerifiedObjectBytes {
			return nil, nil, fmt.Errorf("object for role %q exceeds the verification bound", reference.Role())
		}
		for _, prior := range referencesByCID[reference.CID()] {
			if prior.Bytes() != reference.Bytes() {
				return nil, nil, errors.New("one content ID has conflicting byte lengths")
			}
		}
		referencesByCID[reference.CID()] = append(referencesByCID[reference.CID()], reference)
	}
	entries, err := readDirectoryBounded(digestDirectory, len(referencesByCID))
	if err != nil {
		return nil, nil, err
	}
	if len(entries) != len(referencesByCID) {
		return nil, nil, errors.New("stored object set differs from the manifest")
	}
	objects := make(map[artifact.Role][]byte)
	objectStamps := make(map[string]entryStamp, len(entries))
	total := int64(0)
	for _, entry := range entries {
		if !validObjectName(entry.Name()) {
			return nil, nil, errors.New("stored object has an invalid content-ID name")
		}
		digest, err := artifact.ParseContentDigest("sha256:" + entry.Name())
		if err != nil {
			return nil, nil, err
		}
		references, ok := referencesByCID[digest]
		if !ok {
			return nil, nil, errors.New("stored object is not referenced by the manifest")
		}
		content, stamp, err := readRegularAt(digestDirectory, entry.Name(), 0o400, references[0].Bytes())
		if err != nil {
			return nil, nil, fmt.Errorf("read addressed object: %w", err)
		}
		total += int64(len(content))
		if total > maxVerifiedPackBytes {
			return nil, nil, errors.New("pack exceeds the verification byte bound")
		}
		for _, reference := range references {
			if err := index.VerifyObject(reference, content); err != nil {
				return nil, nil, err
			}
			objects[reference.Role()] = append([]byte(nil), content...)
		}
		objectStamps[entry.Name()] = stamp
	}
	if afterRead != nil {
		if err := afterRead(); err != nil {
			return nil, nil, err
		}
	}
	if err := verifyCommittedPackStable(root, manifestStamp, objectStamps); err != nil {
		return nil, nil, err
	}
	return index, objects, nil
}

type entryStamp struct {
	device uint64
	inode  uint64
	size   int64
	mode   uint32
	links  uint64
	mtimeS int64
	mtimeN int64
	ctimeS int64
	ctimeN int64
}

func readRegularAt(directory *os.File, name string, mode os.FileMode, limit int64) ([]byte, entryStamp, error) {
	var named unix.Stat_t
	if err := unix.Fstatat(int(directory.Fd()), name, &named, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return nil, entryStamp{}, err
	}
	if named.Mode&unix.S_IFMT != unix.S_IFREG || named.Nlink != 1 || os.FileMode(named.Mode).Perm() != mode {
		return nil, entryStamp{}, errors.New("entry is not an exact single-link regular file")
	}
	if named.Size < 0 || named.Size > limit {
		return nil, entryStamp{}, errors.New("entry exceeds its declared byte bound")
	}
	fd, err := unix.Openat(int(directory.Fd()), name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, entryStamp{}, err
	}
	file := os.NewFile(uintptr(fd), name)
	var opened unix.Stat_t
	statErr := unix.Fstat(fd, &opened)
	if statErr == nil && (opened.Dev != named.Dev || opened.Ino != named.Ino || opened.Mode&unix.S_IFMT != unix.S_IFREG ||
		opened.Nlink != 1 || opened.Size != named.Size || os.FileMode(opened.Mode).Perm() != mode) {
		statErr = errors.New("entry identity changed while reading")
	}
	var content []byte
	if statErr == nil {
		content, statErr = io.ReadAll(io.LimitReader(file, limit+1))
	}
	var after unix.Stat_t
	if statErr == nil {
		statErr = unix.Fstat(fd, &after)
	}
	if statErr == nil && (after.Dev != opened.Dev || after.Ino != opened.Ino || after.Size != opened.Size ||
		after.Mode&unix.S_IFMT != unix.S_IFREG || after.Nlink != 1 || os.FileMode(after.Mode).Perm() != mode) {
		statErr = errors.New("entry changed while reading")
	}
	closeErr := file.Close()
	if statErr != nil || closeErr != nil {
		return nil, entryStamp{}, errors.Join(statErr, closeErr)
	}
	if int64(len(content)) != named.Size {
		return nil, entryStamp{}, errors.New("entry byte length changed while reading")
	}
	return content, stampOf(after), nil
}

func stampOf(stat unix.Stat_t) entryStamp {
	mtimeS, mtimeN, ctimeS, ctimeN := statTimes(stat)
	return entryStamp{
		device: uint64(stat.Dev), inode: uint64(stat.Ino), size: stat.Size,
		mode: uint32(stat.Mode), links: uint64(stat.Nlink),
		mtimeS: mtimeS, mtimeN: mtimeN, ctimeS: ctimeS, ctimeN: ctimeN,
	}
}

func readDirectoryBounded(directory *os.File, maximum int) ([]os.DirEntry, error) {
	if maximum < 0 {
		return nil, errors.New("directory entry bound is invalid")
	}
	entries := make([]os.DirEntry, 0, maximum)
	for len(entries) <= maximum {
		chunk, err := directory.ReadDir(maximum + 1 - len(entries))
		entries = append(entries, chunk...)
		if len(entries) > maximum {
			return nil, errors.New("directory exceeds its entry bound")
		}
		if errors.Is(err, io.EOF) {
			return entries, nil
		}
		if err != nil {
			return nil, err
		}
		if len(chunk) == 0 {
			return nil, errors.New("directory enumeration made no progress")
		}
	}
	return nil, errors.New("directory exceeds its entry bound")
}

func verifyCommittedPackStable(root *os.File, manifest entryStamp, objects map[string]entryStamp) error {
	if err := requireDirectoryMode(root, 0o500); err != nil {
		return err
	}
	reopenedRoot, err := reopenDirectory(root)
	if err != nil {
		return err
	}
	defer reopenedRoot.Close()
	entries, err := readDirectoryBounded(reopenedRoot, 2)
	if err != nil || !namedEntries(entries, ManifestName, "objects") {
		return errors.New("pack root changed while verifying")
	}
	if current, err := statEntryAt(reopenedRoot, ManifestName); err != nil || current != manifest {
		return errors.New("manifest changed while verifying")
	}
	device, err := directoryDevice(reopenedRoot)
	if err != nil {
		return err
	}
	objectsDirectory, err := openChildDirectory(reopenedRoot, "objects", device)
	if err != nil {
		return err
	}
	defer objectsDirectory.Close()
	if err := requireDirectoryMode(objectsDirectory, 0o500); err != nil {
		return err
	}
	entries, err = readDirectoryBounded(objectsDirectory, 1)
	if err != nil || len(entries) != 1 || entries[0].Name() != "sha256" {
		return errors.New("objects directory changed while verifying")
	}
	digestDirectory, err := openChildDirectory(objectsDirectory, "sha256", device)
	if err != nil {
		return err
	}
	defer digestDirectory.Close()
	if err := requireDirectoryMode(digestDirectory, 0o500); err != nil {
		return err
	}
	entries, err = readDirectoryBounded(digestDirectory, len(objects))
	if err != nil || len(entries) != len(objects) {
		return errors.New("object set changed while verifying")
	}
	for _, entry := range entries {
		expected, ok := objects[entry.Name()]
		if !ok {
			return errors.New("object set changed while verifying")
		}
		current, err := statEntryAt(digestDirectory, entry.Name())
		if err != nil || current != expected {
			return errors.New("object identity changed while verifying")
		}
	}
	return nil
}

func reopenDirectory(directory *os.File) (*os.File, error) {
	fd, err := unix.Openat(int(directory.Fd()), ".", unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_DIRECTORY, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), directory.Name()), nil
}

func statEntryAt(directory *os.File, name string) (entryStamp, error) {
	var stat unix.Stat_t
	if err := unix.Fstatat(int(directory.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return entryStamp{}, err
	}
	return stampOf(stat), nil
}

func requireDirectoryMode(directory *os.File, mode os.FileMode) error {
	info, err := directory.Stat()
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm() != mode {
		return errors.New("pack directory mode is invalid")
	}
	return nil
}

func namedEntries(entries []os.DirEntry, names ...string) bool {
	want := make(map[string]struct{}, len(names))
	for _, name := range names {
		want[name] = struct{}{}
	}
	for _, entry := range entries {
		if _, ok := want[entry.Name()]; !ok {
			return false
		}
		delete(want, entry.Name())
	}
	return len(want) == 0
}

func openOrCreatePackDirectory(parent *os.File, name string, device uint64) (*os.File, error) {
	if err := unix.Mkdirat(int(parent.Fd()), name, 0o700); err != nil && !errors.Is(err, unix.EEXIST) {
		return nil, err
	}
	directory, err := openChildDirectory(parent, name, device)
	if err != nil {
		return nil, err
	}
	info, err := directory.Stat()
	if err != nil {
		_ = directory.Close()
		return nil, err
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 {
		_ = directory.Close()
		return nil, fmt.Errorf("pack directory %s is not a private 0700 directory", name)
	}
	return directory, nil
}

func entryExistsAt(directory *os.File, name string) (bool, error) {
	var stat unix.Stat_t
	err := unix.Fstatat(int(directory.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW)
	if errors.Is(err, unix.ENOENT) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func unlinkAt(directory *os.File, name string) error {
	err := unix.Unlinkat(int(directory.Fd()), name, 0)
	if errors.Is(err, unix.ENOENT) {
		return os.ErrNotExist
	}
	return err
}

func syncOpenDirectory(directory *os.File) error {
	err := directory.Sync()
	if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOTSUP) {
		return nil
	}
	return err
}

func restoreIncompleteAt(root *os.File) error {
	if err := root.Chmod(0o700); err != nil {
		return err
	}
	exists, err := entryExistsAt(root, IncompleteMarkerName)
	if err != nil {
		return err
	}
	if !exists {
		if err := writeExclusiveAt(root, IncompleteMarkerName, []byte(incompleteMarker), 0o600); err != nil {
			return err
		}
	}
	return syncOpenDirectory(root)
}

func ensureCleanupOrphanAttribute(root *os.File) error {
	if err := unix.Fsetxattr(
		int(root.Fd()),
		cleanupOrphanAttributeName,
		cleanupOrphanAttribute,
		0,
	); err != nil {
		return fmt.Errorf("runfs: persist cleanup-orphan ownership attribute: %w", err)
	}
	return nil
}

func hasCleanupOrphanAttribute(path string, identity os.FileInfo) (bool, error) {
	root, err := openOwnedRoot(path, identity)
	if err != nil {
		return false, err
	}
	defer root.Close()
	content := make([]byte, len(cleanupOrphanAttribute)+1)
	count, err := unix.Fgetxattr(int(root.Fd()), cleanupOrphanAttributeName, content)
	if isNoAttribute(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return count == len(cleanupOrphanAttribute) && string(content[:count]) == string(cleanupOrphanAttribute), nil
}

func validateSentinelAt(directory *os.File, name, expected string) error {
	fd, err := unix.Openat(int(directory.Fd()), name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("runfs: open %s: %w", name, err)
	}
	file := os.NewFile(uintptr(fd), name)
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return fmt.Errorf("runfs: %s is not a private regular file", name)
	}
	content, err := io.ReadAll(io.LimitReader(file, int64(len(expected)+1)))
	if err != nil {
		return fmt.Errorf("runfs: read %s: %w", name, err)
	}
	if string(content) != expected {
		return fmt.Errorf("runfs: %s content is invalid", name)
	}
	return nil
}

func createManifestTemporary(root *os.File, manifest []byte) (string, error) {
	for attempts := 0; attempts < 128; attempts++ {
		name, err := randomEntryName(".manifest-", ".tmp")
		if err != nil {
			return "", err
		}
		fd, err := unix.Openat(int(root.Fd()), name,
			unix.O_CREAT|unix.O_EXCL|unix.O_WRONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o400)
		if errors.Is(err, unix.EEXIST) {
			continue
		}
		if err != nil {
			return "", err
		}
		file := os.NewFile(uintptr(fd), name)
		writeErr := unix.Fchmod(fd, 0o400)
		if writeErr == nil {
			_, writeErr = file.Write(manifest)
		}
		if writeErr == nil {
			writeErr = file.Sync()
		}
		closeErr := file.Close()
		if writeErr != nil || closeErr != nil {
			_ = unix.Unlinkat(int(root.Fd()), name, 0)
			return "", errors.Join(writeErr, closeErr)
		}
		return name, nil
	}
	return "", errors.New("runfs: could not allocate manifest staging name")
}

func removeOwnedTree(path string, identity os.FileInfo) error {
	parent, root, err := openOwnedRootAndParent(path, identity)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	device, err := directoryDevice(root)
	if err == nil {
		err = removeDirectoryContents(root, device, "")
	}
	if err == nil {
		err = removeDirectoryEntry(parent, filepath.Base(path), root)
	} else {
		err = errors.Join(err, root.Close())
	}
	parentCloseErr := parent.Close()
	return errors.Join(err, parentCloseErr)
}

func cleanupOwnedTree(
	path string,
	identity os.FileInfo,
	beforeRemove func() error,
	afterMarkerRemoval func() error,
) error {
	parent, root, err := openOwnedRootAndParent(path, identity)
	if err != nil {
		return err
	}
	defer parent.Close()
	defer root.Close()
	if err := unix.Fchmod(int(root.Fd()), 0o700); err != nil {
		return err
	}
	if err := validateOrCreateCleaningMarker(root); err != nil {
		return err
	}
	if err := ensureCleanupOrphanAttribute(root); err != nil {
		return err
	}
	if err := unix.Fsync(int(root.Fd())); err != nil && err != unix.EINVAL && err != unix.ENOTSUP {
		return err
	}
	device, err := directoryDevice(root)
	if err != nil {
		return err
	}
	if err := removeDirectoryContents(root, device, cleanupMarkerName); err != nil {
		return err
	}
	if beforeRemove != nil {
		if err := beforeRemove(); err != nil {
			return err
		}
	}
	// The root is now closed and empty except for the cleanup marker. If a crash
	// lands after the unlink below, Inspect reports an orphan, which still never
	// grants implicit deletion authority.
	if err := unix.Fchmod(int(root.Fd()), 0o700); err != nil {
		return err
	}
	if err := unix.Fsync(int(root.Fd())); err != nil && err != unix.EINVAL && err != unix.ENOTSUP {
		return err
	}
	if err := unix.Unlinkat(int(root.Fd()), cleanupMarkerName, 0); err != nil {
		return fmt.Errorf("remove cleanup marker: %w", err)
	}
	if err := unix.Fsync(int(root.Fd())); err != nil && err != unix.EINVAL && err != unix.ENOTSUP {
		return err
	}
	if afterMarkerRemoval != nil {
		if err := afterMarkerRemoval(); err != nil {
			return err
		}
	}
	return removeDirectoryEntry(parent, filepath.Base(path), root)
}

func validateOrCreateCleaningMarker(root *os.File) error {
	var stat unix.Stat_t
	err := unix.Fstatat(int(root.Fd()), cleanupMarkerName, &stat, unix.AT_SYMLINK_NOFOLLOW)
	if err == nil {
		return validateSentinelAt(root, cleanupMarkerName, cleanupMarker)
	}
	if !errors.Is(err, unix.ENOENT) {
		return err
	}
	return writeExclusiveAt(root, cleanupMarkerName, []byte(cleanupMarker), 0o600)
}

func removeDirectoryContents(directory *os.File, device uint64, preserve string) error {
	if err := unix.Fchmod(int(directory.Fd()), 0o700); err != nil {
		return err
	}
	entries, err := directory.ReadDir(-1)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() == preserve {
			continue
		}
		var stat unix.Stat_t
		if err := unix.Fstatat(int(directory.Fd()), entry.Name(), &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return err
		}
		if stat.Mode&unix.S_IFMT == unix.S_IFDIR {
			child, err := openChildDirectory(directory, entry.Name(), device)
			if err != nil {
				return fmt.Errorf("runfs: refuse cleanup across mount at %s: %w", entry.Name(), err)
			}
			removeErr := removeDirectoryContents(child, device, "")
			closeErr := child.Close()
			if removeErr != nil || closeErr != nil {
				return errors.Join(removeErr, closeErr)
			}
			if err := unix.Unlinkat(int(directory.Fd()), entry.Name(), unix.AT_REMOVEDIR); err != nil {
				return err
			}
			continue
		}
		if err := unix.Unlinkat(int(directory.Fd()), entry.Name(), 0); err != nil {
			return fmt.Errorf("unlink %s: %w", entry.Name(), err)
		}
	}
	if err := unix.Fsync(int(directory.Fd())); err != nil && err != unix.EINVAL && err != unix.ENOTSUP {
		return err
	}
	return nil
}

func removeDirectoryEntry(parent *os.File, name string, owned *os.File) error {
	var expected unix.Stat_t
	if err := unix.Fstat(int(owned.Fd()), &expected); err != nil {
		return fmt.Errorf("stat opened cleanup directory: %w", err)
	}
	if err := owned.Close(); err != nil {
		return fmt.Errorf("close cleanup directory: %w", err)
	}
	var current unix.Stat_t
	if err := unix.Fstatat(int(parent.Fd()), name, &current, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return fmt.Errorf("stat cleanup entry: %w", err)
	}
	if current.Dev != expected.Dev || current.Ino != expected.Ino || current.Mode&unix.S_IFMT != unix.S_IFDIR {
		return errors.New("runfs: cleanup directory entry identity changed")
	}
	if err := unix.Unlinkat(int(parent.Fd()), name, unix.AT_REMOVEDIR); err != nil {
		return fmt.Errorf("remove cleanup directory entry: %w", err)
	}
	if err := unix.Fsync(int(parent.Fd())); err != nil && err != unix.EINVAL && err != unix.ENOTSUP {
		return err
	}
	return nil
}
