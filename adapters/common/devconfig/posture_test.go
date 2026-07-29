package devconfig

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

// The posture booleans must always be present. An absent flag is ambiguous
// between "off" and "this client is too old to report it", and telling those
// apart is the entire point of recording posture.
func TestPostureMetadata_BooleansAlwaysPresent(t *testing.T) {
	m := Posture{}.Metadata()
	for _, k := range []string{
		"enforce", "fail_closed", "tier2", "secret_detection",
		"content_capture", "findings", "finops",
	} {
		v, ok := m[k]
		if !ok {
			t.Errorf("%s missing from posture metadata", k)
			continue
		}
		if _, isBool := v.(bool); !isBool {
			t.Errorf("%s should be a bool, got %T", k, v)
		}
	}
}

// Strings are omitted when unknown, so a field the adapter could not determine
// reads as absent rather than as a false claim.
func TestPostureMetadata_UnknownStringsOmitted(t *testing.T) {
	m := Posture{}.Metadata()
	for _, k := range []string{
		"adapter", "adapter_version", "provider_version",
		"bundle_version", "bundle_policy_id", "bundle_sha256", "staleness",
	} {
		if _, present := m[k]; present {
			t.Errorf("%s should be omitted when empty, got %v", k, m[k])
		}
	}

	full := Posture{
		Adapter: "codex/1", AdapterVersion: "codex/1", ProviderVersion: "codex-cli 0.145.0",
		BundleVersion: "v7", BundlePolicyID: "pol-1", BundleSHA256: "abc123",
		Staleness: StalenessFresh,
	}.Metadata()
	if full["staleness"] != string(StalenessFresh) {
		t.Errorf("staleness = %v, want %q", full["staleness"], StalenessFresh)
	}
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
		BundleVersion:   "sk-proj-abcdefghijklmnop",
		BundlePolicyID:  "ghp_0123456789abcdefghij",
		BundleSHA256:    strings.Repeat("a", 64),
		Staleness:       StalenessFresh,
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
	// The documented defaults: observe, with secret detection and content
	// capture on (content capture default-ON is brian's 2026-07-15 decision).
	if p.Enforce {
		t.Error("enforce must default off — enforcement never turns on by omission (INV-3)")
	}
	if !p.SecretDetection || !p.ContentCapture {
		t.Errorf("secret_detection and content_capture default on, got %+v", p)
	}
}
