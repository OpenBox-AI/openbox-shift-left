package devinit

import (
	"fmt"
	"path/filepath"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
)

// credfile.go — reading and writing the two secrets in ~/.openbox/.env.
//
// This replaced an injectable secret.Store seam. The seam existed because the
// OS keychain could not be exercised in a test; a plaintext file in a temp
// directory can, so tests point OPENBOX_HOME at t.TempDir() and drive the real
// code path (ADR-0015). An interface over two function calls would be
// indirection with nothing behind it.

// localCredentials is the pair `agent/create` reveals exactly once.
type localCredentials struct {
	apiKey     string
	privateKey string
}

// readLocalCredentials reads the two secrets from the credential file.
//
// A missing file yields a zero value and no error: "not registered yet" is the
// normal first-run state, not a failure. An unparseable file IS an error —
// silently treating it as "not registered" would make the caller register a
// second agent while the user's real credentials sat in a file with a typo.
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

// writeLocalCredentials writes both secrets in one atomic write, under the
// platform's documented variable names, preserving any other key in the file.
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
// generic path rather than an empty string, so a message never reads
// "written to ".
func credentialFileLabel(override string) string {
	if p, err := credentialPath(override); err == nil && p != "" {
		return p
	}
	return "~/.openbox/.env"
}

// credentialPath resolves where credentials go: the caller's override when given,
// else ~/.openbox/.env.
//
// A relative override is REFUSED, for the same reason OPENBOX_HOME is
// (devconfig.Home): it would resolve against the process working directory, so
// `--env-file creds.env` run inside a repo drops a plaintext API key and Ed25519
// seed into the source tree, where only a pre-existing gitignore entry stands
// between it and `git add -A`.
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

// didOrNone renders a DID for output, naming the gap when there is none.
//
// A reuse-path install with credentials but no DID in dev.json is a real state
// (credentials copied to a new machine, say), and printing an empty string there
// reads as a rendering bug rather than as the missing coordinate it is.
func didOrNone(did string) string {
	if did == "" {
		return fmt.Sprintf("no DID in dev.json — run `openbox auth` to set one, or export %s", devconfig.EnvDID)
	}
	return did
}
