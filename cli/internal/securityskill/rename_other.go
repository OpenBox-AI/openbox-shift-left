//go:build !darwin && !linux

package securityskill

import (
	"errors"
	"os"
)

func renameNoReplace(string, string) error {
	return errors.New("security skill: atomic no-replace rename unsupported on this platform")
}

func singleLink(os.FileInfo) bool { return true }
