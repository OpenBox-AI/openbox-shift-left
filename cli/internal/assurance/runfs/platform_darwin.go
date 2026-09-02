//go:build darwin

package runfs

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

const cleanupOrphanAttributeName = "com.openbox.project-assurance.orphan"

func isNoAttribute(err error) bool {
	return errors.Is(err, unix.ENOATTR)
}

func ensureSupportedPlatform() error {
	return nil
}

func statTimes(stat unix.Stat_t) (int64, int64, int64, int64) {
	return stat.Mtim.Sec, stat.Mtim.Nsec, stat.Ctim.Sec, stat.Ctim.Nsec
}

func renameNoReplaceAt(fromDirectory *os.File, from string, toDirectory *os.File, to string) error {
	return unix.RenameatxNp(
		int(fromDirectory.Fd()), from,
		int(toDirectory.Fd()), to,
		unix.RENAME_EXCL,
	)
}

func openChildDirectory(parent *os.File, name string, device uint64) (*os.File, error) {
	var parentFilesystem unix.Statfs_t
	if err := unix.Fstatfs(int(parent.Fd()), &parentFilesystem); err != nil {
		return nil, err
	}
	fd, err := unix.Openat(int(parent.Fd()), name,
		unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_DIRECTORY, 0)
	if err != nil {
		return nil, err
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	if uint64(stat.Dev) != device {
		_ = unix.Close(fd)
		return nil, unix.EXDEV
	}
	var childFilesystem unix.Statfs_t
	if err := unix.Fstatfs(fd, &childFilesystem); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	if childFilesystem.Fsid != parentFilesystem.Fsid {
		_ = unix.Close(fd)
		return nil, unix.EXDEV
	}
	return os.NewFile(uintptr(fd), name), nil
}
