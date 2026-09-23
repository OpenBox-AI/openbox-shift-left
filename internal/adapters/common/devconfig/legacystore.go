package devconfig

// LegacyStore reports whether one tool's identity store predates the v3
// keycloak_workload model. Legacy iff dev.json holds a stored developer_did
// (D1: the attribution label is derived at read time, so dev.json never
// stores one under a v3 identity) or the credential file holds an Ed25519
// signing seed under the current or a deprecated name. Reasons names every
// signal that fired, for doctor and the hook warning to print verbatim.
type LegacyStore struct {
	Legacy  bool
	Reasons []string
}

// LegacyStoreFor inspects one tool's dev.json and .env for legacy markers.
// Files only, never the environment: an exported OPENBOX_AGENT_DID or seed
// says nothing about what is actually on disk, and the question this answers
// is about the store, not the running process's environment.
func LegacyStoreFor(tool string) (LegacyStore, error) {
	cfgPath, err := DevConfigPathFor(tool)
	if err != nil {
		return LegacyStore{}, err
	}
	cfg, err := Load(cfgPath)
	if err != nil {
		return LegacyStore{}, err
	}

	envPath, err := EnvFilePathFor(tool)
	if err != nil {
		return LegacyStore{}, err
	}
	secrets, err := ParseEnvFile(envPath)
	if err != nil {
		return LegacyStore{}, err
	}

	return legacyStoreFromValues(cfg, secrets), nil
}

// legacyStoreFromValues is LegacyStoreFor's detection, applied to a dev config
// and secret map the caller already loaded. resolveCredentialsFrom shares this
// so the reuse gate (LegacyStoreFor) and the runtime resolver can never
// disagree about what counts as legacy.
func legacyStoreFromValues(cfg DevConfig, secrets map[string]string) LegacyStore {
	var ls LegacyStore
	if cfg.DID != "" {
		ls.Legacy = true
		ls.Reasons = append(ls.Reasons, "dev.json holds a stored developer_did")
	}
	for _, name := range legacySeedEnvNames {
		if secrets[name] == "" {
			continue
		}
		ls.Legacy = true
		if name == EnvAgentPrivateKey {
			ls.Reasons = append(ls.Reasons, "the credential file holds "+name)
		} else {
			ls.Reasons = append(ls.Reasons, "the credential file holds the deprecated "+name)
		}
	}
	return ls
}
