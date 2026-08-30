package devconfig

import "regexp"

// Posture is the effective posture of one session.
type Posture struct {
	// Enforce resolved enforcement/privacy posture.
	Enforce         bool
	FailClosed      bool
	Tier2           bool
	SecretDetection bool
	ContentCapture  bool
	Findings        bool
	Finops          bool
	Telemetry       bool
	// RealtimeFlush reports whether telemetry is delivered mid-session (debounced
	// background flush) or batched to session end.
	RealtimeFlush bool

	// Adapter-supplied: which adapter, and which provider build it drove.
	Adapter         string
	AdapterVersion  string
	ProviderVersion string

	// DecisionAuthority names what decides this session's gated tool calls:
	// "control_plane" since that decision, when /evaluate answers. It replaces
	// the bundle coordinates above as the posture's policy-provenance evidence,
	// and it deliberately answers a smaller question.
	DecisionAuthority string
	// FailurePolicy is what governs a gated call when the control plane cannot be
	// reached: "fail_open" (the default; the call proceeds ungoverned) or
	// "fail_closed" (it is denied).
	FailurePolicy string
	// ConfigSource names where each posture flag came from (E8-S9): default,
	// user, env, managed_default, or managed.
	ConfigSource map[string]string
	// ProviderManaged reports whether the provider's own managed configuration is
	// deployed (E8-S8): "true", "false", or "unknown" when it cannot be
	// determined. A string rather than a bool so unknown is not silently false.
	ProviderManaged string
}

// EffectivePosture resolves the posture fields that come from config and env,
// using the same resolvers the runtime itself calls; so the recorded posture
// cannot drift from the behaviour it describes.
func EffectivePosture() Posture {
	p := Posture{ConfigSource: map[string]string{}}
	for _, f := range postureFields() {
		v, src := resolveBoolWithSource(f.name, f.field, f.def, f.env)
		*f.into(&p) = v
		p.ConfigSource[f.name] = string(src)
	}
	warnDeprecatedKeys()

	p.DecisionAuthority = DecisionAuthorityControlPlane
	p.FailurePolicy = FailurePolicyFailOpen
	if p.FailClosed {
		p.FailurePolicy = FailurePolicyFailClosed
	}
	return p
}

const (
	DecisionAuthorityControlPlane = "control_plane"
	FailurePolicyFailOpen         = "fail_open"
	FailurePolicyFailClosed       = "fail_closed"
)

// Flags renders the resolved boolean posture, keyed by the same names
// ConfigSource uses and built from the same field table. It exists so that
// reporting a posture cannot drift from resolving one.
func (p Posture) Flags() map[string]bool {
	fields := postureFields()
	m := make(map[string]bool, len(fields))
	for _, f := range fields {
		m[f.name] = *f.into(&p)
	}
	return m
}

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
		{"telemetry", func(c DevConfig) *bool { return c.Telemetry }, true, EnvTelemetry,
			func(p *Posture) *bool { return &p.Telemetry }},
		{"finops", func(c DevConfig) *bool { return c.Finops }, true, EnvFinops,
			func(p *Posture) *bool { return &p.Finops }},
		{"realtime_flush", func(c DevConfig) *bool { return c.RealtimeFlush }, true, EnvRealtime,
			func(p *Posture) *bool { return &p.RealtimeFlush }},
	}
}

// Metadata renders the posture for a session-start event's metadata under a
// single "posture" key. The strings are omitted when empty, so a field the
// adapter cannot determine reads as unknown rather than as a false claim.
func (p Posture) Metadata() map[string]any {
	m := make(map[string]any, len(postureFields()))
	for k, v := range p.Flags() {
		m[k] = v
	}
	for k, v := range map[string]string{
		"adapter":          p.Adapter,
		"adapter_version":  p.AdapterVersion,
		"provider_version": p.ProviderVersion,
		// They share this map deliberately, to inherit its looksLikeSecret/truncate
		// guards rather than growing a second path.
		"decision_authority": p.DecisionAuthority,
		"failure_policy":     p.FailurePolicy,
		"provider_managed":   p.ProviderManaged,
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

const maxPostureValueLen = 256

var secretShaped = regexp.MustCompile(`obx_live_|obx_test_|obx_key_|BEGIN [A-Z ]*PRIVATE KEY|sk-(proj-)?[A-Za-z0-9]{16}|ghp_[A-Za-z0-9]{16}`)

func looksLikeSecret(v string) bool { return secretShaped.MatchString(v) }

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
