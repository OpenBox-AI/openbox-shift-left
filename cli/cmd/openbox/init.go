package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/openbox-ai/openbox-shift-left/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/backend"
)

// `openbox init` — the front door for both roles.
//
//	openbox init --provider claude-code --enforce      # role=dev (default)
//	openbox init --role approver --org acme.example    # role=approver
//
// There is one spelling. `openbox init` is gone rather than deprecated —
// two ways to onboard is two things to keep true in every doc, and `openbox
// dev` keeps only the commands that operate on an install that already exists
// (`verify`, `sync`).
//
// The two roles write two different files (devconfig.ConfigPathFor) because
// they are two different principals. A developer's hooks must never resolve an
// approver's coordinates — that is the same-principal self-approval the key
// boundary exists to prevent — so role is decided once, here, and the hot path
// never has to ask.

// probeUUID is a well-formed id that cannot exist. Deciding it exercises the
// backend's permission guard without touching a real request: a 403 means the
// credential lacks manage:agent_session, any other status means the guard let
// it through and only the (absent) row was the problem.
const probeUUID = "00000000-0000-4000-8000-000000000000"

func (a *app) runInit(args []string) int {
	role, rest, err := extractRole(args)
	if err != nil {
		return a.errorf("%v", err)
	}
	if role == devconfig.RoleApprover {
		return a.runApproverInit(rest)
	}
	return a.runDevInit(rest)
}

// extractRole pulls --role out of the argument list so each role's own flag set
// stays exactly what that role accepts.
func extractRole(args []string) (devconfig.Role, []string, error) {
	rest := make([]string, 0, len(args))
	value := ""
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--role" || args[i] == "-role":
			if i+1 >= len(args) {
				return "", nil, fmt.Errorf("--role needs a value (%q or %q)", devconfig.RoleDev, devconfig.RoleApprover)
			}
			value = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--role=") || strings.HasPrefix(args[i], "-role="):
			value = args[i][strings.Index(args[i], "=")+1:]
		default:
			rest = append(rest, args[i])
		}
	}
	role, err := devconfig.ParseRole(value)
	return role, rest, err
}

// runApproverInit installs an APPROVER: a credentialed client of the approval
// queue. It registers no agent and installs no hooks — an approver is not a
// governed runtime, it is a different person's principal deciding other
// people's requests (OD-T-3).
func (a *app) runApproverInit(args []string) int {
	fs := a.newFlagSet("openbox init --role approver")
	var orgID, backendURL, clientID, secretBackend, host, envelope string
	var dryRun, decide bool
	fs.StringVar(&orgID, "org", a.env("OPENBOX_ORG_ID", ""), "organization whose approval queue this approver works (required)")
	fs.StringVar(&backendURL, "backend-url", a.env(devconfig.EnvBackendURL, ""), "openbox-backend control-plane base URL")
	fs.StringVar(&clientID, "client-id", a.env("OPENBOX_CLIENT", "openbox-cli"), "value for the x-openbox-client header (Keycloak JWT path)")
	// Parsed only so passing it fails loudly rather than silently doing nothing
	// (ADR-0015 deleted the store it selected).
	fs.StringVar(&secretBackend, "secret-backend", "", "REMOVED — the approver credential lives in ~/.openbox/.env")
	fs.StringVar(&host, "host", "", "agentic host that evaluates a request when running unattended: claude-code|codex (default: none — a human decides)")
	fs.StringVar(&envelope, "envelope", "", "policy bundle bounding what the host may decide; the host may only narrow it")
	fs.BoolVar(&decide, "allow-decide", false, "let this approver DECIDE, not just observe. Off by default: an approver starts in shadow mode until its envelope has been read against real traffic")
	fs.BoolVar(&dryRun, "dry-run", false, "print the plan; make no network or filesystem writes")
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}
	if secretBackend != "" {
		return a.errorf("--secret-backend was removed: there is no secret store to choose any more.\n" +
			"  The approver credential is written to ~/.openbox/.env (plaintext, 0600 — see\n" +
			"  docs/adr/ADR-0015-plaintext-credential-file.md), by this command or by `openbox auth`.\n" +
			"  Re-run without this flag.")
	}
	if orgID == "" {
		return a.errorf("--org is required (or OPENBOX_ORG_ID) — it names the approval queue this approver works")
	}
	if backendURL == "" {
		return a.errorf("set --backend-url or %s to the openbox-backend base URL", devconfig.EnvBackendURL)
	}

	a.migrateLegacyConfig()
	path, err := devconfig.ApproverConfigWritePath()
	if err != nil {
		return a.errorf("%v", err)
	}
	envPath, err := devconfig.EnvFilePath()
	if err != nil {
		return a.errorf("%v", err)
	}

	if dryRun {
		fmt.Fprintf(a.stdout, "DRY RUN — no network calls, no filesystem writes.\n\n")
		fmt.Fprintf(a.stdout, "Would install an APPROVER (a queue client; no agent is registered, no hooks are installed):\n")
		fmt.Fprintf(a.stdout, "  org:          %s\n  backend:      %s\n  host:         %s\n  envelope:     %s\n  mode:         %s\n",
			orgID, backendURL, orNone(host), orNone(envelope), shadowLabel(!decide))
		fmt.Fprintf(a.stdout, "  config:       %s\n  credential:   %s as %s\n", path, envPath, devconfig.EnvControlToken)
		fmt.Fprintf(a.stdout, "\n  NOTE: that credential is an ORGANIZATION key with fleet-wide create/rotate\n")
		fmt.Fprintf(a.stdout, "        authority, and it is stored in PLAINTEXT (ADR-0015). An agent signing key\n")
		fmt.Fprintf(a.stdout, "        compromises one agent; this compromises every agent in %s.\n", orgID)
		return exitOK
	}

	// The approver's credential is its OWN org credential, from the
	// environment and never a flag (INV-1). It is never the developer agent's
	// runtime key: holding both is the self-approval the boundary prevents.
	token := a.getenv(devconfig.EnvControlToken)
	if token == "" {
		return a.errorf("set %s (the APPROVER's own organization credential) in the environment; "+
			"it is never accepted as a flag so it cannot leak via argv/shell history (INV-1)", devconfig.EnvControlToken)
	}
	if problem := controlTokenProblem(token); problem != "" {
		return a.errorf("%s", problem)
	}

	cl := backend.New(backendURL, token, clientID)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := cl.PendingApprovals(ctx, orgID); err != nil {
		return a.errorf("this credential cannot read %s's approval queue: %v", orgID, err)
	}
	fmt.Fprintf(a.stdout, "✓ credential reads the approval queue for %s\n", orgID)

	// Whether it may DECIDE is checked now rather than at the first decision:
	// an approver that discovers its own powerlessness mid-incident is worse
	// than one that refuses to install.
	//
	// The probe needs a real agent from this org, because the decide route
	// answers 403 both for a missing permission AND for an agent outside the
	// caller's org — a made-up agent id cannot tell those apart.
	probeAgent, err := cl.FirstAgentID(ctx)
	if err != nil { //nolint:govet // err is reused deliberately
		return a.errorf("cannot list %s's agents to verify this credential: %v", orgID, err)
	}
	switch {
	case probeAgent == "":
		fmt.Fprintf(a.stderr, "note: %s has no agents yet, so the decide permission could not be verified now;\n"+
			"      it will be exercised on the first decision.\n", orgID)
	default:
		if err := cl.DecideApproval(ctx, probeAgent, probeUUID, backend.ApprovalApprove); err != nil {
			var apiErr *backend.APIError
			if errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusForbidden || apiErr.StatusCode == http.StatusUnauthorized) {
				return a.errorf("this credential cannot decide approvals (HTTP %d) — it needs manage:agent_session", apiErr.StatusCode)
			}
			// Any other error is the absent probe row, which is what we expect.
		}
		fmt.Fprintf(a.stdout, "✓ credential may decide approvals (manage:agent_session)\n")
	}

	// The org key is persisted so `openbox approve` runs later with no
	// environment. That is the point of an approver install — and it is also the
	// largest single exposure this product creates, so the write is announced
	// rather than silent (ADR-0015).
	if err := devconfig.WriteEnvFile(envPath, map[string]string{devconfig.EnvControlToken: token}); err != nil {
		return a.errorf("write approver credential: %v", err)
	}

	cfg := devconfig.ApproverConfig{
		BackendURL:     backendURL,
		OrgID:          orgID,
		Host:           host,
		Envelope:       envelope,
		Shadow:         !decide,
		PollIntervalMS: 1000,
		HostTimeoutMS:  8000,
		MaxAutoPerHour: 60,
	}
	if err := devconfig.WriteApprover(path, cfg); err != nil {
		return a.errorf("%v", err)
	}

	fmt.Fprintf(a.stdout, "\nApprover installed.\n")
	fmt.Fprintf(a.stdout, "  config    %s\n  queue     %s (org %s)\n  mode      %s\n", path, backendURL, orgID, shadowLabel(cfg.Shadow))
	fmt.Fprintf(a.stderr, "\nwarning: %s now holds an ORGANIZATION key in plaintext. It can create and rotate\n", envPath)
	fmt.Fprintf(a.stderr, "         agents across %s — a far larger blast radius than one agent's signing key.\n", orgID)
	fmt.Fprintf(a.stderr, "         Do not run an approver install on a shared host (ADR-0015).\n")
	if host != "" {
		fmt.Fprintf(a.stdout, "  host      %s\n", host)
	}
	fmt.Fprintf(a.stdout, "\n  openbox approve list        # the queue, no environment needed\n")
	return exitOK
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

func shadowLabel(shadow bool) string {
	if shadow {
		return "shadow (records what it would decide; decides nothing)"
	}
	return "deciding"
}
