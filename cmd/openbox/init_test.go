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

func TestDevInitIsGone(t *testing.T) {
	a, _, errb := testApp(nil)
	if code := a.runDev([]string{"init", "--provider", "claude-code"}); code == exitOK {
		t.Error("`openbox dev init` still succeeds; it must not run at all")
	}
	if msg := errb.String(); !strings.Contains(msg, "openbox init") {
		t.Errorf("the error does not point at the surviving spelling:\n%s", msg)
	}
	b, _, errb2 := testApp(nil)
	if code := b.runDev([]string{"nope"}); code == exitOK {
		t.Error("an unknown dev subcommand succeeded")
	}
	if usage := errb2.String(); !strings.Contains(usage, "dev verify") || strings.Contains(usage, "sync") {
		t.Errorf("dev usage must advertise verify and nothing else:\n%s", usage)
	}
}
