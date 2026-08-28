//go:build !windows

package gatewayservice

import (
	"os"

	"github.com/google/renameio/v2"
)

// writeFileAtomic writes data to path atomically, creating a new file with perm.
//
// renameio rather than a hand-rolled CreateTemp→Write→Chmod→Close→Rename: it
// fsyncs the file and its parent directory before renaming, so the contents end
// up as durable as the rename, and it keeps its temp file in the destination
// directory, which is what makes the rename atomic rather than a cross-device
// copy.
//
// perm applies to a NEW file. renameio preserves an EXISTING file's mode, which
// is the behavior wanted here: this is the developer's own settings file, its
// permissions are not an assurance boundary (doctor reports ownership as the tier
// signal precisely because a user-owned file is user-changeable), and a mode the
// developer chose should survive a rewrite.
//
// UNIX ONLY — renameio declares a !windows constraint on every file, so the
// package is empty there. atomicwrite_windows.go keeps the previous behavior.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	return renameio.WriteFile(path, data, perm)
}
