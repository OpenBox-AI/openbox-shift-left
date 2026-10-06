package devinit

import (
	"fmt"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
)

type localCredentials struct {
	apiKey      string
	workloadKey string
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
		apiKey:      kv[devconfig.EnvAPIKeyDirect],
		workloadKey: kv[devconfig.EnvWorkloadPrivateKey],
	}, nil
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

func agentIDOrNone(agentID string) string {
	if agentID == "" {
		// Not "re-run init": this message prints from the reuse branch, so a
		// re-run reuses again and says the same thing. Deleting the credential
		// file is what sends the next run down the branch that can fix it.
		return fmt.Sprintf("no agent id in this tool's dev.json; delete %s and re-run init, or export %s",
			credentialFileLabel(), devconfig.EnvAgentID)
	}
	return agentID
}
