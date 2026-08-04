package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/adapters/common/devconfig"
)

func TestExtractRole(t *testing.T) {
	cases := []struct {
		args []string
		want devconfig.Role
		rest string
	}{
		{[]string{"--provider", "claude-code"}, devconfig.RoleDev, "--provider claude-code"},
		{[]string{"--role", "approver", "--org", "acme"}, devconfig.RoleApprover, "--org acme"},
		{[]string{"--role=approver"}, devconfig.RoleApprover, ""},
		{[]string{"--role", "dev", "--enforce"}, devconfig.RoleDev, "--enforce"},
	}
	for _, c := range cases {
		got, rest, err := extractRole(c.args)
		if err != nil {
			t.Fatalf("extractRole(%v): %v", c.args, err)
		}
		if got != c.want {
			t.Errorf("extractRole(%v) role = %q, want %q", c.args, got, c.want)
		}
		if strings.Join(rest, " ") != c.rest {
			t.Errorf("extractRole(%v) rest = %q, want %q", c.args, strings.Join(rest, " "), c.rest)
		}
	}
	if _, _, err := extractRole([]string{"--role", "auditor"}); err == nil {
		t.Error("an unknown role was accepted; it must fail rather than install the wrong principal")
	}
	if _, _, err := extractRole([]string{"--role"}); err == nil {
		t.Error("a valueless --role was accepted")
	}
}

// The default role installs a developer runtime, and it is the ONLY spelling:
// `openbox dev init` must be gone rather than quietly still working, or there
// are two onboarding paths to keep true in every doc.
func TestInitDefaultsToTheDeveloperRole(t *testing.T) {
	a, out, _ := testApp(nil)
	if code := a.runInit([]string{"--provider", "claude-code", "--dry-run"}); code != exitOK {
		t.Fatalf("openbox init --provider claude-code --dry-run = %d, want 0", code)
	}
	if plan := out.String(); !strings.Contains(plan, "developer agent") {
		t.Errorf("the default role did not plan a developer install:\n%s", plan)
	}
}

func TestDevInitIsGone(t *testing.T) {
	a, _, errb := testApp(nil)
	if code := a.runDev([]string{"init", "--provider", "claude-code", "--dry-run"}); code == exitOK {
		t.Error("`openbox dev init` still succeeds; it must not run at all")
	}
	if msg := errb.String(); !strings.Contains(msg, "openbox init") {
		t.Errorf("the error does not point at the surviving spelling:\n%s", msg)
	}
	// `dev` keeps the commands that operate on an existing install.
	b, _, errb2 := testApp(nil)
	if code := b.runDev([]string{"nope"}); code == exitOK {
		t.Error("an unknown dev subcommand succeeded")
	}
	if usage := errb2.String(); !strings.Contains(usage, "verify|sync") {
		t.Errorf("dev usage still advertises init:\n%s", usage)
	}
}

func TestApproverInitNeedsAnOrgAndABackend(t *testing.T) {
	a, _, _ := testApp(nil)
	if code := a.runInit([]string{"--role", "approver"}); code == exitOK {
		t.Error("approver init succeeded with no --org; it must name the queue it works")
	}
	b, _, _ := testApp(nil)
	if code := b.runInit([]string{"--role", "approver", "--org", "acme.example"}); code == exitOK {
		t.Error("approver init succeeded with no backend URL")
	}
}

// An approver install must not touch the developer's config: they are two
// principals, and the whole reason for the split is that neither can act as
// the other.
func TestApproverInitLeavesTheDeveloperConfigAlone(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv(devconfig.EnvConfigPath, "")
	t.Setenv(devconfig.EnvApproverConfigPath, "")

	a, out, _ := testApp(nil)
	code := a.runInit([]string{"--role", "approver", "--org", "acme.example",
		"--backend-url", "http://localhost:3000", "--dry-run"})
	if code != exitOK {
		t.Fatalf("approver dry-run = %d, want 0", code)
	}
	if plan := out.String(); !strings.Contains(plan, "no agent is registered, no hooks are installed") {
		t.Errorf("the plan does not say an approver registers nothing:\n%s", plan)
	}
	if _, err := os.Stat(filepath.Join(dir, "openbox", "dev.json")); !os.IsNotExist(err) {
		t.Error("approver init wrote (or found) a developer config")
	}
}
