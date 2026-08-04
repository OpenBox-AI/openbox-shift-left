package git

import (
	"crypto/ed25519"
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

func testKeypair(t *testing.T) (seedB64 string, pub ed25519.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(priv.Seed()), pub
}

func sampleInput(seedB64 string) AttestationInput {
	return AttestationInput{
		Repo:           "github.com/acme/app",
		CommitSHA:      "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		TreeSHA:        "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		ParentSHAs:     []string{"cccccccccccccccccccccccccccccccccccccccc"},
		SessionIDs:     []string{"sess-1"},
		BundlePolicyID: "pol-1",
		BundleSHA256:   strings.Repeat("d", 64),
		Adapter:        "openbox-cli",
		DID:            "did:aip:7f3c9b2e-0000-5000-a000-000000000001",
		SeedB64:        seedB64,
		Now:            func() time.Time { return time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC) },
	}
}

func TestAttest_SignsAndVerifies(t *testing.T) {
	seed, pub := testKeypair(t)
	att, err := Attest(sampleInput(seed))
	if err != nil {
		t.Fatalf("attest: %v", err)
	}
	payload, err := att.Verify(pub, sampleInput(seed).CommitSHA)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if payload.CommitSHA != sampleInput(seed).CommitSHA || payload.SessionIDs[0] != "sess-1" {
		t.Errorf("payload did not round-trip: %+v", payload)
	}
	// The policy in force is part of the statement — that is what makes this
	// worth more than provenance alone.
	if payload.BundlePolicyID != "pol-1" || payload.BundleSHA256 == "" {
		t.Errorf("bundle coordinates missing from the signed payload: %+v", payload)
	}
	if payload.Version != AttestationVersion {
		t.Errorf("version = %d, want %d", payload.Version, AttestationVersion)
	}
}

// The attack this exists to stop: stamping a real session onto an unrelated
// commit. A genuine attestation replayed onto another commit must fail even
// though its signature is valid.
func TestAttest_ReplayOntoAnotherCommitFails(t *testing.T) {
	seed, pub := testKeypair(t)
	att, err := Attest(sampleInput(seed))
	if err != nil {
		t.Fatal(err)
	}
	other := "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	if _, err := att.Verify(pub, other); err == nil {
		t.Error("an attestation for one commit must not verify against another")
	}
}

func TestAttest_TamperedPayloadFails(t *testing.T) {
	seed, pub := testKeypair(t)
	att, err := Attest(sampleInput(seed))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.StdEncoding.DecodeString(att.CanonicalB64)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)/2] ^= 0x01
	att.CanonicalB64 = base64.StdEncoding.EncodeToString(raw)
	if _, err := att.Verify(pub, ""); err == nil {
		t.Error("a tampered payload must not verify")
	}
}

// A different agent's key must not verify, or the DID binding would be
// decorative.
func TestAttest_ForeignKeyFails(t *testing.T) {
	seed, _ := testKeypair(t)
	_, otherPub := testKeypair(t)
	att, err := Attest(sampleInput(seed))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := att.Verify(otherPub, ""); err == nil {
		t.Error("an attestation must not verify under a foreign key")
	}
}

// The outer DID is routing metadata; if it disagrees with the signed value, a
// verifier that resolved the key from the outer field would check the wrong key.
func TestAttest_OuterDIDMustMatchSigned(t *testing.T) {
	seed, pub := testKeypair(t)
	att, err := Attest(sampleInput(seed))
	if err != nil {
		t.Fatal(err)
	}
	att.DID = "did:aip:00000000-0000-5000-a000-000000000999"
	if _, err := att.Verify(pub, ""); err == nil {
		t.Error("a mismatched outer DID must be rejected")
	}
}

// Refuse rather than produce something unverifiable: a broken attestation would
// force the deploy path to decide what it means.
func TestAttest_RefusesIncompleteInput(t *testing.T) {
	seed, _ := testKeypair(t)
	cases := map[string]func(*AttestationInput){
		"no commit sha": func(i *AttestationInput) { i.CommitSHA = "" },
		"no session":    func(i *AttestationInput) { i.SessionIDs = nil },
		"invalid session": func(i *AttestationInput) {
			i.SessionIDs = []string{"has space"}
		},
		"no did":      func(i *AttestationInput) { i.DID = "" },
		"bad did":     func(i *AttestationInput) { i.DID = "not-a-did" },
		"no seed":     func(i *AttestationInput) { i.SeedB64 = "" },
		"short seed":  func(i *AttestationInput) { i.SeedB64 = base64.StdEncoding.EncodeToString([]byte("short")) },
		"broken seed": func(i *AttestationInput) { i.SeedB64 = "!!!" },
	}
	for name, mutate := range cases {
		in := sampleInput(seed)
		mutate(&in)
		if _, err := Attest(in); err == nil {
			t.Errorf("%s: Attest should have refused", name)
		}
	}
}

// INV-1: the signed bytes are shipped to the server, so a credential embedded in
// a remote URL must never reach them.
func TestCanonicalRemote_StripsCredentials(t *testing.T) {
	cases := map[string]string{
		"git@github.com:acme/app.git":                    "github.com/acme/app",
		"https://github.com/acme/app.git":                "github.com/acme/app",
		"https://x-token:ghp_secret@github.com/acme/app": "github.com/acme/app",
		"ssh://git@github.com/acme/app.git":              "github.com/acme/app",
		"":                                               "",
	}
	for in, want := range cases {
		if got := canonicalRemote(in); got != want {
			t.Errorf("canonicalRemote(%q) = %q, want %q", in, got, want)
		}
		if strings.Contains(canonicalRemote(in), "ghp_secret") {
			t.Errorf("credential leaked into the signed repo identity from %q", in)
		}
	}
}
