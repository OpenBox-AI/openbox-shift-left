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
// It keys on the DEFAULT data plane, not on what a self-hosted host looks like.
// The earlier version asked "does the backend look private?" — localhost,
// RFC1918, .local/.internal, single-label — which silently passed the most
// ordinary self-hosted deployment of all: a public domain. A control plane at
// openbox-api.example.com is not private by any of those tests, so the warning
// never fired and the install proceeded to sign every request against
// core.openbox.ai.
//
// The question that actually matters is simpler and has no false-negative shape:
// the data plane is about to default to DefaultBaseURL, so unless the control
// plane belongs to that same deployment, it is the wrong data plane. Anything
// not on the default core's domain warns — public, private or loopback alike.
func selfHostedWithoutDataPlane(backendURL, baseURL string) bool {
	if strings.TrimSpace(baseURL) != "" {
		return false
	}
	u, err := url.Parse(strings.TrimSpace(backendURL))
	if err != nil || u.Host == "" {
		return false
	}
	def, err := url.Parse(devconfig.DefaultBaseURL)
	if err != nil {
		return false
	}
	return !sameDeployment(u.Hostname(), def.Hostname())
}

// sameDeployment reports whether two hosts plausibly belong to one OpenBox
// deployment, by comparing their registrable domain — so api.openbox.ai and
// core.openbox.ai match while openbox-api.example.com does not.
//
// It is deliberately shallow: it decides whether to print a warning, so being
// wrong costs one line of output either way. It does not consult the public
// suffix list, which would make a co.uk-style hosted domain compare one label
// too short — that direction only ever produces a warning that is already
// correct advice.
func sameDeployment(host, defaultHost string) bool {
	h, d := strings.ToLower(strings.TrimSpace(host)), strings.ToLower(defaultHost)
	if h == "" || d == "" {
		return false
	}
	return h == d || registrableDomain(h) == registrableDomain(d)
}

// registrableDomain returns the last two dot-separated labels of a host, or the
// host itself when it has fewer. An IP address or a single-label name therefore
// compares as itself, which is what we want: neither is the hosted deployment.
func registrableDomain(host string) string {
	parts := strings.Split(host, ".")
	if len(parts) < 2 {
		return host
	}
	return strings.Join(parts[len(parts)-2:], ".")
}
