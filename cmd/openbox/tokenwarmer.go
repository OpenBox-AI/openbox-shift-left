package main

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/client/workloadauth"
	"github.com/openbox-ai/openbox-shift-left/internal/provider"
)

// tokenWarmInterval is how often a lane daemon checks each tool's hook token
// cache. It is well inside the renewal window (workloadauth renews 120s
// before expiry), so while a daemon runs the file is renewed before any hook
// would have to renew it inside its own evaluation budget.
var tokenWarmInterval = 20 * time.Second

// startTokenWarmers keeps each configured tool's workload-token.json fresh
// for the hook processes that read it. Tokens live 300s, so without this
// every fifth minute some gated hook paid for a bootstrap and a Keycloak
// exchange out of its 10s budget, and a slow token endpoint denied it.
//
// This is a separate Authenticator on the hook's FILE cache, not the daemon's
// own sending client, which stays on its memory cache (laneidentity.go). The
// file is only ever replaced by an atomic rename of a whole valid entry, so a
// hook and a daemon writing it concurrently leave one valid token either way,
// and a hook's Invalidate racing a renewal leaves either a new token or a
// miss.
func startTokenWarmers(ctx context.Context, logger *log.Logger) {
	for _, tool := range provider.Supported() {
		creds, err := devconfig.ResolveCredentialsFor(tool)
		if err != nil || creds.TokenCachePath == "" {
			continue // resolveProviderIdentities already reports why
		}
		auth, err := workloadauth.NewAuthenticator(creds.APIKey, creds.WorkloadPrivateKey, creds.TokenCachePath, creds.BaseURL, &http.Client{Timeout: 30 * time.Second})
		if err != nil {
			logger.Printf("openbox: %s token warmer not started: %v", tool, err)
			continue
		}
		go warmTokenLoop(ctx, logger, tool, auth)
	}
}

// warmTokenLoop runs Warm now and on every tick until ctx ends, logging a
// failure once when it starts and once when it clears rather than every tick.
func warmTokenLoop(ctx context.Context, logger *log.Logger, tool string, auth *workloadauth.Authenticator) {
	t := time.NewTicker(tokenWarmInterval)
	defer t.Stop()
	failing := false
	for {
		err := auth.Warm(ctx)
		switch {
		case err != nil && !failing && ctx.Err() == nil:
			logger.Printf("openbox: %s workload token renewal failing (hooks renew on their own until it recovers): %v", tool, err)
			failing = true
		case err == nil && failing:
			logger.Printf("openbox: %s workload token renewal recovered", tool)
			failing = false
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
