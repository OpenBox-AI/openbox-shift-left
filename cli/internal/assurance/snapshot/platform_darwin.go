//go:build darwin

package snapshot

import (
	"os"

	"golang.org/x/sys/unix"
)

func openChildDirectory(parent *os.File, name string, device uint64) (*os.File, error) {
	return openChild(parent, name, device, unix.O_DIRECTORY)
}

func openRegularFile(parent *os.File, name string, device uint64) (*os.File, error) {
	return openChild(parent, name, device, unix.O_NONBLOCK)
}

func openChild(parent *os.File, name string, device uint64, flags int) (*os.File, error) {
	var parentFilesystem unix.Statfs_t
	if err := unix.Fstatfs(int(parent.Fd()), &parentFilesystem); err != nil {
		return nil, err
	}
	fd, err := unix.Openat(int(parent.Fd()), name,
		unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|flags, 0)
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
