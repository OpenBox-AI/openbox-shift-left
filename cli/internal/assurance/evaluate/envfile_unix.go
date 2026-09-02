//go:build darwin || linux

package evaluate

import (
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

func readEnvironmentFileNoFollow(filename string) ([]byte, error) {
	fd, err := unix.Open(filename, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("project evaluate: open environment file without following symlinks: %w", err)
	}
	file := os.NewFile(uintptr(fd), filename)
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("project evaluate: inspect environment file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("project evaluate: environment file must be a regular non-symlink file")
	}
	if info.Size() > 64<<10 {
		return nil, errors.New("project evaluate: environment file exceeds 64 KiB")
	}
	content, err := io.ReadAll(io.LimitReader(file, (64<<10)+1))
	if err != nil {
		return nil, fmt.Errorf("project evaluate: read environment file: %w", err)
	}
	if len(content) > 64<<10 {
		return nil, errors.New("project evaluate: environment file exceeds 64 KiB")
	}
	return content, nil
}
