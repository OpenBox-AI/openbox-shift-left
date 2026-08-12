package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/openbox-ai/openbox-shift-left/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/backend"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/prompt"
)

// authrotate.go — `openbox auth --rotate`.
//
// The recovery path for an agent that exists remotely whose credentials are
// gone: rotation re-issues both, preserving the agent's id and DID. It is also
// the documented way out for an install whose credentials are stranded in the OS
// keychain this release stopped reading (ADR-0015).
//
// Rotation is DESTRUCTIVE server-side — it invalidates the previous key and
// writes a SECURITY_EVENT audit entry — so it never happens implicitly. Without
// --rotate no rotate endpoint is called at all.
func (a *app) runAuthRotate(f authFields, piped map[string]string, envPath, backendURLFlag string, yes bool) int {
	token := a.getenv(devconfig.EnvControlToken)
	if token == "" {
		token = piped[devconfig.EnvControlToken]
	}
	if token == "" {
		return a.errorf("rotation needs an organization credential.\n"+
			"  Set %s (an obx_key_ organization key with update:agent) in the environment; it is never\n"+
			"  accepted as a flag so it cannot leak via argv or shell history (INV-1).\n"+
			"  No org key? Ask whoever has one to rotate for you, or run `openbox auth` without --rotate\n"+
			"  and leave the agent id blank to register a fresh agent instead.",
			devconfig.EnvControlToken)
	}
	if problem := controlTokenProblem(token); problem != "" {
		return a.errorf("%s", problem)
	}
	backendURL := firstNonEmptyStr(backendURLFlag, f.backendURL, devconfig.ResolveBackendURL(), devconfig.DefaultBackendURL)
	if backendURL == "" {
		return a.errorf("no backend URL — pass --backend-url or set %s", devconfig.EnvBackendURL)
	}

	agentID := strings.TrimSpace(f.agentID)
	if agentID == "" {
		return a.errorf("rotation needs the agent id of the agent to re-issue credentials for.\n" +
			"  It is on the agent's page in the dashboard, and `openbox doctor` prints the one this\n" +
			"  machine is configured with. Rotation preserves that agent and its DID; registering a\n" +
			"  new agent instead is `openbox auth` with a blank agent id.")
	}

	// The confirm gate precedes the REMOTE call, not just the write: by the time
	// RotateAPIKey returns, the previous key is already dead.
	if !yes {
		fmt.Fprintf(a.stdout, "\nRotate credentials for agent %s?\n", agentID)
		fmt.Fprintf(a.stdout, "  • the agent's CURRENT API key is invalidated immediately, server-side\n")
		fmt.Fprintf(a.stdout, "  • any other machine using that key stops working until it is re-issued too\n")
		fmt.Fprintf(a.stdout, "  • the DID is preserved, so lineage and history stay attached to this agent\n")
		fmt.Fprintf(a.stdout, "  • OpenBox records a SECURITY_EVENT audit entry for the rotation\n")
		p := prompt.New(a.stdinFile(), a.stdout)
		ok, err := p.Confirm("Proceed", false)
		if err != nil {
			return a.errorf("%v", err)
		}
		if !ok {
			fmt.Fprintln(a.stdout, "Nothing rotated.")
			return exitOK
		}
	}

	cl := backend.New(backendURL, token, "openbox-cli")
	ctx := context.Background()

	// Key first, then identity. A failure between the two leaves the agent with a
	// working signing identity and a dead key, which `--rotate` can retry; the
	// reverse order would leave a working key that cannot sign, which looks
	// healthy until the first event fails verification.
	newKey, err := cl.RotateAPIKey(ctx, agentID)
	if err != nil {
		return a.errorf("%v", err)
	}
	newDID, newPrivateKey, err := cl.RotateIdentity(ctx, agentID)
	if err != nil {
		// The key is already rotated at this point, and the error says so, because
		// otherwise a user retries and cannot understand why the old key stopped
		// working "before" anything happened.
		return a.errorf("%v\n  note: the API key rotation already succeeded, so the previous key is invalid. "+
			"Re-run `openbox auth --rotate` once the identity rotation works.", err)
	}

	// Guard before ANY write: a half-valid credential pair on disk is worse than
	// no rotation, because the install looks configured and fails at flush time.
	if strings.HasPrefix(newKey, "obx_key_") {
		return a.errorf("the rotated API key looks like an ORGANIZATION key (obx_key_…), not an agent runtime key.\n" +
			"  Refusing to write it: an org key in the agent's runtime slot would give the hook\n" +
			"  org-wide authority. Report this — the endpoint returned the wrong credential type.")
	}
	if problem := privateKeyProblem(newPrivateKey); problem != "" {
		return a.errorf("the rotated signing key is unusable: %s\n"+
			"  Nothing was written. Agent %s now has rotated credentials this machine does not hold;\n"+
			"  re-run `openbox auth --rotate`.", problem, agentID)
	}
	if f.did != "" && newDID != f.did {
		// The DID is derived from the agent id, so it cannot change across a
		// rotation. If it did, something is wrong upstream and silently adopting
		// the new one would re-attribute this machine's history.
		return a.errorf("the rotated identity returned DID %s but this agent's DID is %s.\n"+
			"  Rotation must preserve the DID (it is derived from the agent id), so refusing to write.\n"+
			"  Nothing was changed locally.", newDID, f.did)
	}

	f.apiKey, f.privateKey, f.did = newKey, newPrivateKey, newDID
	if code := a.writeSecrets(envPath, f, piped); code != exitOK {
		return code
	}
	if code := a.writeCoordinates(f); code != exitOK {
		return code
	}
	fmt.Fprintf(a.stdout, "\n✓ rotated agent %s — new API key and signing key, same DID %s\n", agentID, newDID)
	a.warnShadowedByEnv(f, envPath)
	fmt.Fprintf(a.stdout, "\nIf this machine's hooks are already installed, nothing further is needed.\n")
	return exitOK
}
