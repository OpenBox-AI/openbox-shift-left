package evaluate

import (
	"fmt"
	"sort"
	"strings"
)

// buildSandboxPolicy renders the evaluation policy as OpenShell YAML meeting
// the sandbox service's security floor: read_write exactly ["/sandbox"],
// run_as_user/group by NAME not uid, landlock best_effort only where the
// deployment opted in.
//
// Credentials are never written here. credential_binding names a provider and
// the gateway resolves it, so the key appears in neither policy nor request.
// The Core relay is the only endpoint: the guest can reach only the host
// gateway, so a Core on the host's loopback is unreachable without it.
func buildSandboxPolicy(applicationExecutable, openBoxProvider string, relayPort int) []byte {
	var policy strings.Builder
	policy.WriteString("version: 1\n")
	policy.WriteString("filesystem_policy:\n")
	policy.WriteString("  include_workdir: false\n")
	policy.WriteString("  read_only:\n")
	// The full baseline set, because enrichment only agrees with the service's
	// mirror when it has nothing left to add — upstream skips baseline paths
	// absent from the guest, which the service cannot see to predict.
	//
	// /tmp is declared read-only for the floor, though upstream appends it to
	// read-write anyway.
	for _, path := range []string{"/app", "/dev/urandom", "/etc", "/lib", "/proc", "/tmp", "/usr", "/var/log"} {
		fmt.Fprintf(&policy, "    - %s\n", path)
	}
	policy.WriteString("  read_write:\n    - /sandbox\n")
	policy.WriteString("landlock:\n  compatibility: best_effort\n")
	policy.WriteString("process:\n  run_as_user: sandbox\n  run_as_group: sandbox\n")

	endpoints := map[string][]sandboxEndpoint{
		"openbox_core_relay": {{
			port:     relayPort,
			provider: openBoxProvider,
			rules: []sandboxRule{
				{method: "GET", path: "/api/v1/auth/validate"},
				{method: "POST", path: "/api/v1/governance/evaluate"},
				{method: "POST", path: "/api/v1/governance/approval"},
			},
		}},
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
