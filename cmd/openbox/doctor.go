package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
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
	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/provider"
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
	a.reportIdentities()

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

	fmt.Fprintf(a.stdout, "\nRecent config-change denials (most recent first)\n")
	denials, denialsUnreadable := recentConfigDenials(5)
	switch {
	case denialsUnreadable:
		fmt.Fprintf(a.stdout, "  (unreadable)\n")
	case len(denials) == 0:
		fmt.Fprintf(a.stdout, "  (none recorded)\n")
	default:
		for _, d := range denials {
			fmt.Fprintf(a.stdout, "  %s\n", d)
		}
	}

	fmt.Fprintf(a.stdout, "\nProvider managed configuration\n")
	for _, prov := range []managed.Provider{managed.ProviderClaudeCode, managed.ProviderCodex} {
		state := managed.ProviderState(prov)
		fmt.Fprintf(a.stdout, "  %-12s %s\n", prov, state)
	}

	a.reportReachability()

	a.reportHookRegistration()

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

// lastDecisionSummary is switched onto the bounded tail reader (V6): its
// unmarshal, both format strings, and both fallbacks are unchanged, so its
// printed output for any file under enforcementTailBytes is byte-identical to
// the whole-file os.ReadFile version it replaces.
func lastDecisionSummary() string {
	lines := tailLines(hookflow.DefaultEnforcementPath(), enforcementTailBytes, 1)
	if len(lines) == 0 {
		return "(none recorded)"
	}
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

// configDenialToolKind matches configToolKind in
// internal/adapters/claude-code/configgate.go. Duplicated, not imported: this
// package cannot reach that adapter's unexported constant, and the value is a
// stable wire field already written to the shared enforcements.jsonl sink.
const configDenialToolKind = "config"

// configDenialScanLines bounds how many of the byte-capped window's raw lines
// recentConfigDenials walks backward through to find tool_kind=="config"
// entries. It only needs to exceed what enforcementTailBytes can ever hold
// (on the order of a hundred records, per the tail cap decision), so a
// generous round number costs nothing.
const configDenialScanLines = 4096

// recentConfigDenials reports the last n config-change denials (D6), reading
// through the same bounded tail helper lastDecisionSummary uses (V6): one
// sink, two readers. unreadable distinguishes a corrupt sink (every raw line
// failed to parse) from a healthy sink that simply has no config denials in
// its window, so the caller can print "(unreadable)" only for the former.
func recentConfigDenials(n int) (out []string, unreadable bool) {
	raw := tailLines(hookflow.DefaultEnforcementPath(), enforcementTailBytes, configDenialScanLines)
	if len(raw) == 0 {
		return nil, false
	}
	parsed := 0
	for i := len(raw) - 1; i >= 0 && len(out) < n; i-- {
		var rec struct {
			Timestamp       string `json:"ts"`
			ToolKind        string `json:"tool_kind"`
			AppliedDecision string `json:"applied_decision"`
			PolicyID        string `json:"policy_id"`
			Reason          string `json:"reason"`
		}
		if json.Unmarshal([]byte(raw[i]), &rec) != nil {
			continue // an unreadable line is skipped, not fatal to the section
		}
		parsed++
		if rec.ToolKind != configDenialToolKind {
			continue
		}
		out = append(out, fmt.Sprintf("%s  %s (policy %s): %s",
			rec.Timestamp, orUnset(rec.AppliedDecision), orUnset(rec.PolicyID), rec.Reason))
	}
	if parsed == 0 {
		return nil, true // every raw line failed to parse: the sink itself is corrupt
	}
	return out, false
}

func withPresence(path string) string {
	if _, err := os.Stat(path); err == nil {
		return path + "  (present)"
	}
	return path + "  (absent)"
}

// reportIdentities prints one row per store: the org config, which carries the
// coordinates every tool shares, and then each governed tool's own.
//
// This is the answer to "why did governance stop". A tool with no store is
// governing nothing, silently, and the only in-product place that says so is
// here -- so an absent row names the command that fixes it rather than just
// reporting a missing file.
//
// It reads through the explicit per-tool accessors and never binds. A bind
// held across these reads would be the one way every row could report the
// first tool's identity, and the differing DIDs are what would stop showing
// it.
func (a *app) reportIdentities() {
	var orgCfg devconfig.DevConfig
	orgPath, err := devconfig.DevConfigPathFor("")
	if err == nil {
		orgCfg, _ = devconfig.Load(orgPath)
		fmt.Fprintf(a.stdout, "  %-11s  %s  (URLs; no agent identity)\n", "org", withPresence(orgPath))
	}
	for _, name := range provider.Supported() {
		cfgPath, err := devconfig.DevConfigPathFor(name)
		if err != nil {
			fmt.Fprintf(a.stdout, "  %-11s  unreadable: %v\n", name, err)
			continue
		}
		cfg, _ := devconfig.Load(cfgPath)
		switch {
		case cfg.DID != "":
			fmt.Fprintf(a.stdout, "  %-11s  %s  DID %s\n", name, cfgPath, cfg.DID)
			a.reportURLDrift(name, orgCfg, cfg)
		default:
			fmt.Fprintf(a.stdout, "  %-11s  %s  no agent; governing nothing. Run `openbox init --provider %s`\n",
				name, withPresence(cfgPath), name)
		}
	}
	a.reportIdentitySource()
	fmt.Fprintln(a.stdout)
}

// reportURLDrift names a tool still pointing at coordinates the organization
// has since changed.
//
// `auth` writes the organization's URLs once and `init` copies them into each
// tool's own config, because one dev.json is loaded and never merged over
// another. That copy is a second store for two fields, so an `auth` re-run
// correcting a URL changes nothing for an already installed tool until that
// tool re-runs `init`. Nothing else would ever say so: the tool keeps posting
// to the old core, which answers 401, and a 401 never spends a delivery
// attempt -- so the spool grows quietly and no message anywhere names a URL.
//
// Only a difference is printed. A line on every row, for every healthy
// machine, is one people learn to skip.
func (a *app) reportURLDrift(name string, org, tool devconfig.DevConfig) {
	for _, f := range []struct{ label, org, tool string }{
		{"core URL", org.BaseURL, tool.BaseURL},
		{"backend URL", org.BackendURL, tool.BackendURL},
	} {
		if f.org == "" || f.org == f.tool {
			continue
		}
		fmt.Fprintf(a.stdout, "  %-11s  %s differs from org (%s vs %s); re-run `openbox init --provider %s`\n",
			"", f.label, f.tool, f.org, name)
	}
}

// reportIdentitySource names which source actually wins, because an exported
// variable outranks every file above and a reader comparing a dashboard to a
// dev.json would otherwise be comparing the wrong two things.
func (a *app) reportIdentitySource() {
	var shadowing []string
	for _, name := range []string{devconfig.EnvDID, devconfig.EnvAPIKeyDirect, devconfig.EnvAgentPrivateKey} {
		if a.getenv(name) != "" {
			shadowing = append(shadowing, name)
		}
	}
	if len(shadowing) == 0 {
		fmt.Fprintf(a.stdout, "  %-11s  each tool's own files above\n", "in effect")
		return
	}
	fmt.Fprintf(a.stdout, "  %-11s  %s (environment); this outranks every file above, for every tool\n",
		"in effect", strings.Join(shadowing, ", "))
	if did := a.getenv(devconfig.EnvDID); did != "" {
		fmt.Fprintf(a.stdout, "  %-11s  every governed tool reports as %s\n", "", did)
	}
}

// reportGateway four separate questions, kept separate on purpose.
func (a *app) reportGateway() {
	home := a.homeDir()
	r := gatewaycheck.Inspect(home, claudeManagedSettingsPath(), 750*time.Millisecond, a.getenv)

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
			fmt.Fprintf(a.stdout, "               not what OpenBox installed. `openbox init --provider claude-code`\n")
			fmt.Fprintf(a.stdout, "               brings it back up; `openbox uninstall` clears the routing.\n")
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

	discarded := spool.DiscardedCount()
	if discarded > 0 {
		fmt.Fprintf(a.stdout, "  DISCARDED    at least %d event(s) were given up on and are GONE: past %d delivery\n", discarded, hookflow.MaxRecoveryAttempts)
		fmt.Fprintf(a.stdout, "               attempts, or past the %d-day retention age. This is real loss of\n", int(hookflow.RetireSpoolAfter.Hours()/24))
		fmt.Fprintf(a.stdout, "               governance evidence, recorded in %s. \"At least\" because\n", spool.DiscardPath())
		fmt.Fprintf(a.stdout, "               that record is size-capped and restarts, so it is a floor.\n")
	}
	if backlog > 0 || discarded > 0 {
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
			fmt.Fprintf(a.stdout, "               `openbox init --provider claude-code` rewrites them.\n")
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

// reportHookRegistration covers both levels an OpenBox registration can live
// at, and the conditions that only exist between them.
//
// An install writes the user-wide file, so that one is the answer to "is this
// machine governed". A project file is ordinarily absent; when it is not, its
// entries are a second registration of the same gate, and where the two name
// different engine paths the tool de-duplicates neither.
func (a *app) reportHookRegistration() {
	fmt.Fprintf(a.stdout, "\nHook registration\n")

	type level struct{ label, path string }
	levels := []level{{"user-wide", providers.ClaudeUserSettingsPath()}}
	if wd, err := os.Getwd(); err != nil {
		fmt.Fprintf(a.stdout, "  current directory unreadable (%v); this project's file was not checked\n", err)
	} else {
		levels = append(levels, level{"this project", providers.ClaudeProjectSettingsPath(wd)})
	}

	engines := map[string][]string{} // engine path -> the levels registering it
	for _, level := range levels {
		audit, err := providers.AuditHooks(level.path)
		switch {
		case err != nil:
			fmt.Fprintf(a.stdout, "  %-13s %s: could not be read; %v\n", level.label, level.path, err)
			continue
		case !audit.Present, len(audit.Engines) == 0:
			note := "(present, no OpenBox hooks)"
			if !audit.Present {
				note = "(absent)"
			}
			fmt.Fprintf(a.stdout, "  %-13s %s  %s\n", level.label, level.path, note)
			if level.label == "user-wide" {
				fmt.Fprintf(a.stdout, "    Nothing is governed on this machine. Run `openbox init --provider claude-code`.\n")
			}
			continue
		}
		fmt.Fprintf(a.stdout, "  %-13s %s  (present)\n", level.label, level.path)
		for _, engine := range audit.Engines {
			fmt.Fprintf(a.stdout, "    engine  %s\n", engine)
			engines[engine] = append(engines[engine], level.label)
		}
		if len(audit.Engines) > 1 {
			fmt.Fprintf(a.stdout, "    WARNING: %d OpenBox engines are registered in this one file. Every hook\n", len(audit.Engines))
			fmt.Fprintf(a.stdout, "      fires once per engine, so every governed tool call is stored TWICE and\n")
			fmt.Fprintf(a.stdout, "      tool success rates and latencies are meaningless. An older engine also\n")
			fmt.Fprintf(a.stdout, "      omits fields the current one sends. Run `openbox init` to replace them.\n")
		}
		if len(audit.DuplicateEvents) > 0 {
			fmt.Fprintf(a.stdout, "    WARNING: registered more than once for: %s; same duplication.\n", strings.Join(audit.DuplicateEvents, ", "))
			fmt.Fprintf(a.stdout, "      Run `openbox init`.\n")
		}
	}

	a.reportCrossLevelHooks(engines)
	a.reportBlockedHooks()
}

// reportCrossLevelHooks is the condition neither file can see on its own.
//
// The same engine at both levels is benign: the commands are byte-identical
// and the tool runs a duplicated identical handler once. Different engine
// paths are the shape it will not de-duplicate, so each one fires and every
// governed call is stored once per engine.
func (a *app) reportCrossLevelHooks(engines map[string][]string) {
	if len(engines) == 0 {
		return
	}
	var shared, distinct []string
	for engine, levels := range engines {
		if len(levels) > 1 {
			shared = append(shared, engine)
		}
		distinct = append(distinct, engine)
	}
	sort.Strings(shared)
	sort.Strings(distinct)

	if len(distinct) > 1 {
		fmt.Fprintf(a.stdout, "    WARNING: %d different engine paths are registered across the two files:\n", len(distinct))
		for _, engine := range distinct {
			fmt.Fprintf(a.stdout, "      %s  (%s)\n", engine, strings.Join(engines[engine], ", "))
		}
		fmt.Fprintf(a.stdout, "      A project-level entry at a DIFFERENT engine path is not de-duplicated\n")
		fmt.Fprintf(a.stdout, "      against the user-wide one, so both fire and every governed tool call in\n")
		fmt.Fprintf(a.stdout, "      this project is stored TWICE. `openbox init` sweeps the project file;\n")
		fmt.Fprintf(a.stdout, "      `openbox uninstall` removes both.\n")
		return
	}
	if len(shared) == 1 {
		fmt.Fprintf(a.stdout, "    Both files register the same engine, so the tool runs it once. `openbox init`\n")
		fmt.Fprintf(a.stdout, "      removes the redundant project-level copy.\n")
	}
}

// reportBlockedHooks answers what absence cannot: on an org-managed machine the
// hooks can be installed, correct, and never run. That is the failure mode
// worth the most here, because it is silent and total -- the install prints
// success and doctor, reading none of these keys, used to agree.
func (a *app) reportBlockedHooks() {
	state := resolveHookBlock()
	fmt.Fprintf(a.stdout, "  %-13s %s\n", "can they run", state.summary)
	for _, line := range state.detail {
		fmt.Fprintf(a.stdout, "    %s\n", line)
	}
}

// reportReachability answers the question a separate verify command used to, in the
// place people actually look when something is wrong.
//
// It is the first thing in doctor that touches the network, which makes its
// failure behaviour the design: doctor is what you run WHEN the control plane
// is unreachable, so an unreachable one has to be a reported line and never a
// non-zero exit that suppresses everything below it. The timeout is bounded for
// the same reason -- a doctor that hangs tells you nothing at all.
func (a *app) reportReachability() {
	fmt.Fprintf(a.stdout, "\nControl plane\n")
	// One line per store, and one store's failure never suppresses another's:
	// a machine with claude-code working and codex uninstalled is a normal
	// machine, and reporting only the first would hide whichever one is broken.
	for _, name := range provider.Supported() {
		a.reportStoreReachability(name)
	}
}

func (a *app) reportStoreReachability(tool string) {
	creds, err := devconfig.ResolveCredentialsFor(tool)
	if err != nil {
		fmt.Fprintf(a.stdout, "  %-11s NOT CHECKED; %v\n", tool, err)
		fmt.Fprintf(a.stdout, "  %-11s Run `openbox init --provider %s`. Until then this tool's hooks fire,\n", "", tool)
		fmt.Fprintf(a.stdout, "  %-11s fail to resolve credentials, and fail open -- governing nothing, silently.\n", "")
		return
	}

	c, err := client.New(client.Config{
		BaseURL:       creds.BaseURL,
		APIKey:        creds.APIKey,
		DID:           creds.DID,
		PrivateKeyB64: creds.PrivateKeyB64,
	})
	if err != nil {
		fmt.Fprintf(a.stdout, "  %-11s NOT CHECKED; the local credentials are unusable: %v\n", tool, err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), doctorReachTimeout)
	defer cancel()
	if err := c.Validate(ctx); err != nil {
		// Status and guidance only. The error never carries the key, the seed,
		// the nonce or the signature (INV-1).
		fmt.Fprintf(a.stdout, "  %-11s NO; %v\n", tool, err)
		fmt.Fprintf(a.stdout, "  %-11s Events spool locally and deliver when this clears, so a short\n", "")
		fmt.Fprintf(a.stdout, "  %-11s outage costs nothing. `openbox doctor` re-checks.\n", "")
		return
	}
	fmt.Fprintf(a.stdout, "  %-11s reachable; authenticated as %s @ %s\n", tool, creds.DID, creds.BaseURL)
}

// doctorReachTimeout keeps the check short. Doctor is a report, and a report
// that blocks on a dead endpoint is worse than one that says it timed out.
const doctorReachTimeout = 5 * time.Second
