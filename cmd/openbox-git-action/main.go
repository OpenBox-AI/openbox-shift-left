// Command openbox-git-action is the CI entrypoint: at push/deploy it resolves
// the OpenBox session(s) that produced the pushed commit and emits a Deploy
// governance event linked to them. Https://core.openbox.ai OPENBOX_API_KEY
// obx_(live|test)_… runtime key (INV-1: never logged) OPENBOX_AGENT_ID the
// v3 keycloak_workload agent's UUID OPENBOX_WORKLOAD_PRIVATE_KEY the RS256
// client-assertion key (PEM or single-line base64 DER; INV-1: never logged)
// OPENBOX_DID, if exported, must equal the attribution label
// devconfig.AttributionDIDFor(OPENBOX_AGENT_ID) derives -- a stale value from
// before a re-init is refused rather than silently misattributing. Exit
// codes: 0 = resolved (emit success OR fail-open drop, INV-3); 2 = usage /
// precondition fault the operator must fix (bad --sha, missing creds when
// not --dry-run, a stale OPENBOX_DID).
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"

	gitaction "github.com/openbox-ai/openbox-shift-left/internal/actions/openbox-git-action"
	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
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

	// DeveloperDID is derived, never read off the wire (D1): a workload agent
	// has no DID at the backend. An exported OPENBOX_DID is optional going
	// forward, but if a CI config still carries one from before this machine's
	// last (re-)init, it must agree with what OPENBOX_AGENT_ID derives to, or
	// this run is about to misattribute every session under a stale identity.
	agentID := os.Getenv("OPENBOX_AGENT_ID")
	var developerDID string
	if agentID != "" {
		did, err := devconfig.AttributionDIDFor(agentID)
		if err != nil {
			logger.Printf("openbox-git-action: OPENBOX_AGENT_ID %q is not usable: %v", agentID, err)
			return 2
		}
		developerDID = did
		if exported := os.Getenv("OPENBOX_DID"); exported != "" && exported != did {
			logger.Printf("openbox-git-action: OPENBOX_DID %q does not match the attribution label "+
				"derived from OPENBOX_AGENT_ID (%q); refusing to emit under a stale identity", exported, did)
			return 2
		}
	}

	// Any config fault degrades to Noop; never break CI, never over-attribute
	// over telemetry.
	resolver := gitaction.NewResolver(*dir, selectVerifier(*dryRun, logger))

	act := &gitaction.Action{
		Resolver: resolver,
		Meta: gitaction.DeployMeta{
			Repo:         *repo,
			Environment:  *env,
			DeveloperDID: developerDID,
		},
		Log: logger,
	}

	if !*dryRun {
		c, err := client.New(client.Config{
			BaseURL:            os.Getenv("OPENBOX_BASE_URL"),
			APIKey:             os.Getenv("OPENBOX_API_KEY"),
			WorkloadPrivateKey: os.Getenv("OPENBOX_WORKLOAD_PRIVATE_KEY"),
			Logger:             logger,
		})
		if err != nil {
			logger.Printf("openbox-git-action: client config error: %v", err)
			return 2
		}
		act.Emitter = c
	}

	res, err := act.Run(context.Background(), *sha, *base)
	if err != nil {
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

// selectVerifier builds the real ownership verifier, witnessed against core:
// the client authenticates with this run's own workload identity and
// core's GET /api/v3/auth/validate names the agent id that credential
// actually belongs to, which NewAPIVerifier then requires to match
// OPENBOX_AGENT_ID before it ever reads a session. Any config or witness
// fault degrades to Noop, same as before -- never break CI, never
// over-attribute over telemetry.
func selectVerifier(dryRun bool, logger *log.Logger) gitaction.OwnershipVerifier {
	if dryRun || os.Getenv("OPENBOX_OWNERSHIP_VERIFY") != "1" {
		return gitaction.NoopVerifier{}
	}
	apiURL := os.Getenv("OPENBOX_OWNERSHIP_API_URL")
	witnessClient, err := client.New(client.Config{
		BaseURL:            os.Getenv("OPENBOX_BASE_URL"),
		APIKey:             os.Getenv("OPENBOX_API_KEY"),
		WorkloadPrivateKey: os.Getenv("OPENBOX_WORKLOAD_PRIVATE_KEY"),
		Logger:             logger,
	})
	if err != nil {
		logger.Printf("openbox-git-action: ownership verification DISABLED (client config error): %v", err)
		return gitaction.NoopVerifier{}
	}
	v, err := gitaction.NewAPIVerifier(gitaction.APIVerifierConfig{
		BaseURL: apiURL,
		AgentID: os.Getenv("OPENBOX_AGENT_ID"),
		Witness: func(ctx context.Context) (string, error) {
			res, verr := witnessClient.ValidateDetailed(ctx)
			if verr != nil {
				return "", verr
			}
			return res.AgentID, nil
		},
		OrgAPIKey: os.Getenv("OPENBOX_ORG_API_KEY"),
		Logger:    logger,
	})
	if err != nil {
		logger.Printf("openbox-git-action: ownership verification DISABLED (config error): %v", err)
		return gitaction.NoopVerifier{}
	}
	logger.Printf("openbox-git-action: ownership verification ENABLED against %s", apiURL)
	return v
}
