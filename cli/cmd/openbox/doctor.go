package main

import (
	"encoding/json"
	"fmt"
	"github.com/openbox-ai/openbox-shift-left/adapters/common/hookflow"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/openbox-ai/openbox-shift-left/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/gatewaycheck"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/gatewayservice"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/managed"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/providers"
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
	// longer runs (ADR-0017).
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

// reportGateway prints the local gateway's detection-tier posture (ADR-0021,
// phase 07 requirement 4).
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
		// THE FILE IS NOT THE EFFECTIVE VALUE, and saying so is the difference
		// between a true report and a confident false one. A real environment
		// variable beats the settings file, so every line above can describe a
		// correctly configured, reachable gateway that receives nothing —
		// which is what happened on a real machine whose tool carried the
		// provider URL in its own launch environment.
		if r.EnvOverridesSettings {
			fmt.Fprintf(a.stdout, "  EFFECTIVE    %s — from the environment, NOT the file above\n", r.EnvOverride)
			fmt.Fprintf(a.stdout, "               ANTHROPIC_BASE_URL is set in this process's environment and a real\n")
			fmt.Fprintf(a.stdout, "               environment variable OVERRIDES the settings file, so the file is\n")
			fmt.Fprintf(a.stdout, "               inert and model calls are NOT reaching the gateway. Everything\n")
			fmt.Fprintf(a.stdout, "               above describes configuration that is not in force.\n")
		} else if r.EnvOverride != "" {
			fmt.Fprintf(a.stdout, "  environment  agrees (ANTHROPIC_BASE_URL=%s)\n", r.EnvOverride)
		} else {
			// The honest limit: this is one process's environment, not the tool's.
			fmt.Fprintf(a.stdout, "  environment  ANTHROPIC_BASE_URL not set here — but this is `doctor`'s own\n")
			fmt.Fprintf(a.stdout, "               environment, and it cannot see the environment the tool was\n")
			fmt.Fprintf(a.stdout, "               launched with. A tool that sets the variable itself overrides\n")
			fmt.Fprintf(a.stdout, "               the file, so \"configured\" above is not proof of routing.\n")
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
