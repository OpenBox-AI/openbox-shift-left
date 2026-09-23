package devconfig

import "os"

// WorkloadIdentity is what one successful v3 registration returns: the agent
// id and the two secrets a keycloak_workload agent authenticates with.
type WorkloadIdentity struct {
	AgentID       string
	APIKey        string
	PrivateKeyB64 string
}

// WriteWorkloadIdentity commits a v3 keycloak_workload identity to one tool's
// store, replacing whatever was there before (legacy or v3), in this order:
//
//  1. dev.json: agent_id, identity_method, with any legacy developer_did
//     cleared. Written first because an interrupted write here leaves a
//     coordinate without credentials, which the hook names and the next init
//     repairs; the reverse order would leave credentials with no coordinate to
//     attribute them to (cmd/openbox/adopt.go:73-77).
//  2. .env: OPENBOX_API_KEY and OPENBOX_WORKLOAD_PRIVATE_KEY set; the legacy
//     signing-key names (current and deprecated) removed outright, so a
//     re-init never leaves a v1 seed sitting beside the v3 key.
//  3. The token cache deleted, so a bearer minted for the identity this call
//     just replaced can never be reused under the new one.
func WriteWorkloadIdentity(tool string, id WorkloadIdentity) error {
	cfgPath, err := DevConfigWritePathFor(tool)
	if err != nil {
		return err
	}
	if err := WriteConfig(cfgPath, Update{
		AgentID:        id.AgentID,
		IdentityMethod: IdentityMethodKeycloakWorkload,
	}); err != nil {
		return err
	}

	envPath, err := EnvFilePathFor(tool)
	if err != nil {
		return err
	}
	remove := append([]string{EnvAgentPrivateKey}, deprecatedPrivateKeyEnvNames...)
	if err := WriteEnvFile(envPath, map[string]string{
		EnvAPIKeyDirect:       id.APIKey,
		EnvWorkloadPrivateKey: id.PrivateKeyB64,
	}, remove...); err != nil {
		return err
	}

	cachePath, err := WorkloadTokenCachePathFor(tool)
	if err != nil {
		return err
	}
	if err := os.Remove(cachePath); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
