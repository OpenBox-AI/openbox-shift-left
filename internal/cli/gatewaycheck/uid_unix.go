//go:build !windows

package gatewaycheck

import (
	"io/fs"
	"syscall"
)

func init() {
	statUID = func(info fs.FileInfo) int {
		if st, ok := info.Sys().(*syscall.Stat_t); ok {
			return int(st.Uid)
		}
		return -1
	}
}
