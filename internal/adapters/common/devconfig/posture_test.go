package devconfig

import (
	"encoding/json"
	"reflect"
	"regexp"
	"strings"
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

// TestPostureFields_CoverEveryConfigControl every governance control in
// DevConfig must be reported in the posture.
func TestPostureFields_CoverEveryConfigControl(t *testing.T) {
	// A control that cannot engage must not appear in the posture, or an org
	// reading `true` would believe a signature check was protecting it.
	notPosture := map[string]bool{
		"install_git_hook":        true,
		"require_verified_bundle": true,
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
			t.Errorf("DevConfig.%s (%q) is a boolean control but is not in postureFields() — "+
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
			"posture egresses on every session start — no field may be wired to a credential source (INV-1)",
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
	if p.Enforce != ResolveEnforce() ||
		p.FailClosed != ResolveFailClosed() ||
		p.Tier2 != ResolveTier2() ||
		p.SecretDetection != ResolveSecretDetection() ||
		p.ContentCapture != ResolveContentCapture() ||
		p.Findings != ResolveFindings() ||
		p.Finops != ResolveFinops() {
		t.Errorf("EffectivePosture drifted from the resolvers: %+v", p)
	}
	if p.Flags()["content_capture"] != p.ContentCapture {
		t.Error("Flags disagrees with the resolved posture — doctor would report a control that is not in force")
	}
	if _, reported := p.Flags()["require_verified_bundle"]; reported {
		t.Error("require_verified_bundle is still reported — it cannot engage, so reporting it overstates")
	}
	// That decision reversed the default deliberately, and the INV-3 property it
	// cited is preserved elsewhere and unchanged: a failure never blocks a tool
	// call, because fail_closed defaults off and the gate fails open on error.
	if !p.Enforce {
		t.Error("enforce must default ON ")
	}
	if p.FailClosed {
		t.Error("fail_closed must stay off — enforce-by-default is only defensible while an outage cannot block a developer")
	}
	if !p.SecretDetection || !p.ContentCapture {
		t.Errorf("secret_detection and content_capture default on, got %+v", p)
	}
}

// TestPostureReportsDecisionProvenance policy provenance replaced the bundle
// coordinates, and the replacement has to answer a question posture can
// actually answer at the moment it is built.
func TestPostureReportsDecisionProvenance(t *testing.T) {
	t.Run("default is control plane, fail-open", func(t *testing.T) {
		isolateConfig(t)
		p := EffectivePosture()
		if p.DecisionAuthority != DecisionAuthorityControlPlane {
			t.Errorf("decision authority = %q, want %q", p.DecisionAuthority, DecisionAuthorityControlPlane)
		}
		if p.FailurePolicy != FailurePolicyFailOpen {
			t.Errorf("failure policy = %q, want %q — the default must not overstate", p.FailurePolicy, FailurePolicyFailOpen)
		}
	})

	t.Run("fail_closed is reported", func(t *testing.T) {
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
			t.Errorf("decision_authority = %v, want %q — that decision makes this posture's policy-provenance evidence",
				m["decision_authority"], DecisionAuthorityControlPlane)
		}
		if m["failure_policy"] != FailurePolicyFailOpen {
			t.Errorf("failure_policy = %v, want %q", m["failure_policy"], FailurePolicyFailOpen)
		}
		for k := range m {
			if strings.HasPrefix(k, "bundle_") || k == "staleness" {
				t.Errorf("%q is still emitted — it reports a subsystem that decision deleted", k)
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
