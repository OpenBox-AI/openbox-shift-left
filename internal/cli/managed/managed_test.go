package managed

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
)

func fixedClock() func() time.Time {
	return func() time.Time { return time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC) }
}

// TestPlanInstall_RequiresAbsoluteBinary a managed hook cannot depend on the
// provider's working directory, so a relative binary path is refused rather
// than rendered into a file that would silently not run.
func TestPlanInstall_RequiresAbsoluteBinary(t *testing.T) {
	for _, bin := range []string{"", "  ", "openbox", "./openbox"} {
		if _, err := PlanInstall([]Provider{ProviderClaudeCode}, bin); err == nil {
			t.Errorf("PlanInstall(%q) should refuse a non-absolute binary path", bin)
		}
	}
}

// TestPlanInstall_SubstitutesBinaryPath the rendered file must invoke the
// deployed binary, and the placeholder must be gone; a leftover placeholder
// would produce a hook that cannot execute.
func TestPlanInstall_SubstitutesBinaryPath(t *testing.T) {
	plan, err := PlanInstall([]Provider{ProviderClaudeCode, ProviderCodex}, "/opt/openbox/bin/openbox")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(plan.Files) < 2 {
		t.Fatalf("expected files for both providers, got %d", len(plan.Files))
	}
	invokes := map[string]bool{}
	for _, f := range plan.Files {
		body := string(f.Contents)
		if strings.Contains(body, binPlaceholder) {
			t.Errorf("%s still contains the %s placeholder", f.Path, binPlaceholder)
		}
		if strings.Contains(body, "/opt/openbox/bin/openbox") {
			invokes[filepath.Base(f.Path)] = true
		}
	}
	// Codex's managed files cannot: `hooks` is not a requirements.toml key, and
	// managed_config.toml carries defaults only; the Codex hook is installed by
	// `openbox init --provider codex`.
	if !invokes["managed-settings.json"] {
		t.Errorf("claude-code managed settings must invoke the deployed binary, got %v", invokes)
	}
}

// TestPlanInstall_CodexMandatesAreTopLevel is the E8-S8 regression guard.
func TestPlanInstall_CodexMandatesAreTopLevel(t *testing.T) {
	plan, err := PlanInstall([]Provider{ProviderCodex}, "/opt/openbox/bin/openbox")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	var req []byte
	for _, f := range plan.Files {
		if filepath.Base(f.Path) == "requirements.toml" {
			req = f.Contents
		}
	}
	if req == nil {
		t.Fatal("codex plan has no requirements.toml")
	}
	keys := devconfig.TopLevelTOMLKeys(req)
	for _, want := range []string{"allowed_approval_policies", "allowed_sandbox_modes"} {
		if !keys[want] {
			t.Errorf("%s is not a TOP-LEVEL key in requirements.toml (nested keys are ignored by Codex); top-level keys = %v", want, keys)
		}
	}
	body := string(req)
	for _, forbidden := range []string{`"never"`, `"danger-full-access"`} {
		for _, line := range strings.Split(body, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "#") {
				continue
			}
			if strings.Contains(line, forbidden) {
				t.Errorf("requirements.toml allows %s: %q", forbidden, line)
			}
		}
	}
	if keys["allow_managed_hooks_only"] {
		t.Error("allow_managed_hooks_only is enabled but this template ships no managed hook definition — " +
			"Codex would ignore the user-level hooks.json and run no OpenBox hook (see the template comment)")
	}
}

// TestMandates_CodexRejectsNestedKeys a mandate must be recognized from a TOP-
// level key only.
func TestMandates_CodexRejectsNestedKeys(t *testing.T) {
	nested := []byte("[hooks]\nallow_managed_hooks_only = true\nallowed_sandbox_modes = [\"read-only\"]\n")
	if mandates(ProviderCodex, nested) {
		t.Error("keys nested under [hooks] are ignored by Codex and must not count as a mandate")
	}
	top := []byte("allowed_sandbox_modes = [\"read-only\"]\n\n[experimental_network]\nenabled = true\n")
	if !mandates(ProviderCodex, top) {
		t.Error("a top-level mandate key must be recognized")
	}
	if mandates(ProviderCodex, []byte("[hooks]\nPreToolUse = \"openbox hook codex PreToolUse\"\n")) {
		t.Error("naming our hook in requirements.toml is not a mandate")
	}
}

func TestApply_IdempotentAndBacksUp(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "managed-settings.json")
	plan := Plan{Files: []File{{Path: target, Contents: []byte("v2"), Mode: 0o644}}}

	out, err := Apply(plan, false, fixedClock())
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if out[0].Action != "written" {
		t.Errorf("first apply = %q, want written", out[0].Action)
	}

	out, err = Apply(plan, false, fixedClock())
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if out[0].Action != "unchanged" {
		t.Errorf("second apply = %q, want unchanged", out[0].Action)
	}

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

// TestApply_CommentedMarkerIsNotStrictness the property that matters most:
// never quietly downgrade a mandate. A strictness marker that survives only as
// a comment in the incoming file is not in effect, so replacing a live mandate
// with it is a downgrade and must be refused.
func TestApply_CommentedMarkerIsNotStrictness(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "requirements.toml")
	strict := "allow_managed_hooks_only = true\nallowed_sandbox_modes = [\"read-only\"]\n"
	if err := os.WriteFile(target, []byte(strict), 0o644); err != nil {
		t.Fatal(err)
	}
	commented := Plan{Files: []File{{
		Path:     target,
		Contents: []byte("# allow_managed_hooks_only = true\nallowed_sandbox_modes = [\"read-only\"]\n"),
		Mode:     0o644,
	}}}
	out, err := Apply(commented, false, fixedClock())
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if out[0].Action != "skipped" {
		t.Fatalf("action = %q, want skipped (the incoming marker is only a comment)", out[0].Action)
	}
	if body, _ := os.ReadFile(target); string(body) != strict {
		t.Error("the live mandate must be left in place")
	}
}

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

	out, err = Apply(lax, true, fixedClock())
	if err != nil {
		t.Fatalf("forced apply: %v", err)
	}
	if out[0].Action != "written" {
		t.Errorf("forced action = %q, want written", out[0].Action)
	}
}

// TestApply_AllowsEqualOrStricter replacing a file with an equally-strict or
// stricter one is not a downgrade and must proceed, or upgrades would be
// impossible.
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

// TestTemplates_AllProvidersRender every shipped template must render and be
// non-trivial: an embed that silently resolved to nothing would produce an
// empty managed file, i.e. No mandate at all.
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
