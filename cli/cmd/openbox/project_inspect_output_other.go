//go:build !darwin && !linux

package main

import "errors"

func ensureInspectionOutputSupported() error {
	return errors.New("inspection output is not supported on this platform")
}

func publishInspectionDirectory(_, _, _ string) error {
	return errors.New("inspection output is not supported on this platform")
}
