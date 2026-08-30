package main

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
)

// controlTokenProblem the runtime key is an output of `openbox init`, never an
// input to it.
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

func safePrefix(token string) string {
	if len(token) > 12 {
		return token[:12]
	}
	return token
}

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
// deployment, by comparing their registrable domain; so api.openbox.ai and
// core.openbox.ai match while openbox-api.example.com does not.
func sameDeployment(host, defaultHost string) bool {
	h, d := strings.ToLower(strings.TrimSpace(host)), strings.ToLower(defaultHost)
	if h == "" || d == "" {
		return false
	}
	return h == d || registrableDomain(h) == registrableDomain(d)
}

func registrableDomain(host string) string {
	parts := strings.Split(host, ".")
	if len(parts) < 2 {
		return host
	}
	return strings.Join(parts[len(parts)-2:], ".")
}
