// Command openbox-git-action is the CI entrypoint for STORY-SL-6: at
// push/deploy it resolves the OpenBox session(s) that produced the pushed
// commit and emits a Deploy governance event linked to them.
//
// It is designed to run as a CI step (GitHub Actions and equivalents). Config
// comes from flags with CI-env fallbacks:
//
//	--sha         commit that was pushed/deployed  (env: GITHUB_SHA)
//	--base        optional range base; resolve base..sha (env: OPENBOX_DEPLOY_BASE)
//	--repo        repository slug for metadata     (env: GITHUB_REPOSITORY)
//	--environment deploy environment               (env: OPENBOX_DEPLOY_ENV)
//	--dir         repository working dir           (default: cwd)
//	--dry-run     resolve + print the event as JSON; do NOT emit (no creds needed)
//
// Client identity (an OpenBox agent minted by openbox-backend POST /agent/create):
//
//	OPENBOX_BASE_URL   openbox-core base, e.g. https://core.openbox.ai
//	OPENBOX_API_KEY    obx_(live|test)_… runtime key   (INV-1: never logged)
//	OPENBOX_DID        the agent's did:aip:<uuid>
//	OPENBOX_SEED       base64 raw 32-byte Ed25519 seed (INV-1: never logged)
//
// Exit codes: 0 = resolved (emit success OR fail-open drop, INV-3); 2 = usage /
// precondition fault the operator must fix (bad --sha, missing creds when not
// --dry-run). It never exits non-zero over a telemetry transport failure.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"

	gitaction "github.com/openbox-ai/openbox-shift-left/actions/openbox-git-action"
	"github.com/openbox-ai/openbox-shift-left/client"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("openbox-git-action", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		sha    = fs.String("sha", envOr("GITHUB_SHA", ""), "pushed/deployed commit (real pushed SHA)")
		base   = fs.String("base", os.Getenv("OPENBOX_DEPLOY_BASE"), "optional range base; resolve base..sha")
		repo   = fs.String("repo", os.Getenv("GITHUB_REPOSITORY"), "repository slug for metadata")
		env    = fs.String("environment", envOr("OPENBOX_DEPLOY_ENV", "production"), "deploy environment")
		dir    = fs.String("dir", "", "repository working dir (default: current dir)")
		dryRun = fs.Bool("dry-run", false, "resolve and print the event as JSON; do not emit")
	)
	if err := fs.Parse(args); err != nil {
		return 2
	}

	logger := log.New(stderr, "", 0)
	if *sha == "" {
		logger.Printf("openbox-git-action: --sha (or GITHUB_SHA) is required")
		return 2
	}

	// Phase-1: NoopVerifier — the session-ownership read API is external/deferred
	// (EXT-lineage / FR-7), so resolved trailers stay unverified claims (SL5-SEC-1).
	resolver := gitaction.NewResolver(*dir, gitaction.NoopVerifier{})

	act := &gitaction.Action{
		Resolver: resolver,
		Meta: gitaction.DeployMeta{
			Repo:         *repo,
			Environment:  *env,
			DeveloperDID: os.Getenv("OPENBOX_DID"),
		},
		Log: logger,
	}

	// A real client is built only when we intend to emit. Dry-run needs no creds.
	if !*dryRun {
		c, err := client.New(client.Config{
			BaseURL: os.Getenv("OPENBOX_BASE_URL"),
			APIKey:  os.Getenv("OPENBOX_API_KEY"),
			DID:     os.Getenv("OPENBOX_DID"),
			SeedB64: os.Getenv("OPENBOX_SEED"),
			Logger:  logger,
		})
		if err != nil {
			// A structurally unusable identity is a precondition fault, not a
			// telemetry drop — surface it so the operator fixes the CI secrets.
			logger.Printf("openbox-git-action: client config error: %v", err)
			return 2
		}
		act.Emitter = c
	}

	res, err := act.Run(context.Background(), *sha, *base)
	if err != nil {
		// Resolution failure = bad SHA / repo, a precondition to fix.
		logger.Printf("openbox-git-action: resolve failed: %v", err)
		return 2
	}

	if *dryRun {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(map[string]any{
			"resolution": res.Resolution,
			"event":      res.Event,
			"emitted":    res.Emitted,
		})
	} else {
		fmt.Fprintf(stdout, "openbox-git-action: %s status=%s sessions=%d emitted=%t\n",
			res.Event.Metadata["deploy_id"], res.Resolution.Status,
			len(res.Resolution.Sessions), res.Emitted)
	}
	return 0
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
