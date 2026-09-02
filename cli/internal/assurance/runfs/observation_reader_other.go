//go:build !darwin && !linux

package runfs

import "errors"

func ReadObservation(string) (map[string][]byte, []byte, error) {
	return nil, nil, errors.New("runfs: observation reader unsupported on this platform")
}
