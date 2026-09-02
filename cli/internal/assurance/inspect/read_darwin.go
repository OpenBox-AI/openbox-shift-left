//go:build darwin

package inspect

import (
	"errors"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

func readManifestFile(root, relative string, expected int64) ([]byte, error) {
	rootFD, err := unix.Open(root, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_DIRECTORY, 0)
	if err != nil {
		return nil, err
	}
	rootFile := os.NewFile(uintptr(rootFD), root)
	defer rootFile.Close()
	var rootStat unix.Stat_t
	var rootFilesystem unix.Statfs_t
	if err := unix.Fstat(rootFD, &rootStat); err != nil {
		return nil, err
	}
	if err := unix.Fstatfs(rootFD, &rootFilesystem); err != nil {
		return nil, err
	}
	components := strings.Split(relative, "/")
	current := rootFile
	for _, component := range components[:len(components)-1] {
		nextFD, err := unix.Openat(int(current.Fd()), component, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_DIRECTORY, 0)
		if err != nil {
			closeManifestDescendant(rootFile, current)
			return nil, err
		}
		next := os.NewFile(uintptr(nextFD), component)
		if err := sameManifestFilesystem(nextFD, rootStat.Dev, rootFilesystem.Fsid); err != nil {
			_ = next.Close()
			closeManifestDescendant(rootFile, current)
			return nil, err
		}
		closeManifestDescendant(rootFile, current)
		current = next
	}
	fileFD, err := unix.Openat(int(current.Fd()), components[len(components)-1], unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	closeManifestDescendant(rootFile, current)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fileFD), relative)
	defer file.Close()
	if err := sameManifestFilesystem(fileFD, rootStat.Dev, rootFilesystem.Fsid); err != nil {
		return nil, err
	}
	var before unix.Stat_t
	if err := unix.Fstat(fileFD, &before); err != nil {
		return nil, err
	}
	if before.Mode&unix.S_IFMT != unix.S_IFREG || before.Nlink != 1 || before.Size != expected {
		return nil, errors.New("manifest identity, type, link count, or size changed")
	}
	content, err := readExactManifest(file, expected)
	if err != nil {
		return nil, err
	}
	var after unix.Stat_t
	if err := unix.Fstat(fileFD, &after); err != nil {
		return nil, err
	}
	if before.Dev != after.Dev || before.Ino != after.Ino || before.Size != after.Size {
		return nil, errors.New("manifest identity changed while reading")
	}
	return content, nil
}

func sameManifestFilesystem(fd int, device int32, filesystem unix.Fsid) error {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return err
	}
	if stat.Dev != device {
		return unix.EXDEV
	}
	var current unix.Statfs_t
	if err := unix.Fstatfs(fd, &current); err != nil {
		return err
	}
	if current.Fsid != filesystem {
		return unix.EXDEV
	}
	return nil
}

func closeManifestDescendant(root, current *os.File) {
	if current != root {
		_ = current.Close()
	}
}
