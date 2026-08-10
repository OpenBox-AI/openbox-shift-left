//go:build windows

package hookflow

import "syscall"

// detachAttr detaches the spawned flusher from the hook's console/process
// group on Windows (CREATE_NEW_PROCESS_GROUP | DETACHED_PROCESS), mirroring
// the unix Setsid so provider teardown cannot kill a drain in flight.
func detachAttr() *syscall.SysProcAttr {
	const createNewProcessGroup = 0x00000200
	const detachedProcess = 0x00000008
	return &syscall.SysProcAttr{CreationFlags: createNewProcessGroup | detachedProcess}
}
