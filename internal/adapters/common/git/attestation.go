package git

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// AttestationVersion is the payload schema version. It is inside the signed
// bytes, so a verifier cannot be tricked into reading a v1 payload as a later
// shape with different semantics.
const AttestationVersion = 1

// Attestation is the signed statement about one commit.
type Attestation struct {
	// CanonicalB64 is the exact bytes that were signed, base64.
	CanonicalB64 string `json:"canonical_b64"`
	SigB64       string `json:"sig_b64"`
	// DID identifies the signing key so the verifier can resolve it. It is also
	// inside the signed payload; this copy is a routing convenience, and a
	// verifier must use the payload's value to decide whose key to check.
	DID string `json:"did"`
}

// AttestationPayload is what gets signed.
type AttestationPayload struct {
	Version int `json:"v"`
	// Repo is a canonical remote identity ("github.com/org/repo"), not a local
	// path: a path would differ per machine and say nothing about which
	// repository the commit belongs to.
	Repo string `json:"repo,omitempty"`
	// CommitSHA, TreeSHA and ParentSHAs pin the commit's identity.
	CommitSHA  string   `json:"commit_sha"`
	TreeSHA    string   `json:"tree_sha,omitempty"`
	ParentSHAs []string `json:"parent_shas,omitempty"`
	// SessionIDs are the sessions being claimed, in trailer order.
	SessionIDs []string `json:"session_ids"`
	// ThreadID records a forked Codex thread when it differs from the session
	// (E8-S4), so a commit attributed by thread id can still be joined to the
	// root session's event stream.
	ThreadID string `json:"thread_id,omitempty"`
	// BundlePolicyID and BundleSHA256 record which policy was in force.
	BundlePolicyID string `json:"bundle_policy_id,omitempty"`
	BundleSHA256   string `json:"bundle_sha256,omitempty"`
	Adapter        string `json:"adapter,omitempty"`
	DID            string `json:"did"`
	SignedAt       string `json:"signed_at"` // RFC3339
}

// AttestationInput is everything Attest needs.
type AttestationInput struct {
	Repo           string
	CommitSHA      string
	TreeSHA        string
	ParentSHAs     []string
	SessionIDs     []string
	ThreadID       string
	BundlePolicyID string
	BundleSHA256   string
	Adapter        string
	DID            string
	// PrivateKeyB64 is the agent's Ed25519 seed (32 bytes, base64); the same seed
	// AIP request signing uses, so no new key material is introduced and the
	// public half is already KMS-resident under the DID alias.
	PrivateKeyB64 string
	Now           func() time.Time
}

// Attest builds and signs an attestation. Producing an attestation that cannot
// be verified would be worse than producing none, because the deploy path
// would then have to decide what a broken attestation means.
func Attest(in AttestationInput) (*Attestation, error) {
	if in.CommitSHA == "" {
		return nil, fmt.Errorf("attestation needs a commit sha")
	}
	sessions := validSessionIDs(in.SessionIDs)
	if len(sessions) == 0 {
		return nil, fmt.Errorf("attestation needs at least one valid session id")
	}
	if !strings.HasPrefix(in.DID, "did:aip:") {
		return nil, fmt.Errorf("attestation needs the agent DID")
	}
	seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(in.PrivateKeyB64))
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("attestation needs a %d-byte Ed25519 seed", ed25519.SeedSize)
	}
	now := time.Now
	if in.Now != nil {
		now = in.Now
	}

	payload := AttestationPayload{
		Version:        AttestationVersion,
		Repo:           in.Repo,
		CommitSHA:      in.CommitSHA,
		TreeSHA:        in.TreeSHA,
		ParentSHAs:     in.ParentSHAs,
		SessionIDs:     sessions,
		ThreadID:       in.ThreadID,
		BundlePolicyID: in.BundlePolicyID,
		BundleSHA256:   in.BundleSHA256,
		Adapter:        in.Adapter,
		DID:            in.DID,
		SignedAt:       now().UTC().Format(time.RFC3339),
	}
	canonical, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal attestation payload: %w", err)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	return &Attestation{
		CanonicalB64: base64.StdEncoding.EncodeToString(canonical),
		SigB64:       base64.StdEncoding.EncodeToString(ed25519.Sign(priv, canonical)),
		DID:          in.DID,
	}, nil
}

// Payload decodes the signed bytes. Callers that have not verified the
// signature must treat the result as untrusted input.
func (a *Attestation) Payload() (AttestationPayload, error) {
	var p AttestationPayload
	if a == nil {
		return p, fmt.Errorf("no attestation")
	}
	raw, err := base64.StdEncoding.DecodeString(a.CanonicalB64)
	if err != nil {
		return p, fmt.Errorf("decode attestation payload: %w", err)
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return p, fmt.Errorf("parse attestation payload: %w", err)
	}
	if p.Version != AttestationVersion {
		return p, fmt.Errorf("unsupported attestation version %d", p.Version)
	}
	return p, nil
}

// Verify checks the signature against a public key and that the payload is
// about the expected commit.
func (a *Attestation) Verify(pub ed25519.PublicKey, expectCommitSHA string) (AttestationPayload, error) {
	payload, err := a.Payload()
	if err != nil {
		return payload, err
	}
	if len(pub) != ed25519.PublicKeySize {
		return payload, fmt.Errorf("attestation verify needs a %d-byte public key", ed25519.PublicKeySize)
	}
	canonical, err := base64.StdEncoding.DecodeString(a.CanonicalB64)
	if err != nil {
		return payload, fmt.Errorf("decode attestation payload: %w", err)
	}
	sig, err := base64.StdEncoding.DecodeString(a.SigB64)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return payload, fmt.Errorf("malformed attestation signature")
	}
	if !ed25519.Verify(pub, canonical, sig) {
		return payload, fmt.Errorf("attestation signature does not verify")
	}
	if expectCommitSHA != "" && payload.CommitSHA != expectCommitSHA {
		return payload, fmt.Errorf("attestation is for commit %s, not %s", payload.CommitSHA, expectCommitSHA)
	}
	if a.DID != "" && a.DID != payload.DID {
		return payload, fmt.Errorf("attestation DID mismatch: outer %s, signed %s", a.DID, payload.DID)
	}
	return payload, nil
}

// attestationNoteRef keeping them apart means the human-readable breadcrumb
// and the cryptographic artifact can be fetched, pushed and pruned
// independently; and reading one can never accidentally parse the other.
const attestationNoteRef = "refs/notes/openbox-attest"

// WriteAttestation stores an attestation as a git note on rev.
func (g Git) WriteAttestation(rev string, a *Attestation) error {
	if a == nil {
		return fmt.Errorf("no attestation to write")
	}
	if rev == "" {
		rev = "HEAD"
	}
	body, err := json.Marshal(a)
	if err != nil {
		return fmt.Errorf("marshal attestation: %w", err)
	}
	if _, err := g.run("notes", "--ref", attestationNoteRef, "add", "-f", "-m", string(body), rev); err != nil {
		return fmt.Errorf("write attestation note: %w", err)
	}
	return nil
}

// ReadAttestation returns the attestation recorded for rev, or nil when there
// is none. A missing note is not an error: notes are not pushed by default and
// most commits will have none, so the deploy path must treat absence as
// "inferred" rather than as a failure.
func (g Git) ReadAttestation(rev string) (*Attestation, error) {
	if rev == "" {
		rev = "HEAD"
	}
	// A truncated note cannot be valid JSON, so it is refused rather than half-
	// parsed.
	out, truncated, err := g.runLimited(MaxNoteBytes, "notes", "--ref", attestationNoteRef, "show", rev)
	if err != nil {
		return nil, nil // no note for this commit
	}
	if truncated {
		return nil, fmt.Errorf("attestation note for %s exceeds %d bytes", rev, MaxNoteBytes)
	}
	body := strings.TrimSpace(string(out))
	if body == "" {
		return nil, nil
	}
	var a Attestation
	if err := json.Unmarshal([]byte(body), &a); err != nil {
		return nil, fmt.Errorf("parse attestation note: %w", err)
	}
	return &a, nil
}

// RevParse resolves a revision to its full sha.
func (g Git) RevParse(rev string) (string, error) {
	if rev == "" {
		rev = "HEAD"
	}
	out, err := g.run("rev-parse", rev)
	if err != nil {
		return "", fmt.Errorf("rev-parse %s: %w", rev, err)
	}
	sha := strings.TrimSpace(string(out))
	if sha == "" {
		return "", fmt.Errorf("rev-parse %s returned nothing", rev)
	}
	return sha, nil
}

// CommitIdentity returns the tree and parent shas for a commit, the fields
// that make an attestation about this exact content rather than about a sha
// string.
func (g Git) CommitIdentity(rev string) (treeSHA string, parents []string, err error) {
	if rev == "" {
		rev = "HEAD"
	}
	out, err := g.run("show", "-s", "--format=%T %P", rev)
	if err != nil {
		return "", nil, fmt.Errorf("read commit identity: %w", err)
	}
	fields := strings.Fields(strings.TrimSpace(string(out)))
	if len(fields) == 0 {
		return "", nil, fmt.Errorf("no commit identity for %s", rev)
	}
	return fields[0], fields[1:], nil
}

// CanonicalRemote returns a host/path identity for the repo's origin, or ""
// when there is no usable remote.
func (g Git) CanonicalRemote() string {
	out, err := g.run("config", "--get", "remote.origin.url")
	if err != nil {
		return ""
	}
	return canonicalRemote(strings.TrimSpace(string(out)))
}

func canonicalRemote(url string) string {
	u := strings.TrimSpace(url)
	if u == "" {
		return ""
	}
	u = strings.TrimSuffix(u, ".git")
	switch {
	case strings.HasPrefix(u, "git@"):
		u = strings.TrimPrefix(u, "git@")
		return strings.Replace(u, ":", "/", 1)
	case strings.Contains(u, "://"):
		parts := strings.SplitN(u, "://", 2)
		rest := parts[1]
		if at := strings.LastIndex(rest, "@"); at >= 0 {
			rest = rest[at+1:]
		}
		return rest
	default:
		return u
	}
}
