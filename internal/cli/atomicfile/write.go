//go:build !windows

// Package atomicfile writes a file atomically. A crash or a full disk part-way
// through a plain os.WriteFile truncates ~/.claude/settings.json to invalid
// JSON; and every reader here refuses to rewrite a file it cannot parse, so
// that truncation would block its own repair. Atomicity bounds the damage to a
// lost update rather than a corrupt file; a lock is what would close the race,
// and this package deliberately does not claim to have one.
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
