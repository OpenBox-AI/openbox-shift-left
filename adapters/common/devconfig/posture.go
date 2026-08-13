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
	// RealtimeFlush reports whether telemetry is delivered mid-session
	// (debounced background flush) or batched to session end.
	RealtimeFlush bool
	// RequireVerifiedBundle refuses an unverified policy bundle (OD-RF-3).
	RequireVerifiedBundle bool

	// Adapter-supplied. Bundle* are opaque staleness/integrity coordinates
	// (a policy id, an opaque version, a content hash) — never policy text.
	Adapter         string
	AdapterVersion  string
	ProviderVersion string
	BundleVersion   string
	BundlePolicyID  string
	BundleSHA256    string
	Staleness       Staleness
	// BundleIntegrity is the signature-verification outcome for the local
	// policy bundle (E8-S6): unsigned / verified / no_key / bad_signature /
	// expired / epoch_rollback / malformed. "unsigned" is the compatibility
	// state, not a failure — but it is also not assurance, which is why it is
	// recorded rather than assumed.
	BundleIntegrity string

	// DecisionAuthority names what decides this session's gated tool calls:
	// "control_plane" since ADR-0017, when /evaluate answers.
	//
	// It replaces the bundle coordinates above as the posture's policy-provenance
	// evidence, and it deliberately answers a smaller question. The bundle fields
	// existed because the ENDPOINT was the decider, so only the endpoint knew
	// which policy it had enforced — the control plane had to be told. That is no
	// longer true: the control plane decides, so it already holds the identity of
	// the policy it applied, and asking the endpoint to report it back would be
	// asking a party to attest to someone else's record.
	//
	// What does NOT close itself is the fail-open case, which is why
	// FailurePolicy sits beside this: a call decided locally because /evaluate
	// was unreachable is decided by NO policy, and core may have no record of it
	// at all. That gap is the thing worth reporting.
	//
	// Never described as "verified" or as an integrity claim. It is a statement
	// about who decides, carried over an authenticated channel — not a signature
	// check, which is what the word integrity meant here before and no longer
	// happens.
	DecisionAuthority string
	// FailurePolicy is what governs a gated call when the control plane cannot
	// be reached: "fail_open" (the default — the call proceeds ungoverned) or
	// "fail_closed" (it is denied). Under fail_open this field is the honest
	// statement that enforcement is reachability-dependent.
	FailurePolicy string
	// ConfigSource names where each posture flag came from (E8-S9): default,
	// user, env, managed_default, or managed. This is what lets the control
	// plane distinguish "the org requires enforce" from "this developer happens
	// to have it on" — without it, a machine with no managed config looks
	// identical to a compliant one.
	ConfigSource map[string]string
	// ProviderManaged reports whether the provider's own managed configuration
	// is deployed (E8-S8): "true", "false", or "unknown" when it cannot be
	// determined. A string rather than a bool so unknown is not silently false.
	ProviderManaged string
}

// EffectivePosture resolves the posture fields that come from config and env,
// using the same resolvers the runtime itself calls — so the recorded posture
// cannot drift from the behaviour it describes. The adapter fills in the rest.
func EffectivePosture() Posture {
	p := Posture{ConfigSource: map[string]string{}}
	// Resolve value and provenance together so the two cannot disagree.
	for _, f := range postureFields() {
		v, src := resolveBoolWithSource(f.name, f.field, f.def, f.env)
		*f.into(&p) = v
		p.ConfigSource[f.name] = string(src)
	}
	// Who decides, and what happens when it cannot be reached. Both are
	// knowable at session start, which the deciding policy id is not — posture
	// rides SessionStarted, before this session has made any decision, so a
	// policy id here could only ever be some other session's.
	p.DecisionAuthority = DecisionAuthorityControlPlane
	p.FailurePolicy = FailurePolicyFailOpen
	if p.FailClosed {
		p.FailurePolicy = FailurePolicyFailClosed
	}
	return p
}

// The vocabulary for the two provenance fields, so reporting sites cannot
// invent their own spelling of the same state.
const (
	DecisionAuthorityControlPlane = "control_plane"
	FailurePolicyFailOpen         = "fail_open"
	FailurePolicyFailClosed       = "fail_closed"
)

// Flags renders the resolved boolean posture, keyed by the same names
// ConfigSource uses and built from the same field table.
//
// It exists so that reporting a posture cannot drift from resolving one.
// `require_verified_bundle` shipped as a real control and was invisible in both
// the posture and `openbox doctor` for exactly this reason: the reporting sites
// each held their own hand-written list, so an org could not confirm from the
// endpoint that its own control was on.
func (p Posture) Flags() map[string]bool {
	fields := postureFields()
	m := make(map[string]bool, len(fields))
	for _, f := range fields {
		m[f.name] = *f.into(&p)
	}
	return m
}

// postureFields is the single list the posture and its provenance are built
// from, so a new flag cannot be reported without its source (or vice versa).
func postureFields() []struct {
	name  string
	field func(DevConfig) *bool
	def   bool
	env   string
	into  func(*Posture) *bool
} {
	return []struct {
		name  string
		field func(DevConfig) *bool
		def   bool
		env   string
		into  func(*Posture) *bool
	}{
		{"enforce", func(c DevConfig) *bool { return c.Enforce }, true, EnvEnforce,
			func(p *Posture) *bool { return &p.Enforce }},
		{"fail_closed", func(c DevConfig) *bool { b := c.FailClosed; return &b }, false, EnvFailClosed,
			func(p *Posture) *bool { return &p.FailClosed }},
		{"tier2", func(c DevConfig) *bool { return c.Tier2 }, false, EnvTier2,
			func(p *Posture) *bool { return &p.Tier2 }},
		{"secret_detection", func(c DevConfig) *bool { return c.SecretDetection }, true, EnvSecretDetection,
			func(p *Posture) *bool { return &p.SecretDetection }},
		{"content_capture", func(c DevConfig) *bool { return c.ContentCapture }, true, EnvContentCapture,
			func(p *Posture) *bool { return &p.ContentCapture }},
		{"findings", func(c DevConfig) *bool { return c.Findings }, false, EnvFindings,
			func(p *Posture) *bool { return &p.Findings }},
		// OD-RF-3. Reported because a control an org cannot verify from the
		// endpoint is not assurance — it is a setting the org hopes is on.
		{"require_verified_bundle", func(c DevConfig) *bool { return c.RequireVerifiedBundle }, false, EnvRequireVerified,
			func(p *Posture) *bool { return &p.RequireVerifiedBundle }},
		// Reported because it is an EGRESS posture, and default-on since ADR-0014:
		// with it on, four token counts and a model id leave the machine per turn.
		// The posture record is what lets an auditor tell, after the fact, whether a
		// given session captured — which is what makes the default defensible.
		// Pass-through (not `&b`) so an absent field resolves to the default; see
		// DevConfig.Finops for why the plain-bool version made the flip a no-op.
		{"finops", func(c DevConfig) *bool { return c.Finops }, true, EnvFinops,
			func(p *Posture) *bool { return &p.Finops }},
		// Reported because it decides WHEN a session's evidence exists at all:
		// off, nothing about a running session is queryable until it ends, so a
		// fleet that looks silent is indistinguishable from one that is batching.
		{"realtime_flush", func(c DevConfig) *bool { return c.RealtimeFlush }, true, EnvRealtime,
			func(p *Posture) *bool { return &p.RealtimeFlush }},
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
	m := make(map[string]any, len(postureFields()))
	for k, v := range p.Flags() {
		m[k] = v
	}
	for k, v := range map[string]string{
		"adapter":          p.Adapter,
		"adapter_version":  p.AdapterVersion,
		"provider_version": p.ProviderVersion,
		"bundle_version":   p.BundleVersion,
		"bundle_policy_id": p.BundlePolicyID,
		"bundle_sha256":    p.BundleSHA256,
		"staleness":        string(p.Staleness),
		"bundle_integrity": p.BundleIntegrity,
		"provider_managed": p.ProviderManaged,
	} {
		if v == "" || looksLikeSecret(v) {
			continue // unknown, or refused — see looksLikeSecret
		}
		m[k] = truncate(v, maxPostureValueLen)
	}
	if len(p.ConfigSource) > 0 {
		src := make(map[string]any, len(p.ConfigSource))
		for k, v := range p.ConfigSource {
			src[k] = v
		}
		m["config_source"] = src
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
