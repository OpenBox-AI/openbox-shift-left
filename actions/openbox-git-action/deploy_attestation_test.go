package gitaction

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	obgit "github.com/openbox-ai/openbox-shift-left/adapters/common/git"
)

// The attestation must survive the last hop: resolution attaches it to the
// claim, but only what BuildDeployEvent writes into metadata.sessions[] ever
// reaches core. Core requires ownership AND an accepted attestation to record
// verified lineage, so a claim that arrives without its signature pins the link
// to verified:false no matter how good the signature was.
//
// The original E8-S10 gap was exactly here: resolve.go attached, deploy.go
// dropped, and the end-to-end test stopped at Resolution — so this asserts on
// the marshaled event, the same bytes the client would sign.
func TestBuildDeployEvent_CarriesAttestation(t *testing.T) {
	res := fixedResolution()
	res.Sessions[0].Attestation = &obgit.Attestation{
		CanonicalB64: "eyJ2IjoxfQ==",
		SigB64:       "c2lnbmF0dXJl",
		DID:          "did:aip:7f3c9b2e-0000-5000-a000-000000000001",
	}

	sessions := deploySessions(t, res)
	att, ok := sessions[0]["attestation"].(map[string]any)
	if !ok {
		t.Fatalf("metadata.sessions[0].attestation missing or not an object: %#v", sessions[0])
	}
	if att["canonical_b64"] != "eyJ2IjoxfQ==" {
		t.Errorf("canonical_b64 = %v, want the signed bytes verbatim", att["canonical_b64"])
	}
	if att["sig_b64"] != "c2lnbmF0dXJl" {
		t.Errorf("sig_b64 = %v", att["sig_b64"])
	}
	if att["did"] != "did:aip:7f3c9b2e-0000-5000-a000-000000000001" {
		t.Errorf("did = %v", att["did"])
	}
}

// Absence is the common case (notes are not pushed by default) and must stay
// clean: an explicit null would make core's `len(s.Attestation) > 0` probe log
// a rejection for every ordinary commit.
func TestBuildDeployEvent_OmitsAbsentAttestation(t *testing.T) {
	sessions := deploySessions(t, fixedResolution())
	if _, present := sessions[0]["attestation"]; present {
		t.Errorf("attestation key must be omitted when there is no note: %#v", sessions[0])
	}
}

// A git note is attacker-writable and is carried verbatim, so the resolver must
// refuse to lift an oversized one into the governance record.
func TestAttachAttestations_RejectsOversized(t *testing.T) {
	huge := &obgit.Attestation{CanonicalB64: strings.Repeat("A", maxAttestationBytes+1)}
	if withinAttestationSizeLimit(huge) {
		t.Error("an attestation larger than the cap must not be carried")
	}
	ok := &obgit.Attestation{CanonicalB64: "eyJ2IjoxfQ==", SigB64: "c2ln", DID: "did:aip:x"}
	if !withinAttestationSizeLimit(ok) {
		t.Error("a realistic attestation must be carried")
	}
	if withinAttestationSizeLimit(nil) {
		t.Error("nil must not report as carryable")
	}
}

// deploySessions builds the deploy event and returns metadata.sessions[] as it
// would appear on the wire — decoded from the marshaled event, not read off the
// in-memory map, so json tags and omitempty are exercised.
func deploySessions(t *testing.T, res Resolution) []map[string]any {
	t.Helper()
	ev := BuildDeployEvent(res, DeployMeta{
		Repo:         "openbox-ai/openbox-shift-left",
		Environment:  "production",
		DeveloperDID: "did:aip:7f3c9b2e-0000-5000-a000-000000000001",
	}, time.Date(2026, 7, 31, 9, 0, 0, 0, time.UTC))

	raw, err := json.Marshal(ev.Metadata)
	if err != nil {
		t.Fatalf("marshal metadata: %v", err)
	}
	var md struct {
		Sessions []map[string]any `json:"sessions"`
	}
	if err := json.Unmarshal(raw, &md); err != nil {
		t.Fatalf("unmarshal metadata: %v", err)
	}
	if len(md.Sessions) != 1 {
		t.Fatalf("want 1 session, got %d", len(md.Sessions))
	}
	return md.Sessions
}
