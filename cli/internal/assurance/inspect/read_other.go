//go:build !darwin && !linux

package inspect

func readManifestFile(_, _ string, _ int64) ([]byte, error) {
	return nil, ErrUnsupportedPlatform
}
