//go:build !windows

package gatewaycheck

import (
	"io/fs"
	"syscall"
)

// On unix the owning uid is what separates the MDM tier (root) from the base tier
// (the developer), so tier detection here is a real observation rather than a
// claim OpenBox writes down about itself.
func init() {
	statUID = func(info fs.FileInfo) int {
		if st, ok := info.Sys().(*syscall.Stat_t); ok {
			return int(st.Uid)
		}
		return -1
	}
}
