package managed

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixedClock() func() time.Time {
	return func() time.Time { return time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC) }
}

// A managed hook cannot depend on the provider's working directory, so a relative
// binary path is refused rather than rendered into a file that would silently not
// run.
func TestPlanInstall_RequiresAbsoluteBinary(t *testing.T) {
	for _, bin := range []string{"", "  ", "openbox", "./openbox"} {
		if _, err := PlanInstall([]Provider{ProviderClaudeCode}, bin); err == nil {
			t.Errorf("PlanInstall(%q) should refuse a non-absolute binary path", bin)
		}
	}
}

// The rendered file must invoke the deployed binary, and the placeholder must be
// gone — a leftover placeholder would produce a hook that cannot execute.
func TestPlanInstall_SubstitutesBinaryPath(t *testing.T) {
	plan, err := PlanInstall([]Provider{ProviderClaudeCode, ProviderCodex}, "/opt/openbox/bin/openbox")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(plan.Files) < 2 {
		t.Fatalf("expected files for both providers, got %d", len(plan.Files))
	}
	// No file may keep the placeholder — a leftover would render a hook that
	// cannot execute. Not every file invokes the binary, though: Codex's
	// managed_config.toml sets approval/sandbox DEFAULTS and carries no hook, so
	// the requirement is per-provider rather than per-file.
	invokes := map[string]bool{}
	for _, f := range plan.Files {
		body := string(f.Contents)
		if strings.Contains(body, binPlaceholder) {
			t.Errorf("%s still contains the %s placeholder", f.Path, binPlaceholder)
		}
		if strings.Contains(body, "/opt/openbox/bin/openbox") {
			invokes[filepath.Dir(f.Path)] = true
		}
	}
	if len(invokes) < 2 {
		t.Errorf("each provider needs at least one file invoking the binary, got %v", invokes)
	}
}

func TestApply_IdempotentAndBacksUp(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "managed-settings.json")
	plan := Plan{Files: []File{{Path: target, Contents: []byte("v2"), Mode: 0o644}}}

	// First write.
	out, err := Apply(plan, false, fixedClock())
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if out[0].Action != "written" {
		t.Errorf("first apply = %q, want written", out[0].Action)
	}

	// Re-running must not churn the file — a config-management loop runs this
	// repeatedly and should be a no-op when nothing changed.
	out, err = Apply(plan, false, fixedClock())
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if out[0].Action != "unchanged" {
		t.Errorf("second apply = %q, want unchanged", out[0].Action)
	}

	// Replacing different contents preserves the old file: this overwrites org
	// security configuration, so "I can put it back" has to be true.
	if err := os.WriteFile(target, []byte("v1-operator-edited"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err = Apply(plan, false, fixedClock())
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if out[0].BackupPath == "" {
		t.Fatal("an existing file must be backed up before replacement")
	}
	backup, err := os.ReadFile(out[0].BackupPath)
	if err != nil || string(backup) != "v1-operator-edited" {
		t.Errorf("backup did not preserve the previous contents: %q (%v)", backup, err)
	}
}

// The property that matters most: never quietly downgrade a mandate. A visible
// failure is recoverable; a silent downgrade is what an attacker would want.
func TestApply_RefusesToWeaken(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "managed-settings.json")
	strict := `{"allowManagedHooksOnly": true, "sandbox": {"failIfUnavailable": true}}`
	if err := os.WriteFile(target, []byte(strict), 0o644); err != nil {
		t.Fatal(err)
	}

	lax := Plan{Files: []File{{Path: target, Contents: []byte(`{"hooks": {}}`), Mode: 0o644}}}
	out, err := Apply(lax, false, fixedClock())
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if out[0].Action != "skipped" {
		t.Fatalf("action = %q, want skipped (the existing file is stricter)", out[0].Action)
	}
	if !strings.Contains(out[0].Detail, "--force") {
		t.Errorf("the skip must tell the operator how to override it, got %q", out[0].Detail)
	}
	if body, _ := os.ReadFile(target); string(body) != strict {
		t.Error("the stricter file must be left in place")
	}

	// --force is the deliberate override.
	out, err = Apply(lax, true, fixedClock())
	if err != nil {
		t.Fatalf("forced apply: %v", err)
	}
	if out[0].Action != "written" {
		t.Errorf("forced action = %q, want written", out[0].Action)
	}
}

// Replacing a file with an equally-strict or stricter one is not a downgrade and
// must proceed, or upgrades would be impossible.
func TestApply_AllowsEqualOrStricter(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "managed-settings.json")
	if err := os.WriteFile(target, []byte(`{"allowManagedHooksOnly": true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	stricter := Plan{Files: []File{{
		Path:     target,
		Contents: []byte(`{"allowManagedHooksOnly": true, "sandbox": {"failIfUnavailable": true}}`),
		Mode:     0o644,
	}}}
	out, err := Apply(stricter, false, fixedClock())
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if out[0].Action != "written" {
		t.Errorf("action = %q, want written (the new file is stricter)", out[0].Action)
	}
}

// Every shipped template must render and be non-trivial: an embed that silently
// resolved to nothing would produce an empty managed file, i.e. no mandate at all.
func TestTemplates_AllProvidersRender(t *testing.T) {
	for _, p := range []Provider{ProviderClaudeCode, ProviderCodex} {
		plan, err := PlanInstall([]Provider{p}, "/opt/openbox")
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		for _, f := range plan.Files {
			if len(f.Contents) < 200 {
				t.Errorf("%s: %s rendered only %d bytes — template probably missing",
					p, f.Path, len(f.Contents))
			}
		}
	}
}
