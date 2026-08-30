package devconfig

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Roles — which identity a config file belongs to.
//
// One machine can hold two unrelated identities: the DEVELOPER runtime whose
// sessions are governed, and an APPROVER that decides other people's requests.
// They must not share a file. The developer config is read by every hook on the
// tool hot path; the approver config carries a different principal's settings
// and is read only by `openbox approve`.
//
// The separation is structural rather than conventional: the hook path calls
// DefaultConfigPath and has no way to name the approver file (guarded by
// TestAdaptersNeverReadApproverConfig). Role is chosen once, at install time,
// so nothing on the hot path ever has to decide which identity it is running as.
type Role string

const (
	// RoleDev is the default: the governed developer runtime (dev.json).
	RoleDev Role = "dev"
	// RoleApprover is the approval-queue client (approver.json). It is a
	// credentialed client, not a registered agent — it has no DID and no
	// runtime key of its own (OD-T-3).
	RoleApprover Role = "approver"
)

// EnvApproverConfigPath relocates the approver config, mirroring
// EnvConfigPath's role for the developer one.
const EnvApproverConfigPath = "OPENBOX_APPROVER_CONFIG"

// ParseRole validates a --role value. An empty value is the developer role, so
// every existing invocation keeps its meaning.
func ParseRole(s string) (Role, error) {
	switch Role(s) {
	case "", RoleDev:
		return RoleDev, nil
	case RoleApprover:
		return RoleApprover, nil
	default:
		return "", fmt.Errorf("unknown --role %q (want %q or %q)", s, RoleDev, RoleApprover)
	}
}

// ConfigPathFor is where a role's config lives.
func ConfigPathFor(r Role) string {
	if r == RoleApprover {
		return DefaultApproverConfigPath()
	}
	return DefaultConfigPath()
}

// DefaultApproverConfigPath is ~/.openbox/approver.json (ADR-0015), with the
// same read-side legacy fallback DefaultConfigPath has.
func DefaultApproverConfigPath() string {
	p, err := ApproverConfigPath()
	if err != nil {
		return filepath.Join(legacyConfigDir(), "approver.json")
	}
	return p
}

// ApproverConfig is the non-secret half of an approver install: where the queue
// is, and how this approver is meant to work it.
//
// The control token itself lives in ~/.openbox/.env under OPENBOX_CONTROL_TOKEN
// (ADR-0015), not here — this file stays credential-free. On an approver install
// that token is an org key with fleet-wide create/rotate authority, which is why
// ADR-0015 names it separately from the agent signing key.
type ApproverConfig struct {
	// BackendURL and OrgID name the queue to read.
	BackendURL string `json:"backend_url,omitempty"`
	OrgID      string `json:"org_id,omitempty"`

	// Host is what evaluates a request when the approver runs unattended:
	// "claude-code", "codex", … Empty means no autonomous evaluation — the
	// approver is a queue client for a human.
	Host string `json:"host,omitempty"`

	// Envelope is the policy bundle bounding what the host may decide. The
	// host may only ever NARROW it: an approve outside the envelope is
	// refused no matter who proposed it.
	Envelope string `json:"envelope,omitempty"`

	// Shadow decides nothing and records what it would have decided. It is
	// the default, and the only honest way to learn an envelope's
	// auto-approval rate before giving it authority.
	Shadow bool `json:"shadow"`

	// Operating bounds for unattended runs.
	PollIntervalMS int `json:"poll_interval_ms,omitempty"`
	HostTimeoutMS  int `json:"host_timeout_ms,omitempty"`
	MaxAutoPerHour int `json:"max_auto_per_hour,omitempty"`
}

// LoadApprover reads the approver config. A missing file is not an error — the
// caller may have supplied everything through the environment.
func LoadApprover(path string) (ApproverConfig, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ApproverConfig{}, nil
		}
		return ApproverConfig{}, fmt.Errorf("read approver config: %w", err)
	}
	var c ApproverConfig
	if err := json.Unmarshal(raw, &c); err != nil {
		return ApproverConfig{}, fmt.Errorf("parse approver config %s: %w", path, err)
	}
	return c, nil
}

// WriteApprover writes the approver config 0600 — it names which queue a
// principal works, so it is not world-readable even though it holds no secret.
func WriteApprover(path string, c ApproverConfig) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("approver config mkdir: %w", err)
	}
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal approver config: %w", err)
	}
	return os.WriteFile(path, append(raw, '\n'), 0o600)
}

// BaseURLLabel renders a resolved data-plane base for an install plan.
//
// It exists because an EMPTY base URL is the setting most likely to be wrong
// without looking wrong: the install succeeds, then every signed request goes
// to the SaaS core. Naming the default in the plan is the cheapest way to make
// a self-hosted operator notice before they onboard.
func BaseURLLabel(baseURL string) string {
	if baseURL == "" {
		return DefaultBaseURL + "  (default — the SaaS core; pass --base-url for a self-hosted one)"
	}
	return baseURL
}
