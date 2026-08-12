package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/openbox-ai/openbox-shift-left/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/backend"
)

// `openbox approve` — the terminal half of the human approval tier (E9 §2.3).
//
// The dashboard's Approvals page is the default surface and needs no build.
// This exists because the binary is already installed and already
// cross-platform, so an approver who lives in a terminal gets the queue without
// a second install — the thing the abandoned approver app cost.
//
// It is an API CLIENT and nothing else: it polls and it decides. No listener,
// no socket, no daemon (ADR-0006). And it is meant to run on the APPROVER's
// machine, not the requester's — a same-machine approver is a convenience
// control, not four-eyes (E9 §3.7).
//
// Its credential is the approver's own org control-plane credential, read from
// the environment rather than a flag so it cannot leak through argv or shell
// history (INV-1). It is never the developer agent's obx_ runtime key: holding
// both would be exactly the self-approval the boundary exists to prevent.

// defaultWatchInterval paces --watch. The queue is a human-latency surface, so
// the useful cadence is seconds, not milliseconds.
const defaultWatchInterval = 15 * time.Second

func (a *app) runApprove(args []string) int {
	if len(args) == 0 {
		return a.errorf("usage: openbox approve <list|allow|deny> [flags]  |  openbox approve --watch [--auto --host claude-code]")
	}
	// `openbox approve --watch …` with no subcommand is the working surface:
	// watch the queue, and with --auto let the bounded approver decide inside
	// it. It is `list --watch` plus authority, so it shares that flag set.
	if strings.HasPrefix(args[0], "-") {
		return a.runApproveList(args)
	}
	switch args[0] {
	case "list":
		return a.runApproveList(args[1:])
	case "allow":
		return a.runApproveDecide(args[1:], backend.ApprovalApprove)
	case "deny":
		return a.runApproveDecide(args[1:], backend.ApprovalReject)
	default:
		return a.errorf("usage: openbox approve <list|allow|deny> [flags]  |  openbox approve --watch [--auto --host claude-code]")
	}
}

// approveClient resolves the approver's credentials and the queue to read.
//
// `openbox init --role approver` puts all three — credential, queue, org — in
// place, so an installed approver needs no environment at all. The environment
// still wins where it is set: an operator with two queues to work should not
// have to re-install to switch.
func (a *app) approveClient(orgID, clientID string) (*backend.Client, string, int) {
	cfg, _ := devconfig.LoadApprover(devconfig.DefaultApproverConfigPath())

	token := a.getenv(devconfig.EnvControlToken)
	if token == "" {
		token = a.approverToken()
	}
	if token == "" {
		return nil, "", a.errorf("no approver credential — run `openbox init --role approver --org <id>`, "+
			"or set %s in the environment (never a flag, so it cannot leak via argv/shell history — INV-1)",
			devconfig.EnvControlToken)
	}
	backendURL := devconfig.ResolveBackendURL()
	if backendURL == "" {
		backendURL = cfg.BackendURL
	}
	if backendURL == "" {
		return nil, "", a.errorf("no backend URL configured — set %s", devconfig.EnvBackendURL)
	}
	if orgID == "" {
		orgID = cfg.OrgID
	}
	if orgID == "" {
		return nil, "", a.errorf("set --org <organization-id> (or OPENBOX_ORG_ID) — it names the approval queue to read")
	}
	return backend.New(backendURL, token, clientID), orgID, exitOK
}

// approverToken reads the credential `init --role approver` wrote to
// ~/.openbox/.env. An absent file or key is not an error here: the caller
// reports the one honest message for "no credential at all".
//
// The environment still wins over this (see approveClient), which is why the
// order there is env-then-file rather than the reverse.
func (a *app) approverToken() string {
	path, err := devconfig.EnvFilePath()
	if err != nil {
		return ""
	}
	kv, err := devconfig.ParseEnvFile(path)
	if err != nil {
		return ""
	}
	return kv[devconfig.EnvControlToken]
}

func (a *app) runApproveList(args []string) int {
	fs := a.newFlagSet("openbox approve list")
	var orgID, clientID, host, envelope string
	var watch, auto, decide, once, allowSelf bool
	var interval, hostTimeout time.Duration
	var maxPerHour int
	fs.StringVar(&orgID, "org", a.env("OPENBOX_ORG_ID", ""), "organization id whose approval queue to read")
	fs.StringVar(&clientID, "client-id", a.env("OPENBOX_CLIENT", "openbox-cli"), "value for the x-openbox-client header (Keycloak JWT path)")
	fs.BoolVar(&watch, "watch", false, "keep polling and reprint the queue as it changes")
	fs.DurationVar(&interval, "interval", 0, "poll interval for --watch (default 15s watching, 1s with --auto)")
	fs.BoolVar(&auto, "auto", false, "work the queue autonomously inside the org envelope (ADR-0012). Records what it would do; decides only with --decide")
	fs.BoolVar(&decide, "decide", false, "let --auto actually decide, instead of shadow-recording what it would decide")
	fs.StringVar(&host, "host", "", "agentic host consulted for requests the envelope marks consultable: claude-code (default: the host in approver.json)")
	fs.StringVar(&envelope, "envelope", "", "envelope file bounding what may be decided (default: the envelope in approver.json)")
	fs.DurationVar(&hostTimeout, "host-timeout", 0, "how long a host consultation may take before the request is left for a human")
	fs.IntVar(&maxPerHour, "max-per-hour", -1, "cap on autonomous decisions per hour (0 = unbounded)")
	fs.BoolVar(&once, "once", false, "make one pass over the queue and exit")
	fs.BoolVar(&allowSelf, "allow-same-agent", false, "allow deciding requests filed by THIS machine's own developer agent. Off by default: same-agent approval is a convenience control, not four-eyes")
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}
	cl, org, code := a.approveClient(orgID, clientID)
	if cl == nil {
		return code
	}

	if auto {
		return a.runApproveAuto(cl, org, autoFlags{
			host: host, envelope: envelope, decide: decide, once: once,
			interval: interval, hostTimeout: hostTimeout, maxPerHour: maxPerHour,
			allowSelf: allowSelf,
		})
	}

	if interval <= 0 {
		interval = defaultWatchInterval
	}
	for {
		if code := a.printPending(cl, org); code != exitOK && !watch {
			return code
		}
		if !watch {
			return exitOK
		}
		time.Sleep(interval)
	}
}

func (a *app) printPending(cl *backend.Client, orgID string) int {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pending, err := cl.PendingApprovals(ctx, orgID)
	if err != nil {
		return a.errorf("read approval queue: %v", err)
	}
	if len(pending) == 0 {
		fmt.Fprintf(a.stdout, "No pending approvals.\n")
		return exitOK
	}
	// Oldest first: the request closest to expiring is the one that needs a
	// decision, and it should not be at the bottom of the list.
	sort.SliceStable(pending, func(i, j int) bool {
		return created(pending[i]).Before(created(pending[j]))
	})

	fmt.Fprintf(a.stdout, "%d pending approval(s):\n\n", len(pending))
	undecidable := 0
	for _, ap := range pending {
		fmt.Fprintf(a.stdout, "  %s\n", ap.ID)
		fmt.Fprintf(a.stdout, "    tool     %s\n", orUnset(ap.ActivityType))
		fmt.Fprintf(a.stdout, "    agent    %s\n", orUnset(ap.Name()))
		fmt.Fprintf(a.stdout, "    reason   %s\n", orUnset(deref(ap.Reason)))

		// The decisive line. An approver decides on WHAT is being asked, so it
		// gets its own field rather than being buried among the identifiers.
		if req := ap.Request(); req != "" {
			fmt.Fprintf(a.stdout, "    request  %s\n", req)
		} else {
			// Never let this read as "nothing notable" — an approval nobody can
			// evaluate is a control that is not being exercised, and saying so
			// is the point of the tool.
			undecidable++
			fmt.Fprintf(a.stdout, "    request  (not captured — see the note below)\n")
		}
		if s := structuralInput(ap.Context()); s != "" {
			fmt.Fprintf(a.stdout, "    context  %s\n", s)
		}
		fmt.Fprintf(a.stdout, "    expires  %s\n", expiryLabel(ap))
		fmt.Fprintf(a.stdout, "    decide   openbox approve allow %s   |   openbox approve deny %s\n\n", ap.ID, ap.ID)
	}
	if undecidable > 0 {
		fmt.Fprintf(a.stdout, "%d request(s) carry no command or arguments, so there is nothing to judge\n", undecidable)
		fmt.Fprintf(a.stdout, "them on. The developer runtime only sends them under content capture; with it\n")
		fmt.Fprintf(a.stdout, "off (`content_capture:false`), approving here is a rubber stamp — deny, or turn\n")
		fmt.Fprintf(a.stdout, "capture on for the escalation path. See OD-E9-7.\n")
	}
	return exitOK
}

// structuralInput renders the request's activity_input as sorted key=value
// pairs. The values are structural identifiers by construction (tool kind, tool
// name, file path, MCP server); it renders whatever arrived rather than
// selecting fields, so a new identifier shows up without a code change.
func structuralInput(in map[string]any) string {
	if len(in) == 0 {
		return ""
	}
	keys := make([]string, 0, len(in))
	for k := range in {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%v", k, in[k]))
	}
	return strings.Join(parts, "  ")
}

func (a *app) runApproveDecide(args []string, action string) int {
	verb := "allow"
	if action == backend.ApprovalReject {
		verb = "deny"
	}
	// The event id is taken before the flags rather than after: Go's flag
	// package stops parsing at the first positional argument, so
	// `approve allow <id> --org x` would otherwise silently drop --org.
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return a.errorf("usage: openbox approve %s <event-id> [--org <id>]", verb)
	}
	eventID := args[0]

	fs := a.newFlagSet("openbox approve " + verb)
	var orgID, clientID string
	fs.StringVar(&orgID, "org", a.env("OPENBOX_ORG_ID", ""), "organization id whose approval queue to read")
	fs.StringVar(&clientID, "client-id", a.env("OPENBOX_CLIENT", "openbox-cli"), "value for the x-openbox-client header (Keycloak JWT path)")
	if code, ok := parseFlags(fs, args[1:]); !ok {
		return code
	}
	if fs.NArg() != 0 {
		return a.errorf("usage: openbox approve %s <event-id> [--org <id>] — unexpected extra argument %q", verb, fs.Arg(0))
	}

	cl, org, code := a.approveClient(orgID, clientID)
	if cl == nil {
		return code
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// The decide route is per-agent, and only the queue knows which agent owns
	// a given event — so the id an approver reads off the list is enough, and
	// they never have to hunt for an agent id. It also means deciding something
	// that is no longer pending fails here rather than server-side.
	pending, err := cl.PendingApprovals(ctx, org)
	if err != nil {
		return a.errorf("read approval queue: %v", err)
	}
	var target *backend.Approval
	for i := range pending {
		if strings.EqualFold(pending[i].ID, eventID) {
			target = &pending[i]
			break
		}
	}
	if target == nil {
		return a.errorf("%s is not in the pending queue — it may already be decided or expired", eventID)
	}
	if target.Expired() {
		return a.errorf("%s expired at %s — its window has closed and the decision would be refused",
			eventID, target.ExpiresAt.Format(time.RFC3339))
	}
	if err := cl.DecideApproval(ctx, target.AgentID, target.ID, action); err != nil {
		return a.errorf("decide approval: %v", err)
	}
	fmt.Fprintf(a.stdout, "%s %s — %s on %s\n", decidedLabel(action), target.ID,
		orUnset(target.ActivityType), orUnset(target.Name()))
	return exitOK
}

func decidedLabel(action string) string {
	if action == backend.ApprovalApprove {
		return "Approved"
	}
	return "Rejected"
}

func created(a backend.Approval) time.Time {
	if a.CreatedAt == nil {
		return time.Time{}
	}
	return *a.CreatedAt
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func expiryLabel(a backend.Approval) string {
	if a.ExpiresAt == nil {
		return "(unset)"
	}
	if a.Expired() {
		return a.ExpiresAt.Format(time.RFC3339) + "  (EXPIRED)"
	}
	return fmt.Sprintf("%s  (in %s)", a.ExpiresAt.Format(time.RFC3339), time.Until(*a.ExpiresAt).Round(time.Second))
}
