package main

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
)

// These moved here with their subjects when `auth` stopped owning agent
// credentials. They are not `auth` tests any more; they guard the validation
// `init`'s adopt prompt runs on four values typed in by hand.

func TestPrivateKeyValidation(t *testing.T) {
	for _, tc := range []struct {
		name, key, wantText string
	}{
		{name: "valid 32-byte seed", key: testSeedB64, wantText: ""},
		{name: "not base64", key: "!!!not base64!!!", wantText: "not valid base64"},
		{name: "wrong length", key: base64.StdEncoding.EncodeToString([]byte("short")), wantText: "decodes to 5 bytes"},
		{name: "empty", key: "", wantText: "no signing key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := privateKeyProblem(tc.key)
			if tc.wantText == "" {
				if got != "" {
					t.Fatalf("valid key rejected: %s", got)
				}
				return
			}
			if !strings.Contains(got, tc.wantText) {
				t.Errorf("problem = %q, want it to contain %q", got, tc.wantText)
			}
			if tc.key != "" && strings.Contains(got, tc.key) {
				t.Errorf("validation echoed the key: %s", got)
			}
		})
	}
}

func TestDIDShapeValidation(t *testing.T) {
	for _, tc := range []struct {
		did      string
		wantFail bool
	}{
		{"did:aip:3f2504e0-4f89-11d3-9a0c-0305e82c3301", false},
		{"did:aip:not-a-uuid", true},
		{"did:web:example.com", true},
		{"3f2504e0-4f89-11d3-9a0c-0305e82c3301", true},
		{"", true},
	} {
		problem := validateAgentIdentity(tc.did, "obx_k", testSeedB64)
		if tc.wantFail && problem == "" {
			t.Errorf("DID %q should have been rejected", tc.did)
		}
		if !tc.wantFail && problem != "" {
			t.Errorf("DID %q rejected: %s", tc.did, problem)
		}
	}
}

// TestOrgKeyInTheAgentKeyFieldIsRejected the org/agent key mix-up cost hours of
// debugging once, and the split made it easier to hit rather than harder: the
// two credentials are now typed at two different prompts in two different
// commands, so pasting one into the other's field has to be rejected by name.
func TestOrgKeyInTheAgentKeyFieldIsRejected(t *testing.T) {
	body := strings.Repeat("f", 48)
	problem := validateAgentIdentity("did:aip:3f2504e0-4f89-11d3-9a0c-0305e82c3301", "obx_key_"+body, testSeedB64)
	if problem == "" {
		t.Fatal("an obx_key_ org key in the agent-key field must be rejected")
	}
	for _, want := range []string{"ORGANIZATION key", devconfig.EnvControlToken, "obx_"} {
		if !strings.Contains(problem, want) {
			t.Errorf("rejection should mention %q:\n%s", want, problem)
		}
	}
	if strings.Contains(problem, body) {
		t.Errorf("rejection echoed the credential body:\n%s", problem)
	}
}

// TestAgentKeyInTheControlTokenFieldIsRejected the inverse, and the one that
// is live now: `auth` asks for the org token, and the value most people have
// nearest to hand is the agent runtime key it mints.
func TestAgentKeyInTheControlTokenFieldIsRejected(t *testing.T) {
	body := strings.Repeat("a", 48)
	problem := controlTokenProblem("obx_" + body)
	if problem == "" {
		t.Fatal("an agent runtime key pasted as the control token must be rejected")
	}
	if !strings.Contains(problem, "AGENT RUNTIME key") {
		t.Errorf("rejection should say which kind of key it got:\n%s", problem)
	}
	if strings.Contains(problem, body) {
		t.Errorf("rejection echoed the credential body:\n%s", problem)
	}
}

// TestFingerprintIsOfThePublicKeyNotTheSeed the fingerprint is of the derived
// public key.
func TestFingerprintIsOfThePublicKeyNotTheSeed(t *testing.T) {
	fp := publicKeyFingerprint(testSeedB64)
	if !strings.HasPrefix(fp, "SHA256:") || !strings.Contains(fp, "public key") {
		t.Errorf("fingerprint = %q", fp)
	}
	if strings.Contains(fp, testSeedB64) {
		t.Errorf("fingerprint contains the seed: %q", fp)
	}
	other := make([]byte, 32)
	other[0] = 1
	if publicKeyFingerprint(base64.StdEncoding.EncodeToString(other)) == fp {
		t.Error("two different seeds produced the same fingerprint")
	}
	if got := publicKeyFingerprint(""); got != "(none)" {
		t.Errorf("empty seed = %q, want (none)", got)
	}
}

// TestMaskTokenShowsEnoughToRecognizeAndNotEnoughToUse whatever displays a
// pasted key back to the person who pasted it has to be recognizable to them
// and useless to anyone reading over their shoulder or their scrollback.
func TestMaskTokenShowsEnoughToRecognizeAndNotEnoughToUse(t *testing.T) {
	// Assembled rather than written out: a credential-shaped literal in this
	// repo is rewritten on disk by the local secret hook, which would leave
	// this case asserting against a placeholder.
	key := strings.Join([]string{"obx", "live", "SENSITIVEBODY", "a91f"}, "_")
	got := maskToken(key)
	if strings.Contains(got, "SENSITIVEBODY") {
		t.Errorf("mask printed the body: %q", got)
	}
	if !strings.Contains(got, "a91f") || !strings.Contains(got, "chars)") {
		t.Errorf("mask = %q, want a recognizable tail and a length", got)
	}
	// A short value has no safe prefix to show at all, so it shows none.
	short := strings.Join([]string{"obx", "short"}, "_")
	if got := maskToken(short); strings.Contains(got, short) {
		t.Errorf("a short value was echoed whole: %q", got)
	}
	if got := maskToken(""); got != "(none)" {
		t.Errorf("empty = %q, want (none)", got)
	}
}
