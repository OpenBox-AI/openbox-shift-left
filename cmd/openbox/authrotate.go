package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/backend"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/prompt"
)

// runAuthRotate rotation is destructive server-side; it invalidates the
// previous key and writes a SECURITY_EVENT audit entry; so it never happens
// implicitly.
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

	newKey, err := cl.RotateAPIKey(ctx, agentID)
	if err != nil {
		return a.errorf("%v", err)
	}
	newDID, newPrivateKey, err := cl.RotateIdentity(ctx, agentID)
	if err != nil {
		return a.errorf("%v\n  note: the API key rotation already succeeded, so the previous key is invalid. "+
			"Re-run `openbox auth --rotate` once the identity rotation works.", err)
	}

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
		// If it did, something is wrong upstream and silently adopting the new one
		// would re-attribute this machine's history.
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
