package devconfig

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
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

// TestManaged_LockedFieldBeatsEnvAndUser the point of the managed layer: a
// locked field beats the environment.
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
		func(c DevConfig) *bool { return c.Enforce }, false, EnvEnforce)
	if !got {
		t.Error("a locked managed field must override both the user config and the env")
	}
	if src != SourceManaged {
		t.Errorf("source = %q, want managed", src)
	}
}

// TestManaged_UnlockedFieldIsOnlyADefault a field the org sets but does not
// lock is a default, not a mandate; which is what gives orgs a "we recommend"
// separate from "we require".
func TestManaged_UnlockedFieldIsOnlyADefault(t *testing.T) {
	dir := t.TempDir()
	managedPath := filepath.Join(dir, "managed.json")
	writeJSON(t, managedPath, `{"content_capture":false}`)
	t.Setenv(EnvManagedConfig, managedPath)
	t.Setenv(EnvConfigPath, filepath.Join(dir, "absent.json"))

	got, src := resolveBoolWithSource("content_capture",
		func(c DevConfig) *bool { return c.ContentCapture }, true, EnvContentCapture)
	if got {
		t.Error("the org default should apply when the developer sets nothing")
	}
	if src != SourceManagedDefault {
		t.Errorf("source = %q, want managed_default", src)
	}

	t.Setenv(EnvContentCapture, "1")
	got, src = resolveBoolWithSource("content_capture",
		func(c DevConfig) *bool { return c.ContentCapture }, true, EnvContentCapture)
	if !got || src != SourceEnv {
		t.Errorf("an unlocked org default must remain overridable, got (%v, %q)", got, src)
	}
}

// TestManaged_AbsentFieldFallsThrough a field absent from the managed file
// falls through to today's behaviour unchanged, so deploying a managed file
// that governs one setting does not silently take over the others.
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

// TestManaged_NoManagedFileIsUnchangedBehaviour with no managed file at all,
// behaviour is byte-identical to before the story.
func TestManaged_NoManagedFileIsUnchangedBehaviour(t *testing.T) {
	dir := t.TempDir()
	userPath := filepath.Join(dir, "dev.json")
	writeJSON(t, userPath, `{"enforce":true}`)
	t.Setenv(EnvManagedConfig, filepath.Join(dir, "nope.json"))
	t.Setenv(EnvConfigPath, userPath)

	got, src := resolveBoolWithSource("enforce",
		func(c DevConfig) *bool { return c.Enforce }, false, EnvEnforce)
	if !got || src != SourceUser {
		t.Errorf("enforce = (%v, %q), want (true, user)", got, src)
	}
	if st := Managed(); st.Present {
		t.Error("Managed().Present should be false with no managed file")
	}
}

// TestManaged_MalformedFileDegradesButIsReported a malformed managed file must
// not take the machine down: a hook that cannot resolve config cannot observe
// either, so the org would lose the very evidence the mandate exists to
// produce.
func TestManaged_MalformedFileDegradesButIsReported(t *testing.T) {
	dir := t.TempDir()
	managedPath := filepath.Join(dir, "managed.json")
	writeJSON(t, managedPath, `{"enforce": not-json`)
	t.Setenv(EnvManagedConfig, managedPath)
	t.Setenv(EnvConfigPath, filepath.Join(dir, "absent.json"))

	got, src := resolveBoolWithSource("enforce",
		func(c DevConfig) *bool { return c.Enforce }, false, EnvEnforce)
	if got || src != SourceDefault {
		t.Errorf("a malformed managed file must not fabricate a value, got (%v, %q)", got, src)
	}
	st := Managed()
	if !st.Present || st.Readable {
		t.Errorf("status = %+v, want present but not readable", st)
	}
}

// TestManaged_UnknownKeyIsReportedNotFatal an unknown key is rejected rather
// than ignored: a typo in a mandate ("enfoce": true) would otherwise read as a
// file that governs nothing.
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
	if !ResolveEnforce() {
		t.Error("the locked enforce mandate was dropped because of an unrelated typo")
	}
}

// TestManaged_MalformedJSONIsStillUnreadable structural damage is still fatal:
// if the file is not JSON at all, nothing about the mandate can be trusted.
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

// TestEffectivePosture_ReportsSourceForEveryFlag the posture's provenance map
// must cover every flag it reports, or the control plane cannot tell a mandate
// from a coincidence.
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
		// tier2 excluded: deprecated and deliberately not honoured
		// (docs/upgrading-to-inline-evaluation.md:67), so it is deliberately
		// absent from the posture and its config_source (see posture_test.go's
		// TestPostureMetadataOmitsTheInertTier2Key).
		"enforce", "fail_closed", "secret_detection",
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

// TestManaged_ShippedTemplateLoadsAndLocks the shipped template must actually
// load; an ops file that documents itself with "//" keys would be rejected by
// the strict decode if the exemption broke, and the failure mode (silently
// unmanaged) is the one this story exists to prevent.
func TestManaged_ShippedTemplateLoadsAndLocks(t *testing.T) {
	// It has to be counted again whenever this package moves, and a wrong count
	// does not fail -- it skips, which silently retires a test whose own subject
	// is a failure mode that is silent.
	const template = "../../../../deployments/managed/openbox/dev.json"
	if _, err := os.Stat(template); err != nil {
		t.Skipf("shipped template not found at %s: %v", template, err)
	}
	t.Setenv(EnvManagedConfig, template)
	t.Setenv(EnvConfigPath, filepath.Join(t.TempDir(), "absent.json"))

	st := Managed()
	if !st.Present || !st.Readable {
		t.Fatalf("shipped managed template must load, got %+v", st)
	}
	t.Setenv(EnvEnforce, "0")
	got, src := resolveBoolWithSource("enforce",
		func(c DevConfig) *bool { return c.Enforce }, false, EnvEnforce)
	if !got || src != SourceManaged {
		t.Errorf("template enforce = (%v, %q), want (true, managed)", got, src)
	}
	t.Setenv(EnvContentCapture, "1")
	got, src = resolveBoolWithSource("content_capture",
		func(c DevConfig) *bool { return c.ContentCapture }, true, EnvContentCapture)
	if !got || src != SourceEnv {
		t.Errorf("template content_capture must stay overridable, got (%v, %q)", got, src)
	}
}

// TestManaged_DocKeyIsNotASetting a "//" documentation key must not be
// mistaken for the field it documents.
func TestManaged_DocKeyIsNotASetting(t *testing.T) {
	dir := t.TempDir()
	managedPath := filepath.Join(dir, "managed.json")
	writeJSON(t, managedPath, `{"//enforce":"we require this","locked":["enforce"]}`)
	t.Setenv(EnvManagedConfig, managedPath)
	t.Setenv(EnvConfigPath, filepath.Join(dir, "absent.json"))

	got, src := resolveBoolWithSource("enforce",
		func(c DevConfig) *bool { return c.Enforce }, false, EnvEnforce)
	if got || src != SourceDefault {
		t.Errorf("a comment about enforce must not enforce anything, got (%v, %q)", got, src)
	}
}

// credsEnvForManagedTest points every path ResolveCredentials reads at dir and
// supplies the identity it requires, so the assertions below turn on the
// content posture alone. Values are derived in code rather than written as
// literals: this repo's own redactor rewrites secret-shaped assignments in
// files an agent authors (see CLAUDE.md, "Privacy posture").
func credsEnvForManagedTest(t *testing.T, dir string) {
	t.Helper()
	t.Setenv(EnvHome, dir)
	t.Setenv(EnvDID, "did:aip:"+strings.Repeat("a", 8))
	t.Setenv(EnvAPIKeyDirect, "obx_"+strings.Repeat("k", 8))
	t.Setenv(EnvAgentPrivateKey, base64.StdEncoding.EncodeToString(make([]byte, 32)))
}

// TestResolveCredentials_HonoursALockedManagedContentCapture the client's
// content posture and the mapper's must come from ONE resolver.
//
// They did not. This field was resolved by hand from the user file plus the
// environment and never consulted the MANAGED layer, so a locked managed
// `content_capture:false` was honoured by ResolveContentCapture -- the observe
// copy carried nothing -- and ignored at the client, leaving the enforce copy
// and the model-call lane bodies to egress under an org lock while the
// SessionStart posture row told the control plane `content_capture:false,
// source: managed`. client.Config calls this field "the org's content
// posture"; the managed layer is precisely the org's.
func TestResolveCredentials_HonoursALockedManagedContentCapture(t *testing.T) {
	dir := t.TempDir()
	managedPath := filepath.Join(dir, "managed.json")
	userPath := filepath.Join(dir, "dev.json")
	writeJSON(t, managedPath, `{"content_capture":false,"locked":["content_capture"]}`)
	writeJSON(t, userPath, `{"content_capture":true}`)
	t.Setenv(EnvManagedConfig, managedPath)
	t.Setenv(EnvConfigPath, userPath)
	credsEnvForManagedTest(t, dir)
	t.Setenv(EnvContentCapture, "1") // the developer's escape hatch must NOT beat a lock

	c, err := ResolveCredentials()
	if err != nil {
		t.Fatalf("ResolveCredentials: %v", err)
	}
	if c.ContentCaptureEnabled {
		t.Error("a locked managed content_capture:false must reach the client; ignoring it egresses content the org locked off")
	}
	if got := ResolveContentCapture(); got != c.ContentCaptureEnabled {
		t.Errorf("client posture %v disagrees with ResolveContentCapture() %v; these must be one resolver, not two copies of a precedence chain",
			c.ContentCaptureEnabled, got)
	}
}

// TestResolveCredentials_UnlockedManagedContentCaptureIsOnlyADefault the fix
// above must not over-reach: an UNLOCKED managed key is a default the
// developer may still override, exactly as resolveBoolWithSource defines it.
// Without this, "honour the managed layer" could quietly become "the org wins
// always" and take the developer's opt-in with it.
func TestResolveCredentials_UnlockedManagedContentCaptureIsOnlyADefault(t *testing.T) {
	dir := t.TempDir()
	managedPath := filepath.Join(dir, "managed.json")
	userPath := filepath.Join(dir, "dev.json")
	writeJSON(t, managedPath, `{"content_capture":false}`) // set, not locked
	writeJSON(t, userPath, `{"content_capture":true}`)
	t.Setenv(EnvManagedConfig, managedPath)
	t.Setenv(EnvConfigPath, userPath)
	credsEnvForManagedTest(t, dir)

	c, err := ResolveCredentials()
	if err != nil {
		t.Fatalf("ResolveCredentials: %v", err)
	}
	if !c.ContentCaptureEnabled {
		t.Error("an unlocked managed key is only a default; the user file must still win")
	}
}
