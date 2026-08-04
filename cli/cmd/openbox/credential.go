package main

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/openbox-ai/openbox-shift-left/adapters/common/devconfig"
)

// Onboarding guards for the two mistakes people actually make.
//
// Both were found by installing on a clean machine and following the printed
// instructions: one produces a 401 from the control plane, the other a 401 from
// the data plane, and neither error says which of the several plausible causes it
// is. A governance tool whose first-run failure is indistinguishable from a
// broken install spends its credibility before it does anything.

// controlTokenProblem describes a credential that cannot work, in the terms the
// user has in front of them (what they copied, from where) rather than in terms
// of what the backend rejected.
//
// OpenBox has two kinds of key and the dashboard shows both:
//
//	obx_key_…   an ORGANIZATION key (Organization → API Keys) — this is the one
//	obx_… / obx_test_…   an AGENT RUNTIME key (Agent detail) — minted BY onboarding
//
// The runtime key is an output of `openbox init`, never an input to it. Sent as a
// control credential it takes the Bearer-JWT branch (client.go) and comes back a
// bare 401.
func controlTokenProblem(token string) string {
	t := strings.TrimSpace(token)
	switch {
	case t == "":
		return ""
	case strings.HasPrefix(t, "obx_key_"):
		return "" // an org key: the right kind
	case strings.HasPrefix(t, "obx_"):
		return fmt.Sprintf("%s looks like an AGENT RUNTIME key (%s…), not an organization credential.\n"+
			"  That key is what onboarding MINTS for the agent — it is never an input to it, and the\n"+
			"  control plane will reject it.\n"+
			"  Use an organization key instead: dashboard → Organization → API Keys (starts obx_key_),\n"+
			"  with create:agent + read:agent to onboard. A Keycloak JWT from your dashboard session\n"+
			"  also works. See docs/getting-started.md § Get the right credential.",
			devconfig.EnvControlToken, safePrefix(t))
	case strings.Count(t, ".") == 2:
		return "" // a JWT: plausible
	default:
		return fmt.Sprintf("%s does not look like either an organization key (obx_key_…) or a Keycloak JWT.\n"+
			"  See docs/getting-started.md § Get the right credential.", devconfig.EnvControlToken)
	}
}

// safePrefix shows just enough of a credential to recognise which one it is —
// the public prefix, never the secret body (INV-1).
func safePrefix(token string) string {
	if len(token) > 12 {
		return token[:12]
	}
	return token
}

// selfHostedWithoutDataPlane reports whether this install is pointed at a
// self-hosted control plane while leaving the data plane at its default — which
// is a hosted URL that self-hosted deployment cannot serve.
//
// It is the highest-cost silent mistake in onboarding: registration succeeds, the
// config looks right, and then every signed request goes somewhere else, which
// surfaces as `dev verify` → 401 "identity rejected". The backend's registration
// reply carries no data-plane URL, so nothing but the operator can supply it.
func selfHostedWithoutDataPlane(backendURL, baseURL string) bool {
	if strings.TrimSpace(baseURL) != "" {
		return false
	}
	u, err := url.Parse(strings.TrimSpace(backendURL))
	if err != nil || u.Host == "" {
		return false
	}
	return isPrivateHost(u.Hostname())
}

// isPrivateHost is a deliberately shallow check: localhost, RFC1918-looking
// addresses, and single-label or .local/.internal names. It only decides whether
// to print a warning, so a false negative costs a warning and a false positive
// costs one line of noise.
func isPrivateHost(host string) bool {
	h := strings.ToLower(host)
	switch {
	case h == "localhost" || h == "127.0.0.1" || h == "::1" || h == "0.0.0.0":
		return true
	case strings.HasPrefix(h, "10.") || strings.HasPrefix(h, "192.168."):
		return true
	case strings.HasPrefix(h, "172."):
		// 172.16.0.0/12 — close enough for a warning.
		var second int
		if _, err := fmt.Sscanf(h, "172.%d.", &second); err == nil && second >= 16 && second <= 31 {
			return true
		}
		return false
	case strings.HasSuffix(h, ".local") || strings.HasSuffix(h, ".internal") || strings.HasSuffix(h, ".lan"):
		return true
	case !strings.Contains(h, "."):
		return true // a single-label host is somebody's internal name
	}
	return false
}
