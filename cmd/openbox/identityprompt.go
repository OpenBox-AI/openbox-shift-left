package main

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"regexp"
	"strings"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
)

// Validation and display for an agent identity typed in by hand.
//
// These moved out of `auth` when it stopped owning agent credentials. They are
// not dead weight left behind: `init --provider <tool>` offers to adopt an
// existing agent rather than register a new one, and adopting means pasting
// exactly these four values. Keeping one copy is what stops the adopt prompt
// from growing a second, subtly different idea of what a valid DID or seed
// looks like.

var didPattern = regexp.MustCompile(`^did:aip:[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// validateAgentIdentity returns a human-readable problem with a hand-entered
// agent identity, or "" when all three values are usable. It never echoes a
// secret body: the org/agent key mix-up is reported by prefix only.
func validateAgentIdentity(did, apiKey, privateKey string) string {
	if strings.HasPrefix(strings.TrimSpace(apiKey), "obx_key_") {
		return fmt.Sprintf("that looks like an ORGANIZATION key (%s…), not this agent's runtime key.\n"+
			"  An obx_key_ key belongs in %s and can create and rotate agents org-wide.\n"+
			"  The agent runtime key starts obx_ (no `key_`) and is shown once on the agent's page\n"+
			"  when it is created. See docs/getting-started.md § Get the right credential.",
			safePrefix(strings.TrimSpace(apiKey)), devconfig.EnvControlToken)
	}
	if strings.TrimSpace(apiKey) == "" {
		return "no API key given. Paste this agent's obx_ runtime key, or decline the adopt\n" +
			"  prompt to register a new agent and have one issued."
	}
	if problem := privateKeyProblem(privateKey); problem != "" {
		return problem
	}
	if strings.TrimSpace(did) == "" {
		return "no DID given. It is on the agent's page in the dashboard, and looks like\n" +
			"  did:aip:xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx. Decline the adopt prompt to register instead."
	}
	if !didPattern.MatchString(strings.TrimSpace(did)) {
		return fmt.Sprintf("%q is not a valid DID. Expected did:aip:<uuid>, e.g.\n"+
			"  did:aip:3f2504e0-4f89-11d3-9a0c-0305e82c3301", did)
	}
	return ""
}

func privateKeyProblem(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return "no signing key given. It is shown once, when the agent is created; decline the\n" +
			"  adopt prompt to register a new agent."
	}
	raw, err := base64.StdEncoding.DecodeString(v)
	if err != nil {
		return "the signing key is not valid base64. Paste the value exactly as OpenBox showed it\n" +
			"  (it is about 44 characters and usually ends in '=')."
	}
	if len(raw) != ed25519.SeedSize {
		return fmt.Sprintf("the signing key decodes to %d bytes; an Ed25519 seed is %d.\n"+
			"  Check you pasted the whole value and not a truncated copy.", len(raw), ed25519.SeedSize)
	}
	return ""
}

func maskToken(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return "(none)"
	}
	if len(v) <= 12 {
		return fmt.Sprintf("(%d chars)", len(v))
	}
	return fmt.Sprintf("%s…%s (%d chars)", v[:8], v[len(v)-4:], len(v))
}

// publicKeyFingerprint the seed itself is never hashed or displayed.
func publicKeyFingerprint(seedB64 string) string {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(seedB64))
	if err != nil || len(raw) != ed25519.SeedSize {
		if strings.TrimSpace(seedB64) == "" {
			return "(none)"
		}
		return "(unreadable; validation will reject it)"
	}
	pub := ed25519.NewKeyFromSeed(raw).Public().(ed25519.PublicKey)
	sum := sha256.Sum256(pub)
	return fmt.Sprintf("SHA256:%s (public key)", base64.RawStdEncoding.EncodeToString(sum[:])[:24])
}
