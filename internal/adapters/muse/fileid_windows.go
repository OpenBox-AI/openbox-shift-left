//go:build windows

package muse

import "os"

// fileIdentity is 0 on Windows, where the standard library exposes no file id
// without an open handle: a replaced log is then told only by being shorter
// than the recorded offset.
func fileIdentity(info os.FileInfo) uint64 { return 0 }
