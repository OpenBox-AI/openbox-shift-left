package devinit

import (
	"fmt"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
)

type localCredentials struct {
	apiKey     string
	privateKey string
}

// readLocalCredentials an unparseable file IS an error; silently treating it
// as "not registered" would make the caller register a second agent while the
// user's real credentials sat in a file with a typo.
func readLocalCredentials() (localCredentials, error) {
	path, err := devconfig.EnvFilePath()
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

func writeLocalCredentials(apiKey, privateKey string) error {
	path, err := devconfig.EnvFilePath()
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
func credentialFileLabel() string {
	if p, err := devconfig.EnvFilePath(); err == nil && p != "" {
		return p
	}
	return "~/.openbox/.env"
}

func didOrNone(did string) string {
	if did == "" {
		return fmt.Sprintf("no DID in dev.json; run `openbox auth` to set one, or export %s", devconfig.EnvDID)
	}
	return did
}
