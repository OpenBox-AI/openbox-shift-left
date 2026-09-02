//go:build darwin

package main

import (
	"os"

	"golang.org/x/sys/unix"
)

func ensureInspectionOutputSupported() error { return nil }

func publishInspectionDirectory(parent, staging, output string) error {
	directory, err := os.Open(parent)
	if err != nil {
		return err
	}
	defer directory.Close()
	return unix.RenameatxNp(int(directory.Fd()), staging, int(directory.Fd()), output, unix.RENAME_EXCL)
}
