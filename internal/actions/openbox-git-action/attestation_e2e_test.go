package gitaction

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"testing"

	obgit "github.com/openbox-ai/openbox-shift-left/internal/adapters/common/git"
)

// End to end through a real repository: a commit carrying a session trailer plus
// a signed attestation note must resolve with the attestation attached, and the
// signature must verify against the agent's public key for that exact commit.
//
// This is the property the story exists for — the trailer alone is a claim anyone
// could write, and this proves the pipeline can carry the cryptographic upgrade
// from the developer's machine to the server.
func TestAttestation_EndToEndThroughRepo(t *testing.T) {
	r := newTestRepo(t)
	g := obgit.Git{Dir: r.dir}
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	const session = "sess-attested-1"

	sha := r.commit(trailerMsg("feat: real work", session))
	tree, parents, err := g.CommitIdentity(sha)
	if err != nil {
		t.Fatalf("commit identity: %v", err)
	}
	att, err := obgit.Attest(obgit.AttestationInput{
		Repo:          "github.com/acme/app",
		CommitSHA:     sha,
		TreeSHA:       tree,
		ParentSHAs:    parents,
		SessionIDs:    []string{session},
		DID:           "did:aip:7f3c9b2e-0000-5000-a000-000000000001",
		PrivateKeyB64: base64.StdEncoding.EncodeToString(priv.Seed()),
	})
	if err != nil {
		t.Fatalf("attest: %v", err)
	}
	if err := g.WriteAttestation(sha, att); err != nil {
		t.Fatalf("write attestation: %v", err)
	}

	res, err := NewResolver(r.dir, nil).Resolve(context.Background(), sha, "")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(res.Sessions) != 1 || res.Sessions[0].SessionID != session {
		t.Fatalf("resolution did not find the session: %+v", res.Sessions)
	}
	got := res.Sessions[0].Attestation
	if got == nil {
		t.Fatal("the signed attestation was not attached to the claim")
	}
	payload, err := got.Verify(pub, sha)
	if err != nil {
		t.Fatalf("attestation from the repo does not verify: %v", err)
	}
	if payload.TreeSHA != tree {
		t.Errorf("tree sha = %q, want %q", payload.TreeSHA, tree)
	}

	// ...and all the way onto the wire. Verifying the claim but never checking
	// the emitted event is what let the deploy event drop the attestation while
	// this test stayed green.
	sessions := deploySessions(t, res)
	att2, ok := sessions[0]["attestation"].(map[string]any)
	if !ok {
		t.Fatalf("the deploy event dropped the attestation: %#v", sessions[0])
	}
	if att2["sig_b64"] != att.SigB64 || att2["canonical_b64"] != att.CanonicalB64 {
		t.Error("the deploy event altered the signed bytes")
	}
}

// A commit with no note resolves exactly as before — absence must degrade to the
// inferred/attributed claim, not to an error, because notes are not pushed by
// default and most commits will have none.
func TestAttestation_AbsentNoteIsNormal(t *testing.T) {
	r := newTestRepo(t)
	sha := r.commit(trailerMsg("feat: unattested", "sess-plain-1"))

	res, err := NewResolver(r.dir, nil).Resolve(context.Background(), sha, "")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(res.Sessions) != 1 {
		t.Fatalf("expected one claim, got %+v", res.Sessions)
	}
	if res.Sessions[0].Attestation != nil {
		t.Error("no attestation should be attached when there is no note")
	}
}

// An attestation naming a different session must not be presented as evidence
// for this claim: notes are keyed by sha, so one cannot be moved between
// commits, but a single note can name sessions other than the one being resolved.
func TestAttestation_ForeignSessionNotAttached(t *testing.T) {
	r := newTestRepo(t)
	g := obgit.Git{Dir: r.dir}
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	sha := r.commit(trailerMsg("feat: work", "sess-mine"))

	att, err := obgit.Attest(obgit.AttestationInput{
		CommitSHA:     sha,
		SessionIDs:    []string{"sess-someone-else"},
		DID:           "did:aip:7f3c9b2e-0000-5000-a000-000000000001",
		PrivateKeyB64: base64.StdEncoding.EncodeToString(priv.Seed()),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := g.WriteAttestation(sha, att); err != nil {
		t.Fatal(err)
	}

	res, err := NewResolver(r.dir, nil).Resolve(context.Background(), sha, "")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(res.Sessions) != 1 {
		t.Fatalf("expected one claim, got %+v", res.Sessions)
	}
	if res.Sessions[0].Attestation != nil {
		t.Error("an attestation naming another session must not be attached to this claim")
	}
}
