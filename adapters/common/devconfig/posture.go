package devconfig

import "regexp"

// Effective endpoint posture — the evidence that makes assurance tiers
// visible without a managed deployment (E8-S5).
//
// Enforcement, redaction, content capture and policy freshness are all
// resolved on the developer's own machine from a config file the developer can
// edit and env vars they can set. The control plane therefore cannot tell an
// enforcing session from an observing one, nor a session running fresh policy
// from one whose freshness check was silently skipped for want of a control
// token (report SL-01 and the SL-03 nuance). Posture closes that by recording
// what was *actually* in effect, as ordinary structural metadata on the
// session-start event: no new endpoint, no new table, and — because core
// merges event metadata into the session's Merkle leaf — tamper-evident for
// free.
//
// It is a claim by the endpoint, not a proof about it. A tampered client can
// lie. What it buys is that the honest majority becomes measurable and a
// downgrade becomes visible, which is the prerequisite for the managed tier
// (E8-S8/S9) meaning anything.
//
// INV-1: every field is a boolean, a bounded enum, or an opaque non-secret id.
// No token, key, seed, path or content value may ever be added here — this
// object egresses on every session start.

// Staleness is the outcome of the session-start policy-freshness check.
// Naming the *reason* a check did not happen is the point: "skipped" used to
// be a line on the hook's stderr that nobody collected, so a session running
// unverified policy looked exactly like one running fresh policy.
type Staleness string

const (
	// StalenessNotChecked — the check did not run because the session is not
	// enforcing. Honest and expected in observe mode, not a degradation.
	StalenessNotChecked Staleness = "not_checked"
	// StalenessFresh — local bundle pin matches the backend's current policy.
	StalenessFresh Staleness = "fresh"
	// StalenessStaleWarned — policy changed; fail-open, so the session
	// proceeded on the last-good bundle after warning.
	StalenessStaleWarned Staleness = "stale_warned"
	// StalenessStaleBlocked — policy changed; fail-closed, so the session was
	// marked stale and its tool calls are denied until `openbox dev sync`.
	StalenessStaleBlocked Staleness = "stale_blocked"
	// StalenessSkippedNoToken — no control token / backend url / agent id, so
	// freshness is unknowable. This is the SL-03 silent skip, now recorded.
	StalenessSkippedNoToken Staleness = "skipped_no_token"
	// StalenessSkippedNoPin — no local bundle pin to compare against (never
	// synced, or a pinless bundle).
	StalenessSkippedNoPin Staleness = "skipped_no_pin"
	// StalenessError — the check ran and failed (offline, HTTP error, bad
	// response). The session proceeded on the last-good bundle.
	StalenessError Staleness = "error"
)

// Posture is the effective posture of one session. The boolean block is
// resolved from config+env by EffectivePosture; the string block is supplied
// by the adapter, which owns the bundle read and knows its own versions.
type Posture struct {
	// Resolved enforcement/privacy posture.
	Enforce         bool
	FailClosed      bool
	Tier2           bool
	SecretDetection bool
	ContentCapture  bool
	Findings        bool
	Finops          bool

	// Adapter-supplied. Bundle* are opaque staleness/integrity coordinates
	// (a policy id, an opaque version, a content hash) — never policy text.
	Adapter         string
	AdapterVersion  string
	ProviderVersion string
	BundleVersion   string
	BundlePolicyID  string
	BundleSHA256    string
	Staleness       Staleness
}

// EffectivePosture resolves the posture fields that come from config and env,
// using the same resolvers the runtime itself calls — so the recorded posture
// cannot drift from the behaviour it describes. The adapter fills in the rest.
func EffectivePosture() Posture {
	return Posture{
		Enforce:         ResolveEnforce(),
		FailClosed:      ResolveFailClosed(),
		Tier2:           ResolveTier2(),
		SecretDetection: ResolveSecretDetection(),
		ContentCapture:  ResolveContentCapture(),
		Findings:        ResolveFindings(),
		Finops:          ResolveFinops(),
	}
}

// Metadata renders the posture for a session-start event's metadata under a
// single "posture" key.
//
// The booleans are always present: an absent flag would be ambiguous between
// "off" and "this client is too old to report it", and the whole value of the
// record is being able to tell those apart. The strings are omitted when
// empty, so a field the adapter cannot determine reads as unknown rather than
// as a false claim.
func (p Posture) Metadata() map[string]any {
	m := map[string]any{
		"enforce":          p.Enforce,
		"fail_closed":      p.FailClosed,
		"tier2":            p.Tier2,
		"secret_detection": p.SecretDetection,
		"content_capture":  p.ContentCapture,
		"findings":         p.Findings,
		"finops":           p.Finops,
	}
	for k, v := range map[string]string{
		"adapter":          p.Adapter,
		"adapter_version":  p.AdapterVersion,
		"provider_version": p.ProviderVersion,
		"bundle_version":   p.BundleVersion,
		"bundle_policy_id": p.BundlePolicyID,
		"bundle_sha256":    p.BundleSHA256,
		"staleness":        string(p.Staleness),
	} {
		if v == "" || looksLikeSecret(v) {
			continue // unknown, or refused — see looksLikeSecret
		}
		m[k] = truncate(v, maxPostureValueLen)
	}
	return m
}

// maxPostureValueLen bounds each string value. These are identifiers we
// mostly generate ourselves, but provider_version comes from an external
// binary, so the whole object stays bounded at the untrusted boundary.
const maxPostureValueLen = 256

// secretShaped matches the credential formats this system actually handles:
// OpenBox agent and org keys, PEM private keys, and the provider API-key
// prefixes a misconfigured environment might surface.
var secretShaped = regexp.MustCompile(`obx_live_|obx_test_|obx_key_|BEGIN [A-Z ]*PRIVATE KEY|sk-(proj-)?[A-Za-z0-9]{16}|ghp_[A-Za-z0-9]{16}`)

// looksLikeSecret is a last-resort guard on the one object that egresses
// unconditionally on every session start.
//
// No code path assigns a credential to a posture field — the guarantee is
// structural, and this is not a licence to route secrets through here. But
// INV-1 is worth defending in depth at a boundary this exposed, so a
// secret-shaped value is dropped (the field then reads as unknown) rather
// than masked, because a mask still confirms a secret was present.
func looksLikeSecret(v string) bool { return secretShaped.MatchString(v) }

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
