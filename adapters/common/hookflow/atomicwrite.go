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
//
// NewPendingFile rather than renameio.WriteFile, and the reason is perm.
// WriteFile appends WithExistingPermissions() AFTER WithPermissions(perm), and
// that option overwrites the requested mode with the mode of the file already on
// disk (renameio tempfile.go:269-288). So a record that became group- or
// world-readable for any reason kept those permissions through every subsequent
// 0600 rewrite, silently — where the hand-rolled writer this replaced chmod'd on
// every write, and where atomicwrite_windows.go still does. WithStaticPermissions
// is the option that means what the caller asked for.
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	t, err := renameio.NewPendingFile(path, renameio.WithStaticPermissions(perm))
	if err != nil {
		return err
	}
	defer t.Cleanup()
	if _, err := t.Write(data); err != nil {
		return err
	}
	return t.CloseAtomicallyReplace()
}
