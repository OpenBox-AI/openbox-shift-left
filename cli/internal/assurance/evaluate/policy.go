package evaluate

import (
	"encoding/json"
	"path"
	"sort"
)

type policyDocument struct {
	Version          int                      `json:"version"`
	FilesystemPolicy filesystemPolicy         `json:"filesystem_policy"`
	Landlock         landlockPolicy           `json:"landlock"`
	Process          processPolicy            `json:"process"`
	NetworkPolicies  map[string]networkPolicy `json:"network_policies"`
}

type filesystemPolicy struct {
	IncludeWorkdir bool     `json:"include_workdir"`
	ReadOnly       []string `json:"read_only"`
	ReadWrite      []string `json:"read_write"`
}

type landlockPolicy struct {
	Compatibility string `json:"compatibility"`
}
type processPolicy struct {
	RunAsUser  int `json:"run_as_user"`
	RunAsGroup int `json:"run_as_group"`
}
type networkPolicy struct {
	Name      string           `json:"name"`
	Binaries  []policyBinary   `json:"binaries"`
	Endpoints []policyEndpoint `json:"endpoints"`
}
type policyBinary struct {
	Path string `json:"path"`
}
type policyEndpoint struct {
	Host              string             `json:"host"`
	Port              int                `json:"port"`
	Protocol          string             `json:"protocol"`
	AllowedIPs        []string           `json:"allowed_ips"`
	Enforcement       string             `json:"enforcement"`
	CredentialBinding *credentialBinding `json:"credential_binding,omitempty"`
	Rules             []policyRule       `json:"rules"`
}
type credentialBinding struct {
	Provider string `json:"provider"`
}
type policyRule struct {
	Allow policyAllow `json:"allow"`
}
type policyAllow struct {
	Method string `json:"method"`
	Path   string `json:"path"`
}

func buildPolicy(applicationRoot, applicationExecutable string, relayPort int, effectPorts ...int) ([]byte, error) {
	readOnly := []string{"/dev/urandom", "/etc", "/lib", "/lib64", "/proc", "/usr"}
	if applicationRoot != "" && applicationRoot != "/" {
		readOnly = append(readOnly, path.Clean(applicationRoot))
	}
	sort.Strings(readOnly)
	readOnly = compactStrings(readOnly)
	document := policyDocument{
		Version: 1,
		FilesystemPolicy: filesystemPolicy{
			IncludeWorkdir: false,
			ReadOnly:       readOnly,
			ReadWrite:      []string{"/dev/null", "/tmp"},
		},
		Landlock: landlockPolicy{Compatibility: "best_effort"},
		Process:  processPolicy{RunAsUser: 1000, RunAsGroup: 1000},
		NetworkPolicies: map[string]networkPolicy{
			"openbox_core_relay": {
				Name:     "openbox_core_relay",
				Binaries: []policyBinary{{Path: applicationExecutable}},
				Endpoints: []policyEndpoint{{
					Host: "host.openshell.internal", Port: relayPort,
					Protocol: "rest", AllowedIPs: []string{"192.168.127.254/32"},
					Enforcement:       "enforce",
					CredentialBinding: &credentialBinding{Provider: OpenBoxProvider},
					Rules: []policyRule{
						{Allow: policyAllow{Method: "GET", Path: "/api/v1/auth/validate"}},
						{Allow: policyAllow{Method: "POST", Path: "/api/v1/governance/evaluate"}},
						{Allow: policyAllow{Method: "POST", Path: "/api/v1/governance/approval"}},
					},
				}},
			},
		},
	}
	if len(effectPorts) == 1 && effectPorts[0] > 0 {
		document.NetworkPolicies["safe_effect_sink"] = networkPolicy{
			Name:     "safe_effect_sink",
			Binaries: []policyBinary{{Path: applicationExecutable}},
			Endpoints: []policyEndpoint{{
				Host: "host.openshell.internal", Port: effectPorts[0], Protocol: "rest",
				AllowedIPs: []string{"192.168.127.254/32"}, Enforcement: "enforce",
				Rules: []policyRule{{Allow: policyAllow{Method: "POST", Path: "/effects/safe"}}},
			}},
		}
	}
	return json.Marshal(document)
}

func compactStrings(values []string) []string {
	if len(values) == 0 {
		return values
	}
	result := values[:1]
	for _, value := range values[1:] {
		if value != result[len(result)-1] {
			result = append(result, value)
		}
	}
	return result
}
