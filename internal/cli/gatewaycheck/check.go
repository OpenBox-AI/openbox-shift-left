// Package gatewaycheck inspects the local gateway's posture for `openbox doctor`.
//
// This is the DETECTION TIER, which is the plan's base assurance claim in code
// form. It answers four questions and is careful about the difference between
// them:
//
//   - is the gateway alive?
//   - does the tool's active configuration actually point at it?
//   - who owns that configuration — the developer (base tier) or root (MDM tier)?
//   - is this machine bypass-capable?
//
// The last one is why this package exists, and why its wording matters more than
// its logic. Bypass here is VISIBLE and ATTRIBUTABLE, never prevented: a developer
// can unset one environment variable and their model calls go straight to the
// provider. Prevention belongs to the org's MDM. Any output implying otherwise
// would be the overstatement this product exists to prevent, so the strings below
// say "detectable", never "cannot".
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

// Tier is which assurance tier this machine has reached, inferred from who owns
// the configuration rather than from a flag OpenBox writes. A flag would be a
// claim; ownership is an observation.
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
	// SettingsPath is where that value came from. Empty when nothing sets it.
	SettingsPath string
	// TargetsGateway is whether the configured value points at loopback at all.
	// A settings file that points somewhere else is the interesting case: the
	// gateway can be alive and completely unused.
	TargetsGateway bool

	// Tier is inferred from SettingsPath's ownership.
	Tier Tier
	// OwnerUID is who owns the settings file, when one was found.
	OwnerUID int

	// BypassCapable is true whenever the configuration can be changed by the
	// developer — which under the base tier is ALWAYS. It is reported, not fixed.
	BypassCapable bool
	// BypassNotes are the specific, checkable reasons.
	BypassNotes []string

	// EnvValue is ANTHROPIC_BASE_URL as it stands in the INSPECTING process's
	// environment, empty when unset.
	//
	// PRECEDENCE, because the first version of this got it backwards: for Claude
	// Code the SETTINGS FILE WINS. Anthropic's own documentation is explicit —
	// "when both a shell export and a settings-file env block set the same
	// variable, the settings-file value applies"
	// (code.claude.com/docs/en/llm-gateway-connect#set-in-a-settings-file). So an
	// env value that disagrees with a configured file is NOT an override and must
	// not be reported as one; the file is what the tool uses. It matters only when
	// no settings file sets the variable at all, where the environment is then the
	// effective source.
	EnvValue string

	// EnvDiffersFromSettings is informational, not a fault: both are set and they
	// disagree, and the file is the one in force.
	EnvDiffersFromSettings bool
}

// envKey is the variable Claude Code reads to find its API base.
const envKey = "ANTHROPIC_BASE_URL"

// Inspect builds the report. homeDir and managedPath are injected so a test can
// point them at a temp dir rather than at the developer's real machine.
func Inspect(homeDir, managedPath string, dialTimeout time.Duration, getenv func(string) string) Report {
	r := Report{Tier: TierNone}

	// Injected rather than read directly so a test can drive both branches, and so
	// the caller decides whose environment is being described.
	if getenv != nil {
		r.EnvValue = strings.TrimSpace(getenv(envKey))
	}

	// Managed settings win, and their precedence is why they are checked first:
	// a root-owned managed file cannot be overridden by the user file below it.
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

	// Ownership decides the tier, not the file's location: an org can push the
	// same bytes to either path. A user-writable managed file is base tier
	// wearing an MDM path, and reporting it as MDM would overstate.
	// Ownership decides the tier. -1 means this OS exposed no owner to check
	// (Windows), and that is UNKNOWN rather than "not root": treating the two
	// alike printed "owned by uid -1, not root — the developer can rewrite it"
	// about a file that may be properly ACL-locked, which is a false claim in the
	// confident direction. Unknown ownership cannot confirm the MDM tier either,
	// so it reports base AND says why it could not tell.
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
		// EqualFold, not ==. DNS names are case-insensitive and the gateway's own
		// validator already treats them that way (gateway/config.go
		// isLoopbackSpelling), so `--gateway-addr LOCALHOST:8788` passed
		// validation, was written to the settings file, and then doctor reported
		// "target NOT loopback — this machine is pointed at something else" about a
		// correctly configured machine, alongside "reachable yes". This package's
		// own rule is that doctor must degrade to LESS information, never to a
		// wrong claim.
		if ip := net.ParseIP(host); ip != nil {
			r.TargetsGateway = ip.IsLoopback()
		} else {
			r.TargetsGateway = strings.EqualFold(host, "localhost")
		}
		// The port has to be DEFAULTED, not required. A configured value with no
		// explicit port ("https://api.anthropic.com") is perfectly valid — a real
		// client connects on 443 — but SplitHostPort errors on it, and so did the
		// dial. The result was Alive=false, which the report then explained as
		// "model calls will FAIL rather than escape, the safe direction" while
		// those calls were in fact succeeding, directly, completely ungoverned.
		// Exactly backwards, on the one machine state an operator most needs the
		// truth about.
		r.Alive, r.AliveErr = dial(net.JoinHostPort(host, port), dialTimeout)
	}

	// The env comparison comes last, because it is a statement ABOUT the
	// configured value. Normalised on both sides so a trailing slash is not
	// reported as a disagreement.
	if r.EnvValue != "" && r.ConfiguredAddr != "" &&
		!strings.EqualFold(strings.TrimRight(r.EnvValue, "/"), strings.TrimRight(r.ConfiguredAddr, "/")) {
		r.EnvDiffersFromSettings = true
	}

	r.BypassCapable, r.BypassNotes = bypassAssessment(r, r.BypassNotes)
	return r
}

// bypassAssessment names the specific ways this machine's model traffic could
// leave ungoverned. Every branch is phrased as detection.
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
		// Still not "cannot". A shell export beats a settings file for a
		// directly-launched process, and saying otherwise would be the exact
		// overstatement this package's doc comment warns about.
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

// readBaseURL pulls env.ANTHROPIC_BASE_URL out of a settings file. Any read or
// parse failure is "not configured here": doctor must degrade to less
// information, never to a wrong claim.
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

// ownerUID returns the file's owning uid, or -1 when it cannot be determined.
// Windows has no uid, so tier detection there degrades to "unknown owner" rather
// than silently reporting the MDM tier.
func ownerUID(path string) int {
	info, err := os.Stat(path)
	if err != nil {
		return -1
	}
	return statUID(info)
}

// dial reports whether something accepts a TCP connection.
//
// A connect, not a request: sending an HTTP request would mean deciding what a
// healthy answer looks like, and the gateway's whole job is to relay someone
// else's answers. Accepting a connection is the claim doctor can actually make.
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

// stripScheme turns "http://127.0.0.1:8788" into "127.0.0.1:8788".
func stripScheme(s string) string {
	return strings.TrimPrefix(strings.TrimPrefix(s, "https://"), "http://")
}

// statUID is implemented per-OS: unix reads the syscall stat, Windows cannot.
var statUID = func(fs.FileInfo) int { return -1 }

// hostPort splits a configured base URL into host and port, DEFAULTING the port
// from the scheme when none is given.
//
// This is the whole of finding #7: requiring an explicit port made a perfectly
// valid "https://api.anthropic.com" look unreachable, and the report then
// described a working, ungoverned configuration as failing safely.
func hostPort(configured string) (host, port string) {
	scheme := "http"
	if strings.HasPrefix(configured, "https://") {
		scheme = "https"
	}
	bare := stripScheme(configured)
	// Trim any path, so "https://host/v1" does not become part of the host.
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
