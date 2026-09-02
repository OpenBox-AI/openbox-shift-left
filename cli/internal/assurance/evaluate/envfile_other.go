//go:build !darwin && !linux

package evaluate

import "errors"

func readEnvironmentFileNoFollow(string) ([]byte, error) {
	return nil, errors.New("project evaluate: environment files are unsupported on this platform")
}
