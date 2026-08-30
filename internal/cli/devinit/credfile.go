package devinit

import (
	"fmt"
	"path/filepath"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
)

type localCredentials struct {
	apiKey     string
	privateKey string
}

// readLocalCredentials an unparseable file IS an error; silently treating it
// as "not registered" would make the caller register a second agent while the
// user's real credentials sat in a file with a typo.
func readLocalCredentials(override string) (localCredentials, error) {
	path, err := credentialPath(override)
	if err != nil {
		return localCredentials{}, err
	}
	kv, err := devconfig.ParseEnvFile(path)
	if err != nil {
		return localCredentials{}, err
	}
	return localCredentials{
		apiKey:     kv[devconfig.EnvAPIKeyDirect],
		privateKey: kv[devconfig.EnvAgentPrivateKey],
	}, nil
}

func writeLocalCredentials(override, apiKey, privateKey string) error {
	path, err := credentialPath(override)
	if err != nil {
		return err
	}
	return devconfig.WriteEnvFile(path, map[string]string{
		devconfig.EnvAPIKeyDirect:    apiKey,
		devconfig.EnvAgentPrivateKey: privateKey,
	})
}

// credentialFileLabel names the credential file for output. It degrades to the
// generic path rather than an empty string, so a message never reads "written
// to ".
func credentialFileLabel(override string) string {
	if p, err := credentialPath(override); err == nil && p != "" {
		return p
	}
	return "~/.openbox/.env"
}

func credentialPath(override string) (string, error) {
	if override == "" {
		return devconfig.EnvFilePath()
	}
	if !filepath.IsAbs(override) {
		return "", fmt.Errorf("--env-file must be an absolute path (got %q): a relative path resolves "+
			"against the current directory, which would write credentials into whatever project you are in", override)
	}
	return filepath.Clean(override), nil
}

func didOrNone(did string) string {
	if did == "" {
		return fmt.Sprintf("no DID in dev.json; run `openbox auth` to set one, or export %s", devconfig.EnvDID)
	}
	return did
}
