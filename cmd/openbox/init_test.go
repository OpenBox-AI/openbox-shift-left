package main

import (
	"strings"
	"testing"
)

func TestInitDefaultsToTheDeveloperRole(t *testing.T) {
	isolateHome(t)
	seedCredentials(t)
	a, out, errb := testApp(nil)
	if code := a.runDevInit([]string{"--provider", "claude-code"}); code != exitOK {
		t.Fatalf("openbox init --provider claude-code = %d, want 0; stderr=%q", code, errb.String())
	}
	// The default installs a governed developer runtime, not a queue client.
	if s := out.String(); !strings.Contains(s, "EVERY SESSION") {
		t.Errorf("the default role did not install a developer runtime:\n%s", s)
	}
}

// TestDevVerbIsGone. `openbox dev` was a second namespace with its own
// tombstones for two subcommands that had already been removed from it; the
// verb itself now goes the same way, and what `dev verify` proved is reported
// by `doctor` instead.
func TestDevVerbIsGone(t *testing.T) {
	for _, args := range [][]string{
		{"dev"},
		{"dev", "verify"},
		{"dev", "init", "--provider", "claude-code"},
		{"dev", "sync"},
	} {
		a, _, errb := testApp(nil)
		if code := a.run(args); code == exitOK {
			t.Errorf("%v still succeeds", args)
		}
		if !strings.Contains(errb.String(), "unknown command") {
			t.Errorf("%v was not refused as an unknown command: %s", args, errb.String())
		}
	}
}
