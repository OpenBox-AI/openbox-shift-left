//go:build !windows

// Package atomicfile writes a file atomically. A crash or a full disk part-way
// through a plain os.WriteFile truncates ~/.claude/settings.json to invalid
// JSON; and every reader here refuses to rewrite a file it cannot parse, so
// that truncation would block its own repair. Atomicity bounds the damage to a
// lost update rather than a corrupt file; a lock is what would close the race,
// and this package deliberately does not claim to have one.
//
// Both branches write a temporary file, fsync it, and rename. Neither fsyncs
// the parent directory, so a power failure can lose the rename -- which leaves
// the previous file whole, and a lost update is the bound above. The split is
// two implementations of one guarantee, not two guarantees; see write_windows.go.
package atomicfile

import (
	"os"

	"github.com/google/renameio/v2"
)

// Write writes data to path atomically, creating a new file with perm. A
// caller that requires a mode; the activation record, which can hold a
// displaced relay URL with an embedded credential; must chmod after writing
// rather than trust this argument.
func Write(path string, data []byte, perm os.FileMode) error {
	return renameio.WriteFile(path, data, perm)
}
