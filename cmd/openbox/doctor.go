package main

import (
	"encoding/json"
	"fmt"
	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

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

// runDoctor prints the effective posture and, for each flag, where the value came
// from (E8-S9).
//
// The provenance is the point. "enforce: true" alone does not tell an operator
// whether the org requires it or this developer happens to have switched it on,
// and those are different facts about a fleet. It reports exactly what the
// session posture reports, so what a developer sees locally is what the control
// plane sees — a divergence between the two would make both untrustworthy.
func (a *app) runDoctor(args []string) int {
	fs := a.newFlagSet("doctor")
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}

	p := devconfig.EffectivePosture()
	fmt.Fprintf(a.stdout, "OpenBox developer-runtime posture\n\n")

	// Which identities this machine holds, and which file each posture below
	// came from. A machine can hold both a governed developer runtime and an
	// approver; naming the files is how a reader knows the posture reported
	// here is the one the hooks actually read.
	fmt.Fprintf(a.stdout, "Identity\n")
	fmt.Fprintf(a.stdout, "  developer  %s\n", withPresence(devconfig.DefaultConfigPath()))
	fmt.Fprintf(a.stdout, "  approver   %s\n", withPresence(devconfig.DefaultApproverConfigPath()))
	fmt.Fprintf(a.stdout, "  Everything below is the DEVELOPER posture: hooks read that file and no\n")
	fmt.Fprintf(a.stdout, "  other. The approver config belongs to a different principal.\n\n")

	fmt.Fprintf(a.stdout, "Enforcement\n")
	// Read the flags off the posture rather than re-listing them here: a
	// hand-written list is how `require_verified_bundle` shipped as a real
	// control that this command never mentioned.
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
			note = "  (org mandate — not overridable here)"
		case devconfig.SourceManagedDefault:
			note = "  (org default — overridable)"
		}
		fmt.Fprintf(a.stdout, "  %-*s %-5v  from %s%s\n", width, n, flags[n], source, note)
	}

	fmt.Fprintf(a.stdout, "\nManaged OpenBox config\n")
	m := devconfig.Managed()
	switch {
	case !m.Present:
		fmt.Fprintf(a.stdout, "  %s: absent — every setting above is developer-controlled\n", m.Path)
	case !m.Readable:
		fmt.Fprintf(a.stdout, "  %s: PRESENT BUT UNREADABLE — this machine is meant to be managed and is not.\n", m.Path)
		fmt.Fprintf(a.stdout, "    Sessions fall back to developer-controlled settings. Fix the file (an unknown\n")
		fmt.Fprintf(a.stdout, "    key makes it unreadable, so check for typos in field names).\n")
	default:
		fmt.Fprintf(a.stdout, "  %s: active\n", m.Path)
		if len(m.Locked) == 0 {
			fmt.Fprintf(a.stdout, "    locked: (none) — values act as org defaults the developer may override\n")
		} else {
			fmt.Fprintf(a.stdout, "    locked: %v\n", m.Locked)
		}
		if len(m.UnknownKeys) > 0 {
			fmt.Fprintf(a.stdout, "    WARNING: unrecognized keys, ignored: %v\n", m.UnknownKeys)
			fmt.Fprintf(a.stdout, "      They set nothing. Check the spelling against dev.json's field\n")
			fmt.Fprintf(a.stdout, "      names — an org that misspells a field believes it mandated something.\n")
		}
		if len(m.UnknownLocked) > 0 {
			fmt.Fprintf(a.stdout, "    WARNING: locked names no setting recognizes: %v — these lock NOTHING.\n", m.UnknownLocked)
			fmt.Fprintf(a.stdout, "      Check the spelling against the dev.json field names; a typo here is a\n")
			fmt.Fprintf(a.stdout, "      mandate the org believes is in force and is not.\n")
		}
	}

	// Policy provenance, in place of the bundle block. The heading changed
	// because the claim changed: there is no local artifact to hash or verify,
	// so reporting a "bundle integrity" would be describing a check that no
	// longer runs.
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

	// The LOCAL counterpart to the managed state above, and the only place a
	// second registration is visible at all: while two engines were registered,
	// every hook fired once per engine and every governed tool call was stored
	// twice, with no warning anywhere and no way to tell it from a client defect.
	fmt.Fprintf(a.stdout, "\nProject hook registration\n")
	if wd, err := os.Getwd(); err != nil {
		fmt.Fprintf(a.stdout, "  current directory unreadable (%v) — check skipped\n", err)
	} else {
		audit, err := providers.AuditProjectHooks(wd)
		switch {
		case err != nil:
			fmt.Fprintf(a.stdout, "  %s: could not be read — %v\n", audit.SettingsPath, err)
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
				fmt.Fprintf(a.stdout, "    WARNING: registered more than once for: %s — same duplication.\n", strings.Join(audit.DuplicateEvents, ", "))
				fmt.Fprintf(a.stdout, "      Run `openbox init` in this directory.\n")
			}
		}
	}

	a.reportGateway()
	a.reportLanes()

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

// lastDecisionSummary reads the most recent line of the local enforcement audit
// and says which policy decided it — the local view of policy provenance now
// that there is no bundle to name.
//
// It reports the SOURCE as well as the policy id, because the two answer
// different questions and only together are they honest: a line with no policy
// id and an `evaluate:fail-open` source means the control plane was unreachable
// and the call was decided by the failure policy, which is the one case where no
// policy decided anything at all. Printing an empty policy id alone would read
// as "unknown" rather than as "ungoverned".
//
// Best-effort: an absent or unreadable audit is the normal state on a machine
// that has not run a governed session, not a fault.
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
		return fmt.Sprintf("%s via %s — NO policy decided this call", orUnset(rec.Verdict), orUnset(rec.Source))
	}
	return fmt.Sprintf("policy %s (%s via %s)", rec.PolicyID, orUnset(rec.Verdict), orUnset(rec.Source))
}

// withPresence annotates a config path with whether it exists — an absent
// approver file is the normal state on a developer's machine, not a fault.
func withPresence(path string) string {
	if _, err := os.Stat(path); err == nil {
		return path + "  (present)"
	}
	return path + "  (absent)"
}

// reportGateway prints the local gateway's detection-tier posture (that
// decision, phase 07 requirement 4).
//
// Four separate questions, kept separate on purpose. "Alive" and "actually used"
// are not the same claim — a gateway can be running perfectly while the tool is
// configured to talk straight to the provider — and conflating them is how a
// dashboard comes to show governance that is not happening.
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
		fmt.Fprintf(a.stdout, "  configured   no — ANTHROPIC_BASE_URL is not set in any settings file\n")
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
			fmt.Fprintf(a.stdout, "  target       NOT loopback — this machine is pointed at something else\n")
		}
		if r.Alive {
			fmt.Fprintf(a.stdout, "  reachable    yes\n")
		} else {
			fmt.Fprintf(a.stdout, "  reachable    NO — %s\n", r.AliveErr)
			fmt.Fprintf(a.stdout, "               model calls will FAIL rather than escape, which is the safe\n")
			fmt.Fprintf(a.stdout, "               direction. Start the gateway: `openbox gateway`\n")
		}
		// PRECEDENCE, stated because the first version of this block asserted the
		// reverse and was wrong: for Claude Code the SETTINGS FILE WINS over a shell
		// export (Anthropic's docs, llm-gateway-connect#set-in-a-settings-file). So
		// a differing env value is reported as INFORMATION, never as a fault — the
		// file above is what the tool uses.
		//
		// The remedy line matters more than any of it: `/status` inside a session
		// prints the base URL actually in force. That is the authoritative check,
		// and doctor cannot be — it sees its own environment, not the session's.
		if r.EnvDiffersFromSettings {
			fmt.Fprintf(a.stdout, "  environment  ANTHROPIC_BASE_URL=%s is also set here, and DIFFERS\n", r.EnvValue)
			fmt.Fprintf(a.stdout, "               The settings file above takes precedence for Claude Code, so the\n")
			fmt.Fprintf(a.stdout, "               file is what the tool uses. Confirm with `/status` in a session.\n")
		} else if r.EnvValue != "" {
			fmt.Fprintf(a.stdout, "  environment  agrees (ANTHROPIC_BASE_URL=%s)\n", r.EnvValue)
		} else {
			fmt.Fprintf(a.stdout, "  verify with  `/status` in a Claude Code session — it prints the base URL\n")
			fmt.Fprintf(a.stdout, "               actually in force. doctor reads the file, which is the source\n")
			fmt.Fprintf(a.stdout, "               that wins, but only the session can confirm what it resolved.\n")
		}
		// Where the daemon's own diagnostics are. Named because it is the only
		// place the gateway says it is RELAYING BUT NOT RECORDING — a missing DID,
		// or relayed calls carrying no session header — and none of the four
		// questions above asks that. `--verbose` (or `init --gateway-verbose`)
		// turns it into a per-call record of what actually arrived.
		fmt.Fprintf(a.stdout, "  log          %s\n", gatewayservice.LogPath(home))
	}

	// Always printed, including in the healthiest case. The base assurance claim
	// is DETECTION, and a check that stays silent when everything looks fine
	// trains a reader to believe silence means prevention.
	fmt.Fprintf(a.stdout, "  bypass       %s\n", map[bool]string{true: "DETECTABLE, not prevented", false: "no exposure found"}[r.BypassCapable])
	for _, note := range r.BypassNotes {
		fmt.Fprintf(a.stdout, "               - %s\n", note)
	}
}

// managedSettingsPathForDoctor resolves the provider's managed-settings file, or
// "" where this build has no path for the OS. Reached through the managed package
// so doctor and `managed install` cannot disagree about where the file lives.
func managedSettingsPathForDoctor() string {
	dir := managed.ClaudeCodeManagedDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "managed-settings.json")
}

// reportLanes prints the model-call producer election and the two lanes that
// decision added, per lane and separately from each other.
//
// THE ELECTION LINE IS THE POINT. The precedence is automatic and the developer
// never chose it, so a lane can be perfectly installed, perfectly reachable, and
// emitting nothing — which is exactly the "configured but not in force" shape
// that decision promised would always be detectable. It resolves through the
// SAME function the telemetry daemon uses, deliberately: a check and the thing
// it checks must not be able to disagree about what "elected" means. That is the
// lesson from the duplicate-hook-engine repair, where the diagnostic and the fix
// were built on one classifier on purpose.
//
// "Installed" and "reachable" stay separate for the same reason doctor keeps
// them apart for the gateway: a unit on disk is not a process serving, and
// conflating them is how a dashboard comes to show governance that is not
// happening. Neither answers "is it RECORDING" — the lane log is where that
// lives, so it is named every time.
func (a *app) reportLanes() {
	home := a.homeDir()
	settingsPath := gatewayservice.SettingsPath(home)
	election := activation.ResolveElection(settingsPath)

	fmt.Fprintf(a.stdout, "\nModel-call producer (which lane emits turn events)\n")
	if election.Elected == "" {
		fmt.Fprintf(a.stdout, "  elected      (none) — %s\n", election.Reason)
		fmt.Fprintf(a.stdout, "               No lane emits model-call turns, so token counts and costs for this\n")
		fmt.Fprintf(a.stdout, "               machine are ABSENT rather than merely incomplete.\n")
	} else {
		fmt.Fprintf(a.stdout, "  elected      %s\n", election.Elected)
		fmt.Fprintf(a.stdout, "  because      %s\n", election.Reason)
	}
	if len(election.Routed) > 1 {
		fmt.Fprintf(a.stdout, "  routed       %v — exactly one of these emits; the others still send their own\n", election.Routed)
		fmt.Fprintf(a.stdout, "               non-turn evidence, which does not collide.\n")
	}
	// ROUTED and IN PATH are different facts, and only one state separates them:
	// a base URL sends the call past the relay. Reporting a lane as configured
	// while saying plainly that it cannot see this machine's calls is the honest
	// pair; collapsing them would either hide a configured lane or promise
	// observation that is not happening.
	for _, lane := range election.Routed {
		if !containsLaneName(election.Candidates, lane) {
			fmt.Fprintf(a.stdout, "  NOT IN PATH  %s is configured but cannot see this machine's model calls —\n", lane)
			fmt.Fprintf(a.stdout, "               ANTHROPIC_BASE_URL sends them somewhere it does not intercept.\n")
		}
	}

	for _, lane := range []struct {
		name string
		spec laneservice.Spec
		addr string
	}{
		{"telemetry", laneservice.Telemetry(telemetry.DefaultAddr, false), telemetry.DefaultAddr},
		{"transport", laneservice.Transport(transport.DefaultAddr, false), transport.DefaultAddr},
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
		// Routed is read off the settings file rather than off the unit, because
		// they answer different questions: a unit says a daemon exists, the
		// settings say the tool will actually talk to it.
		// Configured is the ROUTED set: what the tool points at, regardless of
		// whether this lane can win the election or even see the call.
		routed := containsLaneName(election.Routed, activation.Lane(lane.name))
		fmt.Fprintf(a.stdout, "  configured   %s\n", map[bool]string{true: "yes — " + settingsPath, false: "no — the tool is not pointed at it"}[routed])
		occupied, _ := portOccupied(lane.addr)
		if occupied {
			fmt.Fprintf(a.stdout, "  reachable    yes (%s)\n", lane.addr)
		} else {
			fmt.Fprintf(a.stdout, "  reachable    NO — nothing is listening on %s\n", lane.addr)
			if routed {
				fmt.Fprintf(a.stdout, "               The tool is pointed at a port with nothing behind it.\n")
			}
		}
		// ELECTED BUT ABSENT is the election's own worst failure, and it is
		// otherwise silent. The election reads the tool's ROUTING, so a
		// developer's own loopback proxy — or a stale key left by hand — can
		// elect a lane that OpenBox never installed. Every other lane then
		// correctly stands down, and the machine reports no model calls at all
		// while each individual line above still reads as fine. Only putting the
		// two facts on one line makes it visible.
		if election.Elected == activation.Lane(lane.name) && !occupied {
			fmt.Fprintf(a.stdout, "  WARNING      this lane is ELECTED but nothing is listening, so NO lane is emitting\n")
			fmt.Fprintf(a.stdout, "               model-call turns on this machine. If you did not install it, something\n")
			fmt.Fprintf(a.stdout, "               else set its env keys — the election reads where the tool is routed,\n")
			fmt.Fprintf(a.stdout, "               not what OpenBox installed. `openbox init --provider claude-code --full`\n")
			fmt.Fprintf(a.stdout, "               installs it, or --remove-all clears the routing.\n")
		}
		// Named unconditionally: this is the only place a lane says it is running
		// perfectly and recording nothing, and launchd sends stdio to /dev/null
		// unless the unit says otherwise — which ours does.
		fmt.Fprintf(a.stdout, "  log          %s\n", laneLogPath(lane.spec, home))
	}
	fmt.Fprintf(a.stdout, "\n  Installed is not recording. A lane can be reachable, configured and elected\n")
	fmt.Fprintf(a.stdout, "  while emitting nothing — no developer DID, or a posture key off. The log above\n")
	fmt.Fprintf(a.stdout, "  is the only place that says so.\n")
}

// containsLaneName is a local membership test for doctor's two short lane lists.
func containsLaneName(lanes []activation.Lane, want activation.Lane) bool {
	for _, l := range lanes {
		if l == want {
			return true
		}
	}
	return false
}
