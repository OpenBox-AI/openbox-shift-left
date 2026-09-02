//go:build !darwin && !linux

package snapshot

import "errors"

func copySnapshot(_ *Project, _ string, _ selectedSource) (*Snapshot, error) {
	return nil, errors.New("snapshot: safe source copying is not supported on this platform")
}

func hashSelectedFiles(_ *Project, _ []Entry) ([]File, error) {
	return nil, errors.New("snapshot: safe source hashing is not supported on this platform")
}

func sealSnapshot(_ *Snapshot) error {
	return errors.New("snapshot: safe snapshot sealing is not supported on this platform")
}
