package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/activation"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/gatewaycheck"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/gatewayservice"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/laneservice"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/managed"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/providers"
	"github.com/openbox-ai/openbox-shift-left/internal/telemetry"
	"github.com/openbox-ai/openbox-shift-left/internal/transport"
)

func (a *app) runDoctor(args []string) int {
	fs := a.newFlagSet("doctor")
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}

	p := devconfig.EffectivePosture()
	fmt.Fprintf(a.stdout, "OpenBox developer-runtime posture\n\n")

	fmt.Fprintf(a.stdout, "Identity\n")
	fmt.Fprintf(a.stdout, "  developer  %s\n", withPresence(devconfig.DefaultConfigPath()))
	fmt.Fprintf(a.stdout, "  approver   %s\n", withPresence(devconfig.DefaultApproverConfigPath()))
	fmt.Fprintf(a.stdout, "  Everything below is the DEVELOPER posture: hooks read that file and no\n")
	fmt.Fprintf(a.stdout, "  other. The approver config belongs to a different principal.\n\n")

	fmt.Fprintf(a.stdout, "Enforcement\n")
	flags := p.Flags()
	names := make([]string, 0, len(flags))
	width := 0
	for n := range flags {
		names = append(names, n)
		if len(n) > width {
			width = len(n) // derived, so a longer flag name cannot break the column
		}
	}
	sort.Strings(names)
	for _, n := range names {
		source := p.ConfigSource[n]
		note := ""
		switch devconfig.Source(source) {
		case devconfig.SourceManaged:
			note = "  (org mandate; not overridable here)"
		case devconfig.SourceManagedDefault:
			note = "  (org default; overridable)"
		}
		fmt.Fprintf(a.stdout, "  %-*s %-5v  from %s%s\n", width, n, flags[n], source, note)
	}

	fmt.Fprintf(a.stdout, "\nManaged OpenBox config\n")
	m := devconfig.Managed()
	switch {
	case !m.Present:
		fmt.Fprintf(a.stdout, "  %s: absent; every setting above is developer-controlled\n", m.Path)
	case !m.Readable:
		fmt.Fprintf(a.stdout, "  %s: PRESENT BUT UNREADABLE; this machine is meant to be managed and is not.\n", m.Path)
		fmt.Fprintf(a.stdout, "    Sessions fall back to developer-controlled settings. Fix the file (an unknown\n")
		fmt.Fprintf(a.stdout, "    key makes it unreadable, so check for typos in field names).\n")
	default:
		fmt.Fprintf(a.stdout, "  %s: active\n", m.Path)
		if len(m.Locked) == 0 {
			fmt.Fprintf(a.stdout, "    locked: (none); values act as org defaults the developer may override\n")
		} else {
			fmt.Fprintf(a.stdout, "    locked: %v\n", m.Locked)
		}
		if len(m.UnknownKeys) > 0 {
			fmt.Fprintf(a.stdout, "    WARNING: unrecognized keys, ignored: %v\n", m.UnknownKeys)
			fmt.Fprintf(a.stdout, "      They set nothing. Check the spelling against dev.json's field\n")
			fmt.Fprintf(a.stdout, "      names; an org that misspells a field believes it mandated something.\n")
		}
		if len(m.UnknownLocked) > 0 {
			fmt.Fprintf(a.stdout, "    WARNING: locked names no setting recognizes: %v; these lock NOTHING.\n", m.UnknownLocked)
			fmt.Fprintf(a.stdout, "      Check the spelling against the dev.json field names; a typo here is a\n")
			fmt.Fprintf(a.stdout, "      mandate the org believes is in force and is not.\n")
		}
	}

	fmt.Fprintf(a.stdout, "\nPolicy decisions\n")
	fmt.Fprintf(a.stdout, "  decided by      %s\n", orUnset(p.DecisionAuthority))
	fmt.Fprintf(a.stdout, "  if unreachable  %s\n", orUnset(p.FailurePolicy))
	if p.FailurePolicy == devconfig.FailurePolicyFailOpen {
		fmt.Fprintf(a.stdout, "                  gated calls PROCEED when the control plane cannot be\n")
		fmt.Fprintf(a.stdout, "                  reached, so enforcement depends on reachability. Set\n")
		fmt.Fprintf(a.stdout, "                  fail_closed to deny instead.\n")
	}
	fmt.Fprintf(a.stdout, "  last decision   %s\n", lastDecisionSummary())

	fmt.Fprintf(a.stdout, "\nProvider managed configuration\n")
	for _, prov := range []managed.Provider{managed.ProviderClaudeCode, managed.ProviderCodex} {
		state := managed.ProviderState(prov)
		fmt.Fprintf(a.stdout, "  %-12s %s\n", prov, state)
	}

	fmt.Fprintf(a.stdout, "\nProject hook registration\n")
	if wd, err := os.Getwd(); err != nil {
		fmt.Fprintf(a.stdout, "  current directory unreadable (%v); check skipped\n", err)
	} else {
		audit, err := providers.AuditProjectHooks(wd)
		switch {
		case err != nil:
			fmt.Fprintf(a.stdout, "  %s: could not be read; %v\n", audit.SettingsPath, err)
		case !audit.Present:
			fmt.Fprintf(a.stdout, "  %s  (absent)\n", audit.SettingsPath)
			fmt.Fprintf(a.stdout, "    No project hook config in THIS directory. A global-scope install governs\n")
			fmt.Fprintf(a.stdout, "    through managed settings instead; `openbox init` here adds project scope.\n")
		case len(audit.Engines) == 0:
			fmt.Fprintf(a.stdout, "  %s  (present, no OpenBox hooks)\n", audit.SettingsPath)
		default:
			fmt.Fprintf(a.stdout, "  %s  (present)\n", audit.SettingsPath)
			for _, engine := range audit.Engines {
				fmt.Fprintf(a.stdout, "    engine  %s\n", engine)
			}
			if len(audit.Engines) > 1 {
				fmt.Fprintf(a.stdout, "    WARNING: %d OpenBox engines are registered here. Every hook fires once\n", len(audit.Engines))
				fmt.Fprintf(a.stdout, "      per engine, so every governed tool call is stored TWICE and tool\n")
				fmt.Fprintf(a.stdout, "      success rates and latencies are meaningless. An older engine also\n")
				fmt.Fprintf(a.stdout, "      omits fields the current one sends. Run `openbox init` in this\n")
				fmt.Fprintf(a.stdout, "      directory: it replaces registrations left at another engine path.\n")
			}
			if len(audit.DuplicateEvents) > 0 {
				fmt.Fprintf(a.stdout, "    WARNING: registered more than once for: %s; same duplication.\n", strings.Join(audit.DuplicateEvents, ", "))
				fmt.Fprintf(a.stdout, "      Run `openbox init` in this directory.\n")
			}
		}
	}

	a.reportGateway()
	a.reportLanes()
	a.reportCoverage()
	a.reportSpool()

	fmt.Fprintf(a.stdout, "\nWhat this does and does not prove\n")
	fmt.Fprintf(a.stdout, "  Settings sourced from `user` or `env` can be changed by whoever runs this\n")
	fmt.Fprintf(a.stdout, "  command, so they are not assurance. Only `managed` values, and only with the\n")
	fmt.Fprintf(a.stdout, "  provider config deployed, survive a developer who does not want them.\n")
	return exitOK
}

func orUnset(s string) string {
	if s == "" {
		return "(unset)"
	}
	return s
}

func lastDecisionSummary() string {
	raw, err := os.ReadFile(hookflow.DefaultEnforcementPath())
	if err != nil || len(raw) == 0 {
		return "(none recorded)"
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	var rec struct {
		PolicyID string `json:"policy_id"`
		Source   string `json:"source"`
		Verdict  string `json:"verdict"`
	}
	if json.Unmarshal([]byte(lines[len(lines)-1]), &rec) != nil {
		return "(unreadable)"
	}
	if rec.PolicyID == "" {
		return fmt.Sprintf("%s via %s; NO policy decided this call", orUnset(rec.Verdict), orUnset(rec.Source))
	}
	return fmt.Sprintf("policy %s (%s via %s)", rec.PolicyID, orUnset(rec.Verdict), orUnset(rec.Source))
}

func withPresence(path string) string {
	if _, err := os.Stat(path); err == nil {
		return path + "  (present)"
	}
	return path + "  (absent)"
}

// reportGateway four separate questions, kept separate on purpose.
func (a *app) reportGateway() {
	home := a.getenv("HOME")
	if home == "" {
		if h, err := os.UserHomeDir(); err == nil {
			home = h
		}
	}
	r := gatewaycheck.Inspect(home, managedSettingsPathForDoctor(), 750*time.Millisecond, a.getenv)

	fmt.Fprintf(a.stdout, "\nLocal gateway (model-call governance)\n")

	if r.SettingsPath == "" {
		fmt.Fprintf(a.stdout, "  configured   no; ANTHROPIC_BASE_URL is not set in any settings file\n")
	} else {
		fmt.Fprintf(a.stdout, "  configured   %s\n", r.ConfiguredAddr)
		fmt.Fprintf(a.stdout, "  from         %s\n", r.SettingsPath)
		owner := "uid " + strconv.Itoa(r.OwnerUID)
		switch r.OwnerUID {
		case 0:
			owner = "root"
		case -1:
			owner = "unknown (this OS exposes no owner to check)"
		}
		fmt.Fprintf(a.stdout, "  owned by     %s\n", owner)
		fmt.Fprintf(a.stdout, "  tier         %s\n", r.Tier)
		if !r.TargetsGateway {
			fmt.Fprintf(a.stdout, "  target       NOT loopback; this machine is pointed at something else\n")
		}
		if r.Alive {
			fmt.Fprintf(a.stdout, "  reachable    yes\n")
		} else {
			fmt.Fprintf(a.stdout, "  reachable    NO; %s\n", r.AliveErr)
			fmt.Fprintf(a.stdout, "               model calls will FAIL rather than escape, which is the safe\n")
			fmt.Fprintf(a.stdout, "               direction. Start the gateway: `openbox gateway`\n")
		}
		// So a differing env value is reported as information, never as a fault; the
		// file above is what the tool uses.
		if r.EnvDiffersFromSettings {
			fmt.Fprintf(a.stdout, "  environment  ANTHROPIC_BASE_URL=%s is also set here, and DIFFERS\n", r.EnvValue)
			fmt.Fprintf(a.stdout, "               The settings file above takes precedence for Claude Code, so the\n")
			fmt.Fprintf(a.stdout, "               file is what the tool uses. Confirm with `/status` in a session.\n")
		} else if r.EnvValue != "" {
			fmt.Fprintf(a.stdout, "  environment  agrees (ANTHROPIC_BASE_URL=%s)\n", r.EnvValue)
		} else {
			fmt.Fprintf(a.stdout, "  verify with  `/status` in a Claude Code session; it prints the base URL\n")
			fmt.Fprintf(a.stdout, "               actually in force. doctor reads the file, which is the source\n")
			fmt.Fprintf(a.stdout, "               that wins, but only the session can confirm what it resolved.\n")
		}
		fmt.Fprintf(a.stdout, "  log          %s\n", gatewayservice.LogPath(home))
	}

	fmt.Fprintf(a.stdout, "  bypass       %s\n", map[bool]string{true: "DETECTABLE, not prevented", false: "no exposure found"}[r.BypassCapable])
	for _, note := range r.BypassNotes {
		fmt.Fprintf(a.stdout, "               - %s\n", note)
	}
}

// managedSettingsPathForDoctor reached through the managed package so doctor
// and `managed install` cannot disagree about where the file lives.
func managedSettingsPathForDoctor() string {
	dir := managed.ClaudeCodeManagedDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "managed-settings.json")
}

func (a *app) reportLanes() {
	home := a.homeDir()
	settingsPath := gatewayservice.SettingsPath(home)
	election := activation.ResolveElection(settingsPath)

	fmt.Fprintf(a.stdout, "\nModel-call producer (which lane emits turn events)\n")
	undecidable := election.SettingsProblem != ""
	switch {
	case undecidable:
		fmt.Fprintf(a.stdout, "  elected      CANNOT BE DECIDED; %s\n", election.SettingsProblem)
		fmt.Fprintf(a.stdout, "               This is NOT the same as no lane being routed. Nothing here knows\n")
		fmt.Fprintf(a.stdout, "               what this machine is configured to do, so treat every lane line\n")
		fmt.Fprintf(a.stdout, "               below as unverified. They are still printed: whether a unit is\n")
		fmt.Fprintf(a.stdout, "               installed and whether anything is listening do not come from the\n")
		fmt.Fprintf(a.stdout, "               settings file, and they are what recovery starts from.\n")
	case election.Elected == "":
		fmt.Fprintf(a.stdout, "  elected      (none); %s\n", election.Reason)
		fmt.Fprintf(a.stdout, "               No lane emits model-call turns, so token counts and costs for this\n")
		fmt.Fprintf(a.stdout, "               machine are ABSENT rather than merely incomplete.\n")
	default:
		fmt.Fprintf(a.stdout, "  elected      %s\n", election.Elected)
		fmt.Fprintf(a.stdout, "  because      %s\n", election.Reason)
	}
	if len(election.Routed) > 1 {
		fmt.Fprintf(a.stdout, "  routed       %v; exactly one of these emits; the others still send their own\n", election.Routed)
		fmt.Fprintf(a.stdout, "               non-turn evidence, which does not collide.\n")
	}
	for _, lane := range election.Routed {
		if !slices.Contains(election.Candidates, lane) {
			fmt.Fprintf(a.stdout, "  NOT IN PATH  %s is configured but cannot see this machine's model calls -\n", lane)
			fmt.Fprintf(a.stdout, "               ANTHROPIC_BASE_URL sends them somewhere it does not intercept.\n")
		}
	}

	for _, lane := range []struct {
		name string
		spec laneservice.Spec
		addr string
	}{
		{"telemetry", laneservice.Telemetry(telemetry.DefaultAddr, "", false), telemetry.DefaultAddr},
		{"transport", laneservice.Transport(transport.DefaultAddr, "", false), transport.DefaultAddr},
	} {
		fmt.Fprintf(a.stdout, "\n%s lane\n", strings.ToUpper(lane.name[:1])+lane.name[1:])
		unit := lane.spec.UnitPath(runtime.GOOS, home)
		switch {
		case unit == "":
			fmt.Fprintf(a.stdout, "  unit         (no daemon packaging on %s)\n", runtime.GOOS)
		case fileExists(unit):
			fmt.Fprintf(a.stdout, "  unit         %s\n", unit)
		default:
			fmt.Fprintf(a.stdout, "  unit         not installed\n")
		}
		routed := slices.Contains(election.Routed, activation.Lane(lane.name))
		fmt.Fprintf(a.stdout, "  configured   %s\n", map[bool]string{true: "yes; " + settingsPath, false: "no; the tool is not pointed at it"}[routed])
		occupied, _ := portOccupied(lane.addr)
		if occupied {
			fmt.Fprintf(a.stdout, "  reachable    yes (%s)\n", lane.addr)
		} else {
			fmt.Fprintf(a.stdout, "  reachable    NO; nothing is listening on %s\n", lane.addr)
			if routed {
				fmt.Fprintf(a.stdout, "               The tool is pointed at a port with nothing behind it.\n")
			}
		}
		if election.Elected == activation.Lane(lane.name) && !occupied {
			fmt.Fprintf(a.stdout, "  WARNING      this lane is ELECTED but nothing is listening, so NO lane is emitting\n")
			fmt.Fprintf(a.stdout, "               model-call turns on this machine. If you did not install it, something\n")
			fmt.Fprintf(a.stdout, "               else set its env keys; the election reads where the tool is routed,\n")
			fmt.Fprintf(a.stdout, "               not what OpenBox installed. `openbox init --provider claude-code --full`\n")
			fmt.Fprintf(a.stdout, "               installs it, or --remove-all clears the routing.\n")
		}
		fmt.Fprintf(a.stdout, "  log          %s\n", laneLogPath(lane.spec, home))
	}
	fmt.Fprintf(a.stdout, "\n  Installed is not recording. A lane can be reachable, configured and elected\n")
	fmt.Fprintf(a.stdout, "  while emitting nothing; no developer DID, or a posture key off. The log above\n")
	fmt.Fprintf(a.stdout, "  is the only place that says so.\n")
}

// reportSpool is where the machine-wide backlog is actionable, which
// `evidence_undelivered` is not: it counts carry-over files only.
func (a *app) reportSpool() {
	spool := hookflow.Spool{Dir: devconfig.SpoolDir(transportSpoolSubdir)}

	fmt.Fprintf(a.stdout, "\nSpooled evidence (waiting to reach the control plane)\n")
	fmt.Fprintf(a.stdout, "  directory    %s\n", spool.Dir)

	backlog := spool.BacklogCount()
	switch {
	case backlog == 0:
		fmt.Fprintf(a.stdout, "  waiting      0; delivery is self-triggering, so an empty queue is the healthy state\n")
	default:
		fmt.Fprintf(a.stdout, "  waiting      %d event(s), of which %d are in carry-over files from a failed\n", backlog, spool.UndeliveredCount())
		fmt.Fprintf(a.stdout, "               delivery. A lane daemon sweeps every %s; `openbox hook claude-code\n", hookflow.DefaultSweepInterval)
		fmt.Fprintf(a.stdout, "               flush` does it now.\n")
	}

	if discarded := spool.DiscardedCount(); discarded > 0 {
		fmt.Fprintf(a.stdout, "  DISCARDED    at least %d event(s) were given up on and are GONE: past %d delivery\n", discarded, hookflow.MaxRecoveryAttempts)
		fmt.Fprintf(a.stdout, "               attempts, or past the %d-day retention age. This is real loss of\n", int(hookflow.RetireSpoolAfter.Hours()/24))
		fmt.Fprintf(a.stdout, "               governance evidence, recorded in %s. \"At least\" because\n", spool.DiscardPath())
		fmt.Fprintf(a.stdout, "               that record is size-capped and restarts, so it is a floor.\n")
	}
	if backlog > 0 || spool.DiscardedCount() > 0 {
		fmt.Fprintf(a.stdout, "  flusher log  %s\n", spool.FlusherLogPath())
	}
}

// reportCoverage answers what absence cannot; docs/coverage.md §1b.
func (a *app) reportCoverage() {
	home := a.homeDir()
	settingsPath := gatewayservice.SettingsPath(home)

	fmt.Fprintf(a.stdout, "\nRouting durability (has anything un-routed a lane?)\n")
	switch coverage, err := activation.CoverageOf(home); {
	case err != nil:
		fmt.Fprintf(a.stdout, "  unknown      %v\n", err)
		fmt.Fprintf(a.stdout, "               Treat the lane lines above as unverified.\n")
	case len(coverage) == 0:
		fmt.Fprintf(a.stdout, "  n/a          no lane has written env keys on this machine\n")
	default:
		for _, c := range coverage {
			if c.Intact() {
				fmt.Fprintf(a.stdout, "  %-12s intact; all %d managed key(s) still as installed\n", c.Lane, len(c.Managed))
				continue
			}
			label := "CHANGED"
			if c.Vanished() {
				label = "UN-ROUTED"
			}
			fmt.Fprintf(a.stdout, "  %-12s %s: %s\n", c.Lane, label, c.Describe())
			fmt.Fprintf(a.stdout, "               `openbox init --provider claude-code --full` rewrites them.\n")
			fmt.Fprintf(a.stdout, "               Note: during an install this state is normal for a few seconds -\n")
			fmt.Fprintf(a.stdout, "               the daemon is started BEFORE its env keys are written, on purpose.\n")
		}
	}

	fmt.Fprintf(a.stdout, "\nClaude desktop app (a governed surface with no lane of its own)\n")
	// The machine's OWN port: the default would call a routed app unrouted.
	relayPort := activation.RelayPortFrom(activation.ReadSettingsEnv(settingsPath).Env)
	relayAddr := transport.DefaultAddr
	if relayPort == "" {
		relayPort = portOf(transport.DefaultAddr)
	} else {
		relayAddr = "127.0.0.1:" + relayPort
	}
	desktop := activation.InspectDesktop(context.Background(), relayPort)
	fmt.Fprintf(a.stdout, "  coverage     %s\n", desktop.Describe(relayAddr))
	if desktop.Note != "" {
		fmt.Fprintf(a.stdout, "  note         %s\n", desktop.Note)
	}
	fmt.Fprintf(a.stdout, "               Routing the desktop app is NOT implemented: how it resolves proxy\n")
	fmt.Fprintf(a.stdout, "               settings and a trust anchor is still an open question, so this line\n")
	fmt.Fprintf(a.stdout, "               reports coverage and claims nothing about how to fix it.\n")
}

func portOf(addr string) string {
	if _, port, err := net.SplitHostPort(addr); err == nil {
		return port
	}
	return ""
}
