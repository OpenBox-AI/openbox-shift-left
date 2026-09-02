//go:build darwin

package securityskill

import (
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func renameNoReplace(source, target string) error {
	return unix.RenameatxNp(unix.AT_FDCWD, source, unix.AT_FDCWD, target, unix.RENAME_EXCL)
}

func singleLink(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Nlink == 1
}
