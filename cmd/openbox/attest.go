package main

import (
	"os"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	obgit "github.com/openbox-ai/openbox-shift-left/internal/adapters/common/git"
)

func attestContext() (obgit.AttestContext, bool) {
	creds, err := devconfig.ResolveCredentials()
	if err != nil || creds.DID == "" || creds.PrivateKeyB64 == "" {
		return obgit.AttestContext{}, false
	}

	ctx := obgit.AttestContext{
		DID:           creds.DID,
		PrivateKeyB64: creds.PrivateKeyB64,
		Adapter:       "openbox-cli",
		ThreadID:      os.Getenv(obgit.EnvCodexThreadID),
	}

	return ctx, true
}
