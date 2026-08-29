//go:build !windows

// Package atomicfile writes a file atomically.
//
// It exists as its own package because three lanes now rewrite the developer's
// settings file and the activation record beside it, and a second copy of this
// helper is exactly the shape this repo already paid for once: the engine was
// copy-pasted per adapter, the copies drifted, and the drift was on the
// enforcement path. One writer, tagged for the two platforms once.
//
// The property that matters is not speed but the DIFFERENCE between "the old
// file or the new file" and "an arbitrary prefix of the new file". A crash or a
// full disk part-way through a plain os.WriteFile truncates
// ~/.claude/settings.json to invalid JSON — and every reader here refuses to
// rewrite a file it cannot parse, so that truncation would block its own repair.
//
// This does NOT make concurrent writers safe. Two inits, or an init racing the
// tool's own writer, still read-modify-write and the last rename wins with no
// error either way. Atomicity bounds the damage to a lost update rather than a
// corrupt file; a lock is what would close the race, and this package
// deliberately does not claim to have one.
package atomicfile

import (
	"os"

	"github.com/google/renameio/v2"
)

// Write writes data to path atomically, creating a new file with perm.
//
// renameio rather than a hand-rolled CreateTemp→Write→Chmod→Close→Rename: it
// fsyncs the file and its parent directory before renaming, so the contents end
// up as durable as the rename, and it keeps its temp file in the destination
// directory, which is what makes the rename atomic rather than a cross-device
// copy.
//
// perm applies to a NEW file. renameio preserves an EXISTING file's mode, which
// is the behavior wanted for the developer's own settings file: its permissions
// are not an assurance boundary (doctor reports ownership as the tier signal
// precisely because a user-owned file is user-changeable), and a mode the
// developer chose should survive a rewrite. A caller that REQUIRES a mode — the
// activation record, which can hold a displaced relay URL with an embedded
// credential — must chmod after writing rather than trust this argument.
//
// UNIX ONLY — renameio declares a !windows constraint on every file, so the
// package is empty there. write_windows.go keeps the previous behavior.
func Write(path string, data []byte, perm os.FileMode) error {
	return renameio.WriteFile(path, data, perm)
}
