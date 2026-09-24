package main

import (
	"bytes"
	"log"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
)

// TestLaneSpoolDirPrecedence pins the four-tier order a lane daemon's own
// spool resolution relies on: OPENBOX_SPOOL_DIR (the whole path) >
// OPENBOX_SPOOL_ROOT/subdir > filepath.Dir(OPENBOX_HALT_DIR)/subdir (an
// older install that predates OPENBOX_SPOOL_ROOT, but still carries
// OPENBOX_HALT_DIR unconditionally) > devconfig.SpoolDir's own fallback.
func TestLaneSpoolDirPrecedence(t *testing.T) {
	t.Setenv(devconfig.EnvSpoolDir, "")
	t.Setenv(devconfig.EnvSpoolRoot, "")
	t.Setenv(devconfig.EnvHaltDir, "")

	// All three unset: falls all the way through to devconfig.SpoolDir's own
	// fallback (whatever that resolves to; only shape-checked here, its own
	// precedence is devconfig's own test).
	fallback := laneSpoolDir("cc-spool", nil)
	if filepath.Base(fallback) != "cc-spool" {
		t.Fatalf("with nothing set, laneSpoolDir = %q, want a path ending in cc-spool", fallback)
	}

	// OPENBOX_HALT_DIR set, root unset: derives from the halt dir's own
	// parent -- an older unit's own shape (DefaultHaltDir ==
	// ConfigDir()/halted-sessions, so its parent IS ConfigDir()).
	haltDir := filepath.Join("/config-root", "halted-sessions")
	t.Setenv(devconfig.EnvHaltDir, haltDir)
	var logBuf bytes.Buffer
	logger := log.New(&logBuf, "", 0)
	got := laneSpoolDir("cc-spool", logger)
	want := filepath.Join("/config-root", "cc-spool")
	if got != want {
		t.Fatalf("with only OPENBOX_HALT_DIR set, laneSpoolDir = %q, want %q", got, want)
	}
	if !strings.Contains(logBuf.String(), devconfig.EnvSpoolRoot) {
		t.Errorf("resolving from the halt dir's own parent must be logged (an operator needs to know "+
			"to re-run `openbox init`); log = %q", logBuf.String())
	}

	// OPENBOX_SPOOL_ROOT set: outranks the halt-dir fallback.
	t.Setenv(devconfig.EnvSpoolRoot, "/root-only")
	if got := laneSpoolDir("cc-spool", nil); got != filepath.Join("/root-only", "cc-spool") {
		t.Fatalf("with OPENBOX_SPOOL_ROOT set, laneSpoolDir = %q, want /root-only/cc-spool", got)
	}

	// OPENBOX_SPOOL_DIR set: outranks everything else, verbatim.
	t.Setenv(devconfig.EnvSpoolDir, "/pinned/whole/path")
	if got := laneSpoolDir("cc-spool", nil); got != "/pinned/whole/path" {
		t.Fatalf("with OPENBOX_SPOOL_DIR set, laneSpoolDir = %q, want /pinned/whole/path", got)
	}
}
