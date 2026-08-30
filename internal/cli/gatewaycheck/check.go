// Package gatewaycheck inspects the local gateway's posture for `openbox
// doctor`. Bypass here is visible and attributable, never prevented: a
// developer can unset one environment variable and their model calls go
// straight to the provider.
//   - Is the gateway alive?
//   - Does the tool's active configuration actually point at it?
//   - Who owns that configuration; the developer (base tier) or root (MDM
//     tier)?
package gatewaycheck

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Tier is which assurance tier this machine has reached, inferred from who
// owns the configuration rather than from a flag OpenBox writes.
type Tier string

const (
	// TierBase is user-owned configuration: tamper-evident, not tamper-resistant.
	TierBase Tier = "base"
	// TierMDM is root-owned configuration pushed by the org, which the developer
	// cannot rewrite without escalating.
	TierMDM Tier = "mdm"
	// TierNone is no gateway configuration found at all.
	TierNone Tier = "none"
)

// Report is what doctor prints.
type Report struct {
	// Alive is whether something accepts connections at ConfiguredAddr.
	Alive bool
	// AliveErr is why not, when it is not.
	AliveErr string

	// ConfiguredAddr is the address the tool's settings actually name, which is
	// not necessarily the address the gateway was installed on.
	ConfiguredAddr string
	// SettingsPath is where that value came from.
	SettingsPath string
	// TargetsGateway is whether the configured value points at loopback at all.
	TargetsGateway bool

	// Tier is inferred from SettingsPath's ownership.
	Tier Tier
	// OwnerUID is who owns the settings file, when one was found.
	OwnerUID int

	// BypassCapable is true whenever the configuration can be changed by the
	// developer; which under the base tier is always.
	BypassCapable bool
	// BypassNotes are the specific, checkable reasons.
	BypassNotes []string

	// EnvValue is ANTHROPIC_BASE_URL as it stands in the inspecting process's
	// environment, empty when unset. So an env value that disagrees with a
	// configured file is NOT an override and must not be reported as one; the
	// file is what the tool uses.
	EnvValue string

	// EnvDiffersFromSettings is informational, not a fault: both are set and they
	// disagree, and the file is the one in force.
	EnvDiffersFromSettings bool
}

const envKey = "ANTHROPIC_BASE_URL"

// Inspect builds the report. HomeDir and managedPath are injected so a test
// can point them at a temp dir rather than at the developer's real machine.
func Inspect(homeDir, managedPath string, dialTimeout time.Duration, getenv func(string) string) Report {
	r := Report{Tier: TierNone}

	if getenv != nil {
		r.EnvValue = strings.TrimSpace(getenv(envKey))
	}

	for _, candidate := range []struct {
		path string
		tier Tier
	}{
		{managedPath, TierMDM},
		{filepath.Join(homeDir, ".claude", "settings.json"), TierBase},
	} {
		if candidate.path == "" {
			continue
		}
		addr, ok := readBaseURL(candidate.path)
		if !ok {
			continue
		}
		r.SettingsPath = candidate.path
		r.ConfiguredAddr = addr
		r.Tier = candidate.tier
		r.OwnerUID = ownerUID(candidate.path)
		break
	}

	// Unknown ownership cannot confirm the MDM tier either, so it reports base
	// AND says why it could not tell.
	if r.SettingsPath != "" && r.Tier == TierMDM && r.OwnerUID != 0 {
		r.Tier = TierBase
		if r.OwnerUID < 0 {
			r.BypassNotes = append(r.BypassNotes,
				fmt.Sprintf("%s sits at the managed path, but this OS exposes no file owner to "+
					"check, so the MDM tier cannot be CONFIRMED here. It may well be locked down; "+
					"this build simply cannot see it. Verify ownership by hand.", r.SettingsPath))
		} else {
			r.BypassNotes = append(r.BypassNotes,
				fmt.Sprintf("%s sits at the managed path but is owned by uid %d, not root — "+
					"the developer can rewrite it, so this is the base tier, not the MDM tier", r.SettingsPath, r.OwnerUID))
		}
	}

	if r.ConfiguredAddr != "" {
		host, port := hostPort(r.ConfiguredAddr)
		// This package's own rule is that doctor must degrade to less information,
		// never to a wrong claim.
		if ip := net.ParseIP(host); ip != nil {
			r.TargetsGateway = ip.IsLoopback()
		} else {
			r.TargetsGateway = strings.EqualFold(host, "localhost")
		}
		r.Alive, r.AliveErr = dial(net.JoinHostPort(host, port), dialTimeout)
	}

	if r.EnvValue != "" && r.ConfiguredAddr != "" &&
		!strings.EqualFold(strings.TrimRight(r.EnvValue, "/"), strings.TrimRight(r.ConfiguredAddr, "/")) {
		r.EnvDiffersFromSettings = true
	}

	r.BypassCapable, r.BypassNotes = bypassAssessment(r, r.BypassNotes)
	return r
}

func bypassAssessment(r Report, notes []string) (bool, []string) {
	capable := false

	switch r.Tier {
	case TierNone:
		capable = true
		notes = append(notes, "no "+envKey+" is configured anywhere, so model calls go "+
			"straight to the provider and the gateway sees nothing. This is DETECTABLE, "+
			"which is the whole of the base claim: a session with model turns and no "+
			"gateway spans is what it looks like in stored data")
	case TierBase:
		capable = true
		notes = append(notes, "configuration is user-owned, so the developer can change or "+
			"remove it at any time. That is the base tier by design: bypass is DETECTABLE "+
			"and attributable, not prevented. Prevention needs the org's MDM")
	case TierMDM:
		capable = true
		notes = append(notes, "configuration is root-owned, which stops the developer "+
			"rewriting the file — but an environment variable set in a shell still takes "+
			"precedence for a process launched from it. Egress control is what closes that, "+
			"and it is the org's to deploy")
	}

	if r.ConfiguredAddr != "" && !r.TargetsGateway {
		notes = append(notes, envKey+" is set to "+r.ConfiguredAddr+", which is not loopback — "+
			"this machine is configured to talk to something other than its local gateway")
	}
	if r.ConfiguredAddr != "" && !r.Alive {
		notes = append(notes, "the configured gateway is not answering ("+r.AliveErr+"). Model "+
			"calls will FAIL rather than escape, which is the safe direction — but a developer "+
			"debugging that failure may work around it by unsetting "+envKey)
	}
	return capable, notes
}

// readBaseURL any read or parse failure is "not configured here": doctor must
// degrade to less information, never to a wrong claim.
func readBaseURL(path string) (string, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	var doc struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return "", false
	}
	v, ok := doc.Env[envKey]
	if !ok || v == "" {
		return "", false
	}
	return v, true
}

// ownerUID windows has no uid, so tier detection there degrades to "unknown
// owner" rather than silently reporting the MDM tier.
func ownerUID(path string) int {
	info, err := os.Stat(path)
	if err != nil {
		return -1
	}
	return statUID(info)
}

func dial(addr string, timeout time.Duration) (bool, string) {
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		var opErr *net.OpError
		if errors.As(err, &opErr) {
			return false, opErr.Err.Error()
		}
		return false, err.Error()
	}
	conn.Close()
	return true, ""
}

func stripScheme(s string) string {
	return strings.TrimPrefix(strings.TrimPrefix(s, "https://"), "http://")
}

var statUID = func(fs.FileInfo) int { return -1 }

func hostPort(configured string) (host, port string) {
	scheme := "http"
	if strings.HasPrefix(configured, "https://") {
		scheme = "https"
	}
	bare := stripScheme(configured)
	if i := strings.IndexByte(bare, '/'); i >= 0 {
		bare = bare[:i]
	}
	if h, p, err := net.SplitHostPort(bare); err == nil {
		return h, p
	}
	if scheme == "https" {
		return bare, "443"
	}
	return bare, "80"
}
