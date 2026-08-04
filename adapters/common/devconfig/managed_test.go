package devconfig

import (
	"os"
	"path/filepath"
	"testing"
)

func writeJSON(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The point of the managed layer: a locked field beats the environment. If it
// only beat the user config, OPENBOX_ENFORCE=0 would still disable the gate and
// the mandate would be theater (report SL-01).
func TestManaged_LockedFieldBeatsEnvAndUser(t *testing.T) {
	dir := t.TempDir()
	managedPath := filepath.Join(dir, "managed.json")
	userPath := filepath.Join(dir, "dev.json")
	writeJSON(t, managedPath, `{"enforce":true,"locked":["enforce"]}`)
	writeJSON(t, userPath, `{"enforce":false}`)
	t.Setenv(EnvManagedConfig, managedPath)
	t.Setenv(EnvConfigPath, userPath)
	t.Setenv(EnvEnforce, "0") // the developer's escape hatch

	got, src := resolveBoolWithSource("enforce",
		func(c DevConfig) *bool { b := c.Enforce; return &b }, false, EnvEnforce)
	if !got {
		t.Error("a locked managed field must override both the user config and the env")
	}
	if src != SourceManaged {
		t.Errorf("source = %q, want managed", src)
	}
}

// A field the org sets but does not lock is a default, not a mandate — which is
// what gives orgs a "we recommend" separate from "we require".
func TestManaged_UnlockedFieldIsOnlyADefault(t *testing.T) {
	dir := t.TempDir()
	managedPath := filepath.Join(dir, "managed.json")
	writeJSON(t, managedPath, `{"content_capture":false}`)
	t.Setenv(EnvManagedConfig, managedPath)
	t.Setenv(EnvConfigPath, filepath.Join(dir, "absent.json"))

	// With nothing else set, the org default applies.
	got, src := resolveBoolWithSource("content_capture",
		func(c DevConfig) *bool { return c.ContentCapture }, true, EnvContentCapture)
	if got {
		t.Error("the org default should apply when the developer sets nothing")
	}
	if src != SourceManagedDefault {
		t.Errorf("source = %q, want managed_default", src)
	}

	// And the developer can still override it.
	t.Setenv(EnvContentCapture, "1")
	got, src = resolveBoolWithSource("content_capture",
		func(c DevConfig) *bool { return c.ContentCapture }, true, EnvContentCapture)
	if !got || src != SourceEnv {
		t.Errorf("an unlocked org default must remain overridable, got (%v, %q)", got, src)
	}
}

// A field absent from the managed file falls through to today's behaviour
// unchanged, so deploying a managed file that governs one setting does not
// silently take over the others.
func TestManaged_AbsentFieldFallsThrough(t *testing.T) {
	dir := t.TempDir()
	managedPath := filepath.Join(dir, "managed.json")
	userPath := filepath.Join(dir, "dev.json")
	writeJSON(t, managedPath, `{"enforce":true,"locked":["enforce"]}`)
	writeJSON(t, userPath, `{"tier2":true}`)
	t.Setenv(EnvManagedConfig, managedPath)
	t.Setenv(EnvConfigPath, userPath)

	got, src := resolveBoolWithSource("tier2",
		func(c DevConfig) *bool { return c.Tier2 }, false, EnvTier2)
	if !got || src != SourceUser {
		t.Errorf("tier2 = (%v, %q), want (true, user)", got, src)
	}
}

// With no managed file at all, behaviour is byte-identical to before the story.
func TestManaged_NoManagedFileIsUnchangedBehaviour(t *testing.T) {
	dir := t.TempDir()
	userPath := filepath.Join(dir, "dev.json")
	writeJSON(t, userPath, `{"enforce":true}`)
	t.Setenv(EnvManagedConfig, filepath.Join(dir, "nope.json"))
	t.Setenv(EnvConfigPath, userPath)

	got, src := resolveBoolWithSource("enforce",
		func(c DevConfig) *bool { b := c.Enforce; return &b }, false, EnvEnforce)
	if !got || src != SourceUser {
		t.Errorf("enforce = (%v, %q), want (true, user)", got, src)
	}
	if st := Managed(); st.Present {
		t.Error("Managed().Present should be false with no managed file")
	}
}

// A malformed managed file must not take the machine down: a hook that cannot
// resolve config cannot observe either, so the org would lose the very evidence
// the mandate exists to produce. It degrades to unmanaged and reports
// present-but-unreadable so an operator can see the machine is misconfigured.
func TestManaged_MalformedFileDegradesButIsReported(t *testing.T) {
	dir := t.TempDir()
	managedPath := filepath.Join(dir, "managed.json")
	writeJSON(t, managedPath, `{"enforce": not-json`)
	t.Setenv(EnvManagedConfig, managedPath)
	t.Setenv(EnvConfigPath, filepath.Join(dir, "absent.json"))

	got, src := resolveBoolWithSource("enforce",
		func(c DevConfig) *bool { b := c.Enforce; return &b }, false, EnvEnforce)
	if got || src != SourceDefault {
		t.Errorf("a malformed managed file must not fabricate a value, got (%v, %q)", got, src)
	}
	st := Managed()
	if !st.Present || st.Readable {
		t.Errorf("status = %+v, want present but not readable", st)
	}
}

// An unknown key is rejected rather than ignored: a typo in a mandate
// ("enfoce": true) would otherwise read as a file that governs nothing.
// OD-RF-2 reversed this. An unknown key used to make the whole managed file
// unreadable, which dropped EVERY mandate — enforce included — and left the
// machine developer-controlled, reported only in `doctor`. A typo in one field
// should not un-govern a machine, and where the file is group-writable that
// downgrade is inducible by appending junk.
//
// The key is now ignored and reported instead. The mandates it sits beside
// still apply.
func TestManaged_UnknownKeyIsReportedNotFatal(t *testing.T) {
	dir := t.TempDir()
	managedPath := filepath.Join(dir, "managed.json")
	writeJSON(t, managedPath, `{"enfoce":true,"enforce":true,"locked":["enforce"]}`)
	t.Setenv(EnvManagedConfig, managedPath)
	t.Setenv(EnvConfigPath, filepath.Join(dir, "absent.json"))

	st := Managed()
	if !st.Present || !st.Readable {
		t.Fatalf("an unknown key must not invalidate the file, got %+v", st)
	}
	if len(st.UnknownKeys) != 1 || st.UnknownKeys[0] != "enfoce" {
		t.Errorf("UnknownKeys = %v, want [enfoce]", st.UnknownKeys)
	}
	// The mandate beside the typo still governs.
	if !ResolveEnforce() {
		t.Error("the locked enforce mandate was dropped because of an unrelated typo")
	}
}

// Structural damage is still fatal: if the file is not JSON at all, nothing
// about the mandate can be trusted.
func TestManaged_MalformedJSONIsStillUnreadable(t *testing.T) {
	dir := t.TempDir()
	managedPath := filepath.Join(dir, "managed.json")
	writeJSON(t, managedPath, `{ this is not json`)
	t.Setenv(EnvManagedConfig, managedPath)
	t.Setenv(EnvConfigPath, filepath.Join(dir, "absent.json"))

	if st := Managed(); !st.Present || st.Readable {
		t.Errorf("unparseable JSON must still read as unreadable, got %+v", st)
	}
}

// The posture's provenance map must cover every flag it reports, or the control
// plane cannot tell a mandate from a coincidence.
func TestEffectivePosture_ReportsSourceForEveryFlag(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvManagedConfig, filepath.Join(dir, "nope.json"))
	t.Setenv(EnvConfigPath, filepath.Join(dir, "nope-dev.json"))

	p := EffectivePosture()
	m := p.Metadata()
	src, ok := m["config_source"].(map[string]any)
	if !ok {
		t.Fatalf("config_source missing from posture metadata: %v", m)
	}
	for _, flag := range []string{
		"enforce", "fail_closed", "tier2", "secret_detection",
		"content_capture", "findings", "finops",
	} {
		if _, present := m[flag]; !present {
			t.Errorf("posture is missing flag %q", flag)
		}
		if _, present := src[flag]; !present {
			t.Errorf("config_source is missing provenance for %q", flag)
		}
	}
}

// The shipped template must actually load — an ops file that documents itself
// with "//" keys would be rejected by the strict decode if the exemption broke,
// and the failure mode (silently unmanaged) is the one this story exists to
// prevent.
func TestManaged_ShippedTemplateLoadsAndLocks(t *testing.T) {
	const template = "../../../deploy/managed/openbox/dev.json"
	if _, err := os.Stat(template); err != nil {
		t.Skipf("template not present: %v", err)
	}
	t.Setenv(EnvManagedConfig, template)
	t.Setenv(EnvConfigPath, filepath.Join(t.TempDir(), "absent.json"))

	st := Managed()
	if !st.Present || !st.Readable {
		t.Fatalf("shipped managed template must load, got %+v", st)
	}
	// enforce is the mandate the template exists to assert, and it must beat an
	// env override.
	t.Setenv(EnvEnforce, "0")
	got, src := resolveBoolWithSource("enforce",
		func(c DevConfig) *bool { b := c.Enforce; return &b }, false, EnvEnforce)
	if !got || src != SourceManaged {
		t.Errorf("template enforce = (%v, %q), want (true, managed)", got, src)
	}
	// content_capture is deliberately an unlocked org default (OD-E8-1), so a
	// team can still opt back in.
	t.Setenv(EnvContentCapture, "1")
	got, src = resolveBoolWithSource("content_capture",
		func(c DevConfig) *bool { return c.ContentCapture }, true, EnvContentCapture)
	if !got || src != SourceEnv {
		t.Errorf("template content_capture must stay overridable, got (%v, %q)", got, src)
	}
}

// A "//" documentation key must not be mistaken for the field it documents.
func TestManaged_DocKeyIsNotASetting(t *testing.T) {
	dir := t.TempDir()
	managedPath := filepath.Join(dir, "managed.json")
	writeJSON(t, managedPath, `{"//enforce":"we require this","locked":["enforce"]}`)
	t.Setenv(EnvManagedConfig, managedPath)
	t.Setenv(EnvConfigPath, filepath.Join(dir, "absent.json"))

	got, src := resolveBoolWithSource("enforce",
		func(c DevConfig) *bool { b := c.Enforce; return &b }, false, EnvEnforce)
	if got || src != SourceDefault {
		t.Errorf("a comment about enforce must not enforce anything, got (%v, %q)", got, src)
	}
}
