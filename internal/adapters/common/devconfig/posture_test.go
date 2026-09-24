package devconfig

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
)

// TestPostureMetadata_BooleansAlwaysPresent the posture booleans must always
// be present. The list is the field table itself, not a copy of it: a control
// that is resolved but never reported is exactly how require_verified_bundle
// stayed invisible to the orgs that had turned it on.
func TestPostureMetadata_BooleansAlwaysPresent(t *testing.T) {
	m := Posture{}.Metadata()
	for _, f := range postureFields() {
		v, ok := m[f.name]
		if !ok {
			t.Errorf("%s missing from posture metadata", f.name)
			continue
		}
		if _, isBool := v.(bool); !isBool {
			t.Errorf("%s should be a bool, got %T", f.name, v)
		}
	}
}

// TestPostureMetadataOmitsTheInertTier2Key tier2 is parsed but deliberately
// not honoured (docs/upgrading-to-inline-evaluation.md:67: "there are no
// tiers; every gated call is evaluated. Deliberately not honoured"), so
// publishing it beside the flags that ARE honoured lets a control-plane
// reader mistake a dead key for live governance. It must not appear in
// Metadata() or config_source, even when the config sets it explicitly.
func TestPostureMetadataOmitsTheInertTier2Key(t *testing.T) {
	isolateConfig(t)
	cfgPath := filepath.Join(t.TempDir(), "dev.json")
	if err := os.WriteFile(cfgPath, []byte(`{"tier2":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvConfigPath, cfgPath)

	m := EffectivePosture().Metadata()
	if _, present := m["tier2"]; present {
		t.Errorf("tier2 present in posture metadata with DevConfig.Tier2=true; "+
			"docs/upgrading-to-inline-evaluation.md:67 says it is deliberately not honoured, "+
			"so it must not be published beside the flags that are: %v", m)
	}
	if src, ok := m["config_source"].(map[string]any); ok {
		if _, present := src["tier2"]; present {
			t.Errorf("config_source.tier2 present with DevConfig.Tier2=true; "+
				"docs/upgrading-to-inline-evaluation.md:67 says tier2 is deliberately not honoured: %v", src)
		}
	}
}

// TestPostureFields_CoverEveryConfigControl every governance control in
// DevConfig must be reported in the posture.
func TestPostureFields_CoverEveryConfigControl(t *testing.T) {
	// A control that cannot engage must not appear in the posture, or an org
	// reading `true` would believe a signature check was protecting it.
	notPosture := map[string]bool{
		"install_git_hook":        true,
		"require_verified_bundle": true,
		// tier2 is deprecated and deliberately not honoured
		// (docs/upgrading-to-inline-evaluation.md:67); reporting it would let a
		// control-plane reader mistake a dead key for live governance.
		"tier2": true,
	}

	reported := map[string]bool{}
	for _, f := range postureFields() {
		reported[f.name] = true
	}
	typ := reflect.TypeOf(DevConfig{})
	for i := 0; i < typ.NumField(); i++ {
		fld := typ.Field(i)
		if fld.Type.Kind() != reflect.Bool &&
			!(fld.Type.Kind() == reflect.Ptr && fld.Type.Elem().Kind() == reflect.Bool) {
			continue
		}
		name := strings.Split(fld.Tag.Get("json"), ",")[0]
		if name == "" || notPosture[name] {
			continue
		}
		if !reported[name] {
			t.Errorf("DevConfig.%s (%q) is a boolean control but is not in postureFields(); "+
				"add it so `openbox doctor` and the session posture can report it, "+
				"or add it to notPosture here with the reason", fld.Name, name)
		}
	}
}

// TestPostureMetadata_UnknownStringsOmitted strings are omitted when unknown,
// so a field the adapter could not determine reads as absent rather than as a
// false claim.
func TestPostureMetadata_UnknownStringsOmitted(t *testing.T) {
	m := Posture{}.Metadata()
	for _, k := range []string{
		"adapter", "adapter_version", "provider_version",
		"decision_authority", "failure_policy",
	} {
		if _, present := m[k]; present {
			t.Errorf("%s should be omitted when empty, got %v", k, m[k])
		}
	}

	full := Posture{
		Adapter: "codex/1", AdapterVersion: "codex/1", ProviderVersion: "codex-cli 0.145.0",
	}.Metadata()
	if full["provider_version"] != "codex-cli 0.145.0" {
		t.Errorf("provider_version = %v", full["provider_version"])
	}
}

// TestPostureMetadata_NoSecretShapedValues iNV-1: posture egresses on every
// session start, so it must never carry a credential.
func TestPostureMetadata_NoSecretShapedValues(t *testing.T) {
	p := Posture{
		Enforce:         true,
		Adapter:         "obx_live_deadbeefdeadbeefdeadbeef",
		AdapterVersion:  "obx_key_0123456789abcdef",
		ProviderVersion: "-----BEGIN PRIVATE KEY-----",
		ProviderManaged: "ghp_0123456789abcdefghij",
	}
	raw, err := json.Marshal(p.Metadata())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	probe := regexp.MustCompile(`obx_live_|obx_key_|BEGIN [A-Z ]*PRIVATE KEY|sk-proj-|ghp_`)
	if got := probe.FindString(string(raw)); got != "" {
		t.Errorf("secret-shaped value %q reached the posture metadata: %s\n"+
			"posture egresses on every session start; no field may be wired to a credential source (INV-1)",
			got, raw)
	}
}

// TestPostureMetadata_ValuesBounded values are bounded: provider_version comes
// from an external binary's stdout, so the object stays bounded at the
// untrusted boundary.
func TestPostureMetadata_ValuesBounded(t *testing.T) {
	long := strings.Repeat("x", maxPostureValueLen*3)
	m := Posture{ProviderVersion: long, Adapter: long}.Metadata()
	for _, k := range []string{"provider_version", "adapter"} {
		if got := m[k].(string); len(got) > maxPostureValueLen {
			t.Errorf("%s not bounded: len %d > %d", k, len(got), maxPostureValueLen)
		}
	}
}

// TestEffectivePosture_MatchesResolvers effectivePosture must read through the
// same resolvers the runtime uses, so the record cannot drift from the
// behaviour it describes.
func TestEffectivePosture_MatchesResolvers(t *testing.T) {
	t.Setenv(EnvConfigPath, "/nonexistent/dev.json") // defaults only
	p := EffectivePosture()
	// FailClosed excluded from the drift check: delivery is always
	// fail-closed now (HaltOnDeliveryFailure), so the posture no longer
	// tracks ResolveFailClosed's own (now-ignored) resolved value -- see the
	// dedicated assertion below.
	if p.Enforce != ResolveEnforce() ||
		p.SecretDetection != ResolveSecretDetection() ||
		p.ContentCapture != ResolveContentCapture() ||
		p.Findings != ResolveFindings() ||
		p.Finops != ResolveFinops() {
		t.Errorf("EffectivePosture drifted from the resolvers: %+v", p)
	}
	if p.Flags()["content_capture"] != p.ContentCapture {
		t.Error("Flags disagrees with the resolved posture; doctor would report a control that is not in force")
	}
	if _, reported := p.Flags()["require_verified_bundle"]; reported {
		t.Error("require_verified_bundle is still reported; it cannot engage, so reporting it overstates")
	}
	if !p.Enforce {
		t.Error("enforce must default ON ")
	}
	// Delivery is always fail-closed: an event core does not accept halts the
	// run regardless of what the (now-ignored, deprecated) fail_closed key
	// says.
	if !p.FailClosed {
		t.Error("fail_closed must always be true; delivery is always fail-closed now")
	}
	if !p.SecretDetection || !p.ContentCapture {
		t.Errorf("secret_detection and content_capture default on, got %+v", p)
	}
}

// TestPostureReportsDecisionProvenance policy provenance replaced the bundle
// coordinates, and the replacement has to answer a question posture can
// actually answer at the moment it is built.
func TestPostureReportsDecisionProvenance(t *testing.T) {
	t.Run("default is control plane, always fail-closed", func(t *testing.T) {
		isolateConfig(t)
		p := EffectivePosture()
		if p.DecisionAuthority != DecisionAuthorityControlPlane {
			t.Errorf("decision authority = %q, want %q", p.DecisionAuthority, DecisionAuthorityControlPlane)
		}
		// Delivery is always fail-closed now: an unaccepted event halts the
		// run regardless of the (deprecated, ignored) fail_closed key.
		if p.FailurePolicy != FailurePolicyFailClosed {
			t.Errorf("failure policy = %q, want %q; delivery is always fail-closed", p.FailurePolicy, FailurePolicyFailClosed)
		}
	})

	t.Run("fail_closed set is still reported fail-closed (the key is ignored, not honoured)", func(t *testing.T) {
		isolateConfig(t)
		t.Setenv(EnvFailClosed, "1")
		if p := EffectivePosture(); p.FailurePolicy != FailurePolicyFailClosed {
			t.Errorf("failure policy = %q, want %q", p.FailurePolicy, FailurePolicyFailClosed)
		}
	})

	// Build through EffectivePosture, not Posture{}: an empty string is dropped
	// by the unknown-value guard, so a zero value would pass vacuously.
	t.Run("both reach the emitted metadata, and no bundle key does", func(t *testing.T) {
		isolateConfig(t)
		m := EffectivePosture().Metadata()
		if m["decision_authority"] != DecisionAuthorityControlPlane {
			t.Errorf("decision_authority = %v, want %q; that decision makes this posture's policy-provenance evidence",
				m["decision_authority"], DecisionAuthorityControlPlane)
		}
		if m["failure_policy"] != FailurePolicyFailClosed {
			t.Errorf("failure_policy = %v, want %q; delivery is always fail-closed", m["failure_policy"], FailurePolicyFailClosed)
		}
		for k := range m {
			if strings.HasPrefix(k, "bundle_") || k == "staleness" {
				t.Errorf("%q is still emitted; it reports a subsystem that decision deleted", k)
			}
		}
	})

	t.Run("failure_policy tracks fail_closed onto the wire", func(t *testing.T) {
		isolateConfig(t)
		t.Setenv(EnvFailClosed, "1")
		if m := EffectivePosture().Metadata(); m["failure_policy"] != FailurePolicyFailClosed {
			t.Errorf("failure_policy = %v, want %q", m["failure_policy"], FailurePolicyFailClosed)
		}
	})

	t.Run("no verification vocabulary on the new fields", func(t *testing.T) {
		isolateConfig(t)
		p := EffectivePosture()
		for _, v := range []string{p.DecisionAuthority, p.FailurePolicy} {
			for _, banned := range []string{"verif", "integrity", "signed"} {
				if strings.Contains(strings.ToLower(v), banned) {
					t.Errorf("%q implies a cryptographic check that no longer happens", v)
				}
			}
		}
	})
}

// TestDeprecatedKeysAreDetectedWherePostureIsRead a deprecation notice that
// cannot fire is the same as no notice.
func TestDeprecatedKeysAreDetectedWherePostureIsRead(t *testing.T) {
	t.Run("silent when nothing deprecated is set", func(t *testing.T) {
		isolateConfig(t)
		if got := deadKeysPresent(); len(got) != 0 {
			t.Errorf("clean config reported deprecated keys: %v", got)
		}
	})

	for _, tc := range []struct{ name, env, val, want string }{
		{"tier2 explicitly off", EnvTier2, "0", "`tier2`"},
		{"tier2 on", EnvTier2, "1", "`tier2`"},
		{"tier2_timeout_ms", EnvTier2Timeout, "500", "`tier2_timeout_ms`"},
		{"require_verified_bundle", EnvRequireVerified, "1", "`require_verified_bundle`"},
		{"fail_closed on", EnvFailClosed, "1", "`fail_closed`"},
		{"fail_closed explicitly off", EnvFailClosed, "0", "`fail_closed`"},
		{"enforce on", EnvEnforce, "1", "`enforce`"},
		{"enforce explicitly off", EnvEnforce, "0", "`enforce`"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateConfig(t)
			t.Setenv(tc.env, tc.val)
			got := deadKeysPresent()
			if len(got) != 1 || got[0] != tc.want {
				t.Errorf("deadKeysPresent() = %v, want [%s]", got, tc.want)
			}
		})
	}
}

// TestTier2DeprecationWarningFiresExactlyOnce tier2 no longer appears in the
// posture (TestPostureMetadataOmitsTheInertTier2Key), so its one-shot stderr
// warning is now the only remaining surface telling a developer the key is
// dead (CLAUDE.md: "the key stays parseable so it can warn"). It must still
// fire, and deprecationOnce must still cap it at one print per process even
// across repeated resolution.
func TestTier2DeprecationWarningFiresExactlyOnce(t *testing.T) {
	isolateConfig(t)
	t.Setenv(EnvTier2, "1")

	// deprecationOnce is a package-level sync.Once shared by every test in this
	// binary; reset it so this test's result cannot depend on whether some
	// other test happened to resolve a dead key first.
	deprecationOnce = sync.Once{}
	t.Cleanup(func() { deprecationOnce = sync.Once{} })

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	orig := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = orig }() // restore even if a call below fails fatally

	EffectivePosture()
	EffectivePosture() // repeat resolution must not warn a second time

	os.Stderr = orig
	if err := w.Close(); err != nil {
		t.Fatalf("close pipe: %v", err)
	}
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(r); err != nil {
		t.Fatalf("read pipe: %v", err)
	}

	out := buf.String()
	// Count the warning, not the key name: one line names every dead key that
	// is set, so `tier2` appears twice in it the moment tier2_timeout_ms is
	// also present -- which any machine still carrying a legacy env var has.
	if got := strings.Count(out, "set but ignored"); got != 1 {
		t.Errorf("deprecation warning printed to stderr %d times, want exactly 1: %q", got, out)
	}
	if !strings.Contains(out, "`tier2`") {
		t.Errorf("stderr = %q, want the tier2 deprecation warning", out)
	}
}
