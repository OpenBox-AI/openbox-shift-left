package devconfig

import (
	"encoding/json"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// The posture booleans must always be present. An absent flag is ambiguous
// between "off" and "this client is too old to report it", and telling those
// apart is the entire point of recording posture.
// The list is the field table itself, not a copy of it: a control that is
// resolved but never reported is exactly how require_verified_bundle stayed
// invisible to the orgs that had turned it on.
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

// Every governance control in DevConfig must be reported in the posture.
//
// TestPostureMetadata_BooleansAlwaysPresent checks the field table against
// ITSELF, so it cannot catch the failure that actually happened twice: a
// control added to DevConfig with a resolver, and never added to the table.
// This walks the config struct instead, so the next boolean control forces a
// decision — report it, or name it here as deliberately not a posture control.
func TestPostureFields_CoverEveryConfigControl(t *testing.T) {
	// install_git_hook is local convenience (whether we wrote a hook into this
	// repo's .git), not a governance posture an org would attest to.
	//
	// require_verified_bundle is parsed for back-compat and reports nothing: it
	// guarded a local policy bundle, and ADR-0017 deleted the bundle. A control
	// that cannot engage must not appear in the posture, or an org reading
	// `true` would believe a signature check was protecting it.
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

// Strings are omitted when unknown, so a field the adapter could not determine
// reads as absent rather than as a false claim.
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

// INV-1: posture egresses on every session start, so it must never carry a
// credential. The type only has room for booleans, opaque ids and enums, and
// this asserts that stays true even if someone stuffs a secret-shaped value
// into an adapter-supplied field.
func TestPostureMetadata_NoSecretShapedValues(t *testing.T) {
	// Deliberately hostile input in every adapter-supplied string.
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

	// The primary guarantee is structural — no code path assigns a credential
	// to these fields — but Metadata also drops secret-shaped values outright,
	// so a future mis-wiring degrades to a missing field instead of a leak.
	probe := regexp.MustCompile(`obx_live_|obx_key_|BEGIN [A-Z ]*PRIVATE KEY|sk-proj-|ghp_`)
	if got := probe.FindString(string(raw)); got != "" {
		t.Errorf("secret-shaped value %q reached the posture metadata: %s\n"+
			"posture egresses on every session start — no field may be wired to a credential source (INV-1)",
			got, raw)
	}
}

// Values are bounded: provider_version comes from an external binary's stdout,
// so the object stays bounded at the untrusted boundary.
func TestPostureMetadata_ValuesBounded(t *testing.T) {
	long := strings.Repeat("x", maxPostureValueLen*3)
	m := Posture{ProviderVersion: long, Adapter: long}.Metadata()
	for _, k := range []string{"provider_version", "adapter"} {
		if got := m[k].(string); len(got) > maxPostureValueLen {
			t.Errorf("%s not bounded: len %d > %d", k, len(got), maxPostureValueLen)
		}
	}
}

// EffectivePosture must read through the same resolvers the runtime uses, so
// the record cannot drift from the behaviour it describes.
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
	// Flags is what `openbox doctor` and the session record both read, so it
	// must agree with the struct field for every control.
	if p.Flags()["content_capture"] != p.ContentCapture {
		t.Error("Flags disagrees with the resolved posture — doctor would report a control that is not in force")
	}
	// require_verified_bundle must NOT be reported: it guarded a local bundle
	// that no longer exists (ADR-0017), so an org reading `true` would believe a
	// signature check was protecting it.
	if _, reported := p.Flags()["require_verified_bundle"]; reported {
		t.Error("require_verified_bundle is still reported — it cannot engage, so reporting it overstates")
	}
	// The documented defaults: ENFORCE (ADR-0016), with secret detection and
	// content capture on (content capture default-ON is the 2026-07-15 decision).
	//
	// This assertion used to read "enforce must default off — enforcement never
	// turns on by omission (INV-3)". ADR-0016 reversed the default deliberately,
	// and the INV-3 property it cited is preserved elsewhere and unchanged: a
	// FAILURE never blocks a tool call, because fail_closed defaults off and the
	// gate fails open on error. "Never enforce by omission" was a default, not an
	// invariant; "never BLOCK by failure" is the invariant, and it still holds —
	// see the fail_closed assertion below.
	if !p.Enforce {
		t.Error("enforce must default ON (ADR-0016)")
	}
	if p.FailClosed {
		t.Error("fail_closed must stay off — enforce-by-default is only defensible while an outage cannot block a developer")
	}
	if !p.SecretDetection || !p.ContentCapture {
		t.Errorf("secret_detection and content_capture default on, got %+v", p)
	}
}

// Policy provenance replaced the bundle coordinates, and the replacement has to
// answer a question posture can actually answer at the moment it is built.
//
// Posture rides SessionStarted — before this session has decided anything — so a
// deciding policy id here could only ever be some other session's. What IS
// knowable then is who decides and what happens when they cannot be reached, and
// under fail_open that second field is the honest statement that enforcement is
// reachability-dependent.
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

	// Metadata() is the ONLY path onto the wire, and asserting the struct is not
	// asserting the wire. That seam is how the gap shipped: population and
	// serialization were each tested, in isolation, and for months the control
	// plane was told neither field while `openbox doctor` printed both — the exact
	// inverse of the ADR-0017 argument, which is about what the control plane
	// knows. Build through EffectivePosture, not Posture{}: an empty string is
	// dropped by the unknown-value guard, so a zero value would pass vacuously.
	t.Run("both reach the emitted metadata, and no bundle key does", func(t *testing.T) {
		isolateConfig(t)
		m := EffectivePosture().Metadata()
		if m["decision_authority"] != DecisionAuthorityControlPlane {
			t.Errorf("decision_authority = %v, want %q — ADR-0017 makes this posture's policy-provenance evidence",
				m["decision_authority"], DecisionAuthorityControlPlane)
		}
		if m["failure_policy"] != FailurePolicyFailOpen {
			t.Errorf("failure_policy = %v, want %q", m["failure_policy"], FailurePolicyFailOpen)
		}
		// The bundle coordinates are gone, not empty. Absence has to be structural:
		// an empty-value guard would hide a field that a later edit repopulates.
		for k := range m {
			if strings.HasPrefix(k, "bundle_") || k == "staleness" {
				t.Errorf("%q is still emitted — it reports a subsystem ADR-0017 deleted", k)
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

	// The word that must not come back. "verified" and "integrity" described a
	// signature check over a local artifact; there is no artifact and no check,
	// so reusing the vocabulary for a weaker claim would be the overstatement
	// this repo's own rules forbid.
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

// A deprecation notice that cannot fire is the same as no notice. ADR-0017
// removed the last runtime caller of ResolveTier2, so a warning hung off that
// resolver was unreachable — found by driving the real binary, not by a test,
// which is why this one exists.
func TestDeprecatedKeysAreDetectedWherePostureIsRead(t *testing.T) {
	t.Run("silent when nothing deprecated is set", func(t *testing.T) {
		isolateConfig(t)
		if got := deadKeysPresent(); len(got) != 0 {
			t.Errorf("clean config reported deprecated keys: %v", got)
		}
	})

	// An explicit false is the case most worth warning about: it is the setting
	// that used to disable enforcement and silently no longer does.
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
