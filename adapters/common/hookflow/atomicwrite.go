//go:build !windows

package hookflow

import (
	"os"

	"github.com/google/renameio/v2"
)

// atomicWriteFile writes data to path atomically, creating it with perm.
//
// renameio rather than a hand-rolled CreateTemp→Write→Close→Rename, because the
// hand-rolled shape fsynced nothing: the rename was durable while the CONTENTS
// were not, so a crash could leave a record that looks committed and reads as
// empty. renameio fsyncs the file and its parent directory before renaming, and
// it places its temp file in the destination's own directory — which is what
// makes the rename atomic rather than a cross-device copy.
//
// UNIX ONLY, and that is the package's own boundary rather than ours: renameio
// declares `//go:build !windows` on every file, so on Windows the package is
// empty. atomicwrite_windows.go keeps the previous behavior there. The
// consequence is honest and worth stating: Windows gets atomicity without the
// fsync, exactly as before this change.
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	return renameio.WriteFile(path, data, perm)
}
