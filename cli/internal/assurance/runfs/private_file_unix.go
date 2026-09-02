//go:build darwin || linux

package runfs

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// ReadPrivateFile reads one owner-held single-link regular file without
// following its final path component and rejects replacement while reading.
func ReadPrivateFile(path string, mode os.FileMode, limit int64) ([]byte, error) {
	clean, err := filepath.Abs(path)
	if err != nil || limit < 0 {
		return nil, errors.New("runfs: invalid private file request")
	}
	var named unix.Stat_t
	if err := unix.Lstat(clean, &named); err != nil {
		return nil, err
	}
	if named.Mode&unix.S_IFMT != unix.S_IFREG || named.Nlink != 1 || os.FileMode(named.Mode).Perm() != mode || named.Uid != uint32(os.Geteuid()) || named.Size < 0 || named.Size > limit {
		return nil, errors.New("runfs: entry is not an exact owner-held single-link private regular file")
	}
	fd, err := unix.Open(clean, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), clean)
	defer file.Close()
	var opened unix.Stat_t
	if err := unix.Fstat(fd, &opened); err != nil || opened.Dev != named.Dev || opened.Ino != named.Ino || opened.Mode != named.Mode || opened.Nlink != 1 || opened.Uid != named.Uid || opened.Size != named.Size {
		return nil, errors.New("runfs: private file identity changed while opening")
	}
	content, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(content)) != named.Size {
		return nil, fmt.Errorf("runfs: read private file: %w", err)
	}
	var after unix.Stat_t
	if err := unix.Fstat(fd, &after); err != nil || stampOf(after) != stampOf(opened) {
		return nil, errors.New("runfs: private file changed while reading")
	}
	return content, nil
}
