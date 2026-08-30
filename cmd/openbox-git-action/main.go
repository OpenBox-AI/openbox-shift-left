// Command openbox-git-action is the CI entrypoint: at push/deploy it
// resolves the OpenBox session(s) that produced the pushed commit and
// emits a Deploy governance event linked to them.
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
//	OPENBOX_AGENT_PRIVATE_KEY  base64 raw 32-byte Ed25519 signing key
//	                           (INV-1: never logged). OPENBOX_SEED and
//	                           OPENBOX_ED25519_SEED still read as deprecated
//	                           aliases so an existing workflow keeps working.
//
// Ownership verification (off by default). With it off, every trailer stays
// an unverified claim and deploys resolve `inferred` (the NoopVerifier
// default). With it on, each trailer session id is verified as owned by the
// deploy agent before it can be `attributed`; a forged/other id stays out
// of verified_session_ids.
//
//	OPENBOX_OWNERSHIP_VERIFY=1     enable ownership verification (default: off)
//	OPENBOX_OWNERSHIP_API_URL      openbox-backend control-plane origin (https; bare, no path)
//	OPENBOX_AGENT_ID               the deploy agent's UUID (from POST /agent/create)
//	OPENBOX_ORG_API_KEY            org X-API-Key (obx_key_…) holding read:agent_session
//
// The verifier reads openbox-backend's GET /agent/<agentID>/sessions?search=<id>
// with the org key and promotes a claim only when a returned session's run_id
// matches. OPENBOX_AGENT_ID is bound to OPENBOX_DID (uuidv5) at startup so it can
// only ever read the deploy principal's own sessions (INV-4). A misconfigured or
// unreachable verifier NEVER breaks the deploy and NEVER over-attributes: it
// degrades to the NoopVerifier default (everything inferred).
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

	gitaction "github.com/openbox-ai/openbox-shift-left/internal/actions/openbox-git-action"
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

	// Verifier selection. Default: NoopVerifier — resolved trailers stay
	// unverified claims and deploys resolve `inferred`. When the operator
	// opts in and we have creds to sign the read (i.e. not --dry-run), wire
	// the real apiVerifier so owned sessions become `attributed`. Any
	// config fault degrades to Noop — never break CI, never over-attribute
	// over telemetry.
	resolver := gitaction.NewResolver(*dir, selectVerifier(*dryRun, logger))

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
			BaseURL:       os.Getenv("OPENBOX_BASE_URL"),
			APIKey:        os.Getenv("OPENBOX_API_KEY"),
			DID:           os.Getenv("OPENBOX_DID"),
			PrivateKeyB64: privateKeyFromEnv(),
			Logger:        logger,
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

// selectVerifier picks the OwnershipVerifier. It returns the real
// apiVerifier only when the operator explicitly opted in
// (OPENBOX_OWNERSHIP_VERIFY=1), a read-API base URL is set, and we are not
// in --dry-run (which carries no creds to sign the read). In every other
// case — flag off, no URL, dry-run, or a construction fault — it returns
// NoopVerifier, so the default behavior is everything `inferred` and a
// misconfigured verifier can never break the deploy or over-attribute.
func selectVerifier(dryRun bool, logger *log.Logger) gitaction.OwnershipVerifier {
	if dryRun || os.Getenv("OPENBOX_OWNERSHIP_VERIFY") != "1" {
		return gitaction.NoopVerifier{}
	}
	apiURL := os.Getenv("OPENBOX_OWNERSHIP_API_URL")
	v, err := gitaction.NewAPIVerifier(gitaction.APIVerifierConfig{
		BaseURL:   apiURL,
		AgentID:   os.Getenv("OPENBOX_AGENT_ID"),
		PusherDID: os.Getenv("OPENBOX_DID"),
		OrgAPIKey: os.Getenv("OPENBOX_ORG_API_KEY"),
		Logger:    logger,
	})
	if err != nil {
		// Fail-safe: an opted-in but unusable verifier degrades to Noop (everything
		// inferred) rather than breaking CI or over-attributing. Say so loudly.
		logger.Printf("openbox-git-action: ownership verification DISABLED (config error): %v", err)
		return gitaction.NoopVerifier{}
	}
	logger.Printf("openbox-git-action: ownership verification ENABLED against %s", apiURL)
	return v
}

// privateKeyFromEnv reads the signing key under the name OpenBox documents, then
// the two deprecated aliases.
//
// This action shipped with OPENBOX_SEED while the CLI used OPENBOX_ED25519_SEED
// and the platform's own SDK docs said OPENBOX_AGENT_PRIVATE_KEY — three names
// for one value, so a developer following the docs configured a variable nothing
// read. The documented name wins; the aliases keep existing CI workflows green,
// and retiring them needs its own decision.
func privateKeyFromEnv() string {
	for _, name := range []string{"OPENBOX_AGENT_PRIVATE_KEY", "OPENBOX_SEED", "OPENBOX_ED25519_SEED"} {
		if v := os.Getenv(name); v != "" {
			return v
		}
	}
	return ""
}
