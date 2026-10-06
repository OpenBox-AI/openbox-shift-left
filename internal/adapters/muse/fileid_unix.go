//go:build !windows

package muse

import (
	"os"
	"syscall"
)

// fileIdentity is the file's inode, so a replaced log is told from a grown one.
func fileIdentity(info os.FileInfo) uint64 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Ino)
	}
	return 0
}
