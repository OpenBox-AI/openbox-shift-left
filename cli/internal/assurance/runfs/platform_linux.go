//go:build linux

package runfs

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

const cleanupOrphanAttributeName = "user.openbox.project-assurance.orphan"

func isNoAttribute(err error) bool {
	return errors.Is(err, unix.ENODATA)
}

func ensureSupportedPlatform() error {
	return nil
}

func statTimes(stat unix.Stat_t) (int64, int64, int64, int64) {
	return stat.Mtim.Sec, stat.Mtim.Nsec, stat.Ctim.Sec, stat.Ctim.Nsec
}

func renameNoReplaceAt(fromDirectory *os.File, from string, toDirectory *os.File, to string) error {
	return unix.Renameat2(
		int(fromDirectory.Fd()), from,
		int(toDirectory.Fd()), to,
		unix.RENAME_NOREPLACE,
	)
}

func openChildDirectory(parent *os.File, name string, _ uint64) (*os.File, error) {
	fd, err := unix.Openat2(int(parent.Fd()), name, &unix.OpenHow{
		Flags: uint64(unix.O_RDONLY | unix.O_CLOEXEC | unix.O_DIRECTORY),
		Resolve: uint64(unix.RESOLVE_BENEATH |
			unix.RESOLVE_NO_SYMLINKS |
			unix.RESOLVE_NO_XDEV),
	})
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}
