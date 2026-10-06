//go:build !windows

package hookflow

import (
	"os"

	"github.com/google/renameio/v2"
)

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
