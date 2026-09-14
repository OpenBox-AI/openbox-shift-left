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
//
// This reads the bound tool's store and must never gain a fallback to the
// org-level file or the legacy config dir. Falling back would let one tool's
// `init` decide it is already registered because a different tool is -- and
// then install hooks that sign as somebody else's agent. A machine that has
// not run `init` per tool governs nothing until it does, which is the accepted
// cost and a documented upgrade step.
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
// to " -- and the degraded form has to show the per-tool shape, or the one
// message a broken machine does print would send its reader to a file that
// holds the org token and no agent identity at all.
func credentialFileLabel() string {
	if p, err := devconfig.EnvFilePath(); err == nil && p != "" {
		return p
	}
	return "~/.openbox/<tool>/.env"
}

func didOrNone(did string) string {
	if did == "" {
		return fmt.Sprintf("no DID in this tool's dev.json; re-run init to register one, or export %s", devconfig.EnvDID)
	}
	return did
}
