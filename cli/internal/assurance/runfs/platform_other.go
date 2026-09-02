//go:build !darwin && !linux

package runfs

import (
	"errors"
	"os"

	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/artifact"
)

func ensureSupportedPlatform() error {
	return errors.New("runfs: safe run-directory lifecycle is not supported on this platform")
}

func renameNoReplaceAt(_ *os.File, _ string, _ *os.File, _ string) error {
	return ensureSupportedPlatform()
}

func openDirectoryNoFollow(_ string) (*os.File, error) {
	return nil, ensureSupportedPlatform()
}

func removeOwnedTree(_ string, _ os.FileInfo) error {
	return ensureSupportedPlatform()
}

func sealOwnedContents(_ *os.File, _ os.FileInfo, _ map[string][]byte) error {
	return ensureSupportedPlatform()
}

func verifyObservationFilesAt(_ *os.File, _ map[string][]byte) error {
	return errors.New("runfs: observation verification is unsupported on this platform")
}

func entryExistsAt(_ *os.File, _ string) (bool, error) {
	return false, ensureSupportedPlatform()
}

func writeExclusiveAt(_ *os.File, _ string, _ []byte, _ os.FileMode) error {
	return ensureSupportedPlatform()
}

func writePackObjectsAt(_ *os.File, _ map[string][]byte) error {
	return ensureSupportedPlatform()
}

func verifyPackObjectsAt(_ *os.File, _ map[string][]byte) error {
	return ensureSupportedPlatform()
}

func readCommittedPackAt(_ *os.File) (*artifact.ManifestIndex, map[artifact.Role][]byte, error) {
	return nil, nil, ensureSupportedPlatform()
}

func createManifestTemporary(_ *os.File, _ []byte) (string, error) {
	return "", ensureSupportedPlatform()
}

func unlinkAt(_ *os.File, _ string) error {
	return ensureSupportedPlatform()
}

func syncOpenDirectory(_ *os.File) error {
	return ensureSupportedPlatform()
}

func restoreIncompleteAt(_ *os.File) error {
	return ensureSupportedPlatform()
}

func openOwnedRoot(_ string, _ os.FileInfo) (*os.File, error) {
	return nil, ensureSupportedPlatform()
}

func cleanupOwnedTree(_ string, _ os.FileInfo, _ func() error, _ func() error) error {
	return ensureSupportedPlatform()
}

func hasCleanupOrphanAttribute(_ string, _ os.FileInfo) (bool, error) {
	return false, ensureSupportedPlatform()
}
