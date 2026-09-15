package evaluate

import (
	"fmt"
	"sort"
	"strings"
)

// buildSandboxPolicy renders the evaluation policy in the form the sandbox
// service validates: OpenShell's policy YAML, meeting that service's security
// floor exactly.
//
// This is not a reformat of buildPolicy. The floor is stricter than the CLI
// path's policy in three ways that change what the image may do, and each is
// enforced rather than negotiated:
//
//   - read_write is EXACTLY ["/sandbox"]. The CLI path granted /dev/null and
//     /tmp; here /tmp is pinned read-only, because a policy that declares
//     network access must not also hand the workload a writable temp directory
//     the proxy cannot see.
//   - run_as_user and run_as_group are the names "sandbox", not uid 1000. The
//     proto has always taken names; the CLI path's integer was never the wire
//     shape.
//   - landlock best_effort is admitted only when the deployment opted into
//     degraded landlock, and the run records that as a coverage limitation.
//
// The endpoints are the host-side relays the guest reaches through the
// gateway. Credentials are never written here: credential_binding names a
// provider and the gateway resolves it, so the evaluation key does not appear
// in the policy, the request, or the guest's environment.
func buildSandboxPolicy(applicationExecutable string, relayPort int, effectPorts ...int) []byte {
	var policy strings.Builder
	policy.WriteString("version: 1\n")
	policy.WriteString("filesystem_policy:\n")
	policy.WriteString("  include_workdir: false\n")
	policy.WriteString("  read_only:\n")
	// Every OpenShell proxy-mode baseline path is declared here, not because
	// the workload needs all of them, but because enrichment only agrees with
	// the service's local mirror when it has nothing left to add: upstream
	// skips baseline paths that do not exist in the guest, and the service
	// cannot see the guest filesystem to predict which. Declaring the full set
	// makes the enriched policy deterministic.
	//
	// /tmp is declared read-only because the floor requires it, though upstream
	// appends it to read-write regardless — its read-write pass checks only the
	// read-write list for an existing entry. The run records that as a coverage
	// limitation rather than claiming a writable /tmp was prevented.
	for _, path := range []string{"/app", "/dev/urandom", "/etc", "/lib", "/proc", "/tmp", "/usr", "/var/log"} {
		fmt.Fprintf(&policy, "    - %s\n", path)
	}
	policy.WriteString("  read_write:\n    - /sandbox\n")
	policy.WriteString("landlock:\n  compatibility: best_effort\n")
	policy.WriteString("process:\n  run_as_user: sandbox\n  run_as_group: sandbox\n")

	endpoints := map[string][]sandboxEndpoint{
		"openbox_core_relay": {{
			port:     relayPort,
			provider: OpenBoxProvider,
			rules: []sandboxRule{
				{method: "GET", path: "/api/v1/auth/validate"},
				{method: "POST", path: "/api/v1/governance/evaluate"},
				{method: "POST", path: "/api/v1/governance/approval"},
			},
		}},
	}
	if len(effectPorts) == 1 && effectPorts[0] > 0 {
		endpoints["safe_effect_sink"] = []sandboxEndpoint{{
			port:  effectPorts[0],
			rules: []sandboxRule{{method: "POST", path: "/effects/safe"}},
		}}
	}
	policy.WriteString("network_policies:\n")
	for _, name := range sortedKeys(endpoints) {
		fmt.Fprintf(&policy, "  %s:\n    name: %s\n", name, name)
		policy.WriteString("    binaries:\n")
		fmt.Fprintf(&policy, "      - path: %s\n", applicationExecutable)
		policy.WriteString("    endpoints:\n")
		for _, endpoint := range endpoints[name] {
			fmt.Fprintf(&policy, "      - host: host.openshell.internal\n        port: %d\n", endpoint.port)
			policy.WriteString("        protocol: rest\n        enforcement: enforce\n")
			policy.WriteString("        allowed_ips:\n          - 192.168.127.254/32\n")
			if endpoint.provider != "" {
				fmt.Fprintf(&policy, "        credential_binding:\n          provider: %s\n", endpoint.provider)
			}
			policy.WriteString("        rules:\n")
			for _, rule := range endpoint.rules {
				fmt.Fprintf(&policy, "          - allow:\n              method: %s\n              path: %s\n", rule.method, rule.path)
			}
		}
	}
	return []byte(policy.String())
}

type sandboxEndpoint struct {
	port     int
	provider string
	rules    []sandboxRule
}

type sandboxRule struct {
	method string
	path   string
}

func sortedKeys[V any](values map[string]V) []string {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
