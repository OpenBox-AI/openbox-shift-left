package hookflow

import "os"

// AtomicWriteFile exports atomicWriteFile (the renameio-backed committer on
// !windows, the CreateTemp+Sync+Rename one on windows -- see atomicwrite.go
// and atomicwrite_windows.go) for a caller outside this package.
//
// internal/adapters/claude-code writes its Claude Code prior-settings restore
// record through this seam rather than internal/cli/atomicfile, which would
// invert the adapter/cli dependency direction: adapter code may depend on
// adapters/common, never on internal/cli.
func AtomicWriteFile(path string, data []byte, perm os.FileMode) error {
	return atomicWriteFile(path, data, perm)
}
