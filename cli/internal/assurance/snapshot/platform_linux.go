//go:build linux

package snapshot

import (
	"os"

	"golang.org/x/sys/unix"
)

func openChildDirectory(parent *os.File, name string, _ uint64) (*os.File, error) {
	return openChild(parent, name, unix.O_DIRECTORY)
}

func openRegularFile(parent *os.File, name string, _ uint64) (*os.File, error) {
	return openChild(parent, name, unix.O_NONBLOCK)
}

func openChild(parent *os.File, name string, flags int) (*os.File, error) {
	fd, err := unix.Openat2(int(parent.Fd()), name, &unix.OpenHow{
		Flags: uint64(unix.O_RDONLY | unix.O_CLOEXEC | flags),
		Resolve: uint64(unix.RESOLVE_BENEATH |
			unix.RESOLVE_NO_SYMLINKS |
			unix.RESOLVE_NO_XDEV),
	})
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}
