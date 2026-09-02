//go:build !darwin && !linux

package runfs

import "os"

func ReadPrivateFile(string, os.FileMode, int64) ([]byte, error) {
	return nil, ensureSupportedPlatform()
}
