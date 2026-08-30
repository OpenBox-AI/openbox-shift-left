//go:build !windows

package hookflow

import "syscall"

// detachAttr detaches the spawned flusher into its own session (Setsid) so a
// provider that signals the hook's process group on timeout or teardown cannot
// take the flusher down with it mid-drain.
func detachAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
