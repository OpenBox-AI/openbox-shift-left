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

	fmt.Fprintf(a.stdout, "Posture (what decides a call on this machine)\n")
	flags := p.Flags()
	names := make([]string, 0, len(flags))
	width := 0
	allDefault := true
	for n := range flags {
		names = append(names, n)
		if len(n) > width {
			width = len(n) // derived, so a longer flag name cannot break the column
		}
		if devconfig.Source(p.ConfigSource[n]) != devconfig.SourceDefault {
			allDefault = false
		}
	}
	sort.Strings(names)
	// A machine nobody has touched is the common case, and eight rows saying
	// "from default" is eight rows of nothing. The moment one value comes from
	// anywhere else, every row is worth reading: which one moved, and from
	// where, is the whole question.
	if allDefault {
		var on, off []string
		for _, n := range names {
			if flags[n] {
				on = append(on, n)
				continue
			}
			off = append(off, n)
		}
		a.wrapRow("defaults", "ON %s", strings.Join(on, ", "))
		a.wrapRow("", "OFF %s", strings.Join(off, ", "))
	} else {
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
	}

	m := devconfig.Managed()
	switch {
	case !m.Present:
		a.row("managed", "%s", m.Path)
		a.row("", "absent; every setting above is developer-controlled")
	case !m.Readable:
		a.row("managed", "%s", m.Path)
		a.row("", "PRESENT BUT UNREADABLE; this machine is meant to be managed and is not.")
		fmt.Fprintf(a.stdout, "    Sessions fall back to developer-controlled settings. Fix the file (an unknown\n")
		fmt.Fprintf(a.stdout, "    key makes it unreadable, so check for typos in field names).\n")
	default:
		a.row("managed", "%s: active", m.Path)
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

	a.row("decided by", "%s", orUnset(p.DecisionAuthority))
	a.row("if offline", "%s", orUnset(p.FailurePolicy))
	if p.FailurePolicy == devconfig.FailurePolicyFailOpen {
		a.row("", "gated calls PROCEED when it cannot be reached; fail_closed denies")
	}
	a.row("last verdict", "%s", lastDecisionSummary())

	denials, denialsUnreadable := recentConfigDenials(5)
	switch {
	case denialsUnreadable:
		a.row("denials", "(unreadable)")
	case len(denials) == 0:
		a.row("denials", "none recorded")
	default:
		a.row("denials", "%d recent config change(s) denied, most recent first:", len(denials))
		for _, d := range denials {
			a.row("", "%s", d)
		}
	}

	a.reportReachability()
	for _, prov := range []managed.Provider{managed.ProviderClaudeCode, managed.ProviderCodex} {
		// The provider goes in the value: "claude-code cfg" is longer than the
		// label column, and a row that breaks the column to say one word is a
		// worse trade than a slightly longer value.
		a.row("provider cfg", "%s: %s", prov, managed.ProviderState(prov))
	}

	a.reportHookRegistration()

	a.reportGateway()
	a.reportLanes()
	a.reportCoverage()
	a.reportSpool()

	fmt.Fprintf(a.stdout, "\nOnly `managed` values prove anything: `user` and `env` can be changed by\n")
	fmt.Fprintf(a.stdout, "whoever runs this command, and only a deployed provider config survives a\n")
	fmt.Fprintf(a.stdout, "developer who does not want it.\n")
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
		a.row("org", "%s  (URLs; no agent identity)", withPresence(orgPath))
	}
	for _, name := range provider.Supported() {
		cfgPath, err := devconfig.DevConfigPathFor(name)
		if err != nil {
			a.row(name, "unreadable: %v", err)
			continue
		}
		cfg, _ := devconfig.Load(cfgPath)
		switch {
		case cfg.DID != "":
			a.row(name, "%s  DID %s", cfgPath, cfg.DID)
			a.reportURLDrift(name, orgCfg, cfg)
		default:
			a.row(name, "%s", withPresence(cfgPath))
			a.row("", "no agent; governing nothing. Run `openbox init --provider %s`", name)
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
	// Resolved values, not the stored strings. A config written before these
	// fields existed, or by hand, carries no URL at all -- and an absent value
	// resolves to the same built-in default the org may have written out in
	// full, so comparing what is on disk calls that a drift and prints an empty
	// string at the operator.
	for _, f := range []struct{ label, env, org, tool string }{
		{"core URL", devconfig.EnvBaseURL,
			devconfig.FirstNonEmpty(org.BaseURL, devconfig.DefaultBaseURL),
			devconfig.FirstNonEmpty(tool.BaseURL, devconfig.DefaultBaseURL)},
		// No default for the backend: an unset one resolves to nothing, so an
		// org value against an empty tool value is a real difference.
		{"backend URL", devconfig.EnvBackendURL, org.BackendURL, tool.BackendURL},
	} {
		if f.org == "" || f.org == f.tool {
			continue
		}
		// An exported value outranks both files, so neither is what the runtime
		// uses and "re-run init" would change nothing. The environment is
		// already named in the identity block above.
		if a.getenv(f.env) != "" {
			continue
		}
		a.row("", "%s differs from org (%s vs %s); re-run `openbox init --provider %s`",
			f.label, f.tool, f.org, name)
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
		a.row("in effect", "each tool's own files above")
		return
	}
	a.row("in effect", "%s (environment); this outranks every file above, for every tool",
		strings.Join(shadowing, ", "))
	if did := a.getenv(devconfig.EnvDID); did != "" {
		a.row("", "every governed tool reports as %s", did)
	}
}

// reportGateway four separate questions, kept separate on purpose.
func (a *app) reportGateway() {
	home := a.homeDir()
	r := gatewaycheck.Inspect(home, claudeManagedSettingsPath(), 750*time.Millisecond, a.getenv)

	// An unconfigured gateway is the expected state since the transport relay
	// superseded it, so it reports in two lines and stops. A machine that has
	// one still gets the whole section below.
	if r.SettingsPath == "" {
		fmt.Fprintf(a.stdout, "\nLocal gateway\n")
		a.row("configured", "no; ANTHROPIC_BASE_URL is not set in any settings file")
		a.row("bypass", "%s", map[bool]string{true: "DETECTABLE, not prevented", false: "no exposure found"}[r.BypassCapable])
		// The reason is the whole of that claim, and this is now the common
		// machine, so dropping it would make "DETECTABLE" unreachable. Safe to
		// wrap here, unlike the configured branch: the notes that lead with a
		// settings path are only appended when there IS one.
		for _, note := range r.BypassNotes {
			a.wrapRow("", "- %s", note)
		}
		return
	}

	fmt.Fprintf(a.stdout, "\nLocal gateway (model-call governance)\n")

	{
		a.row("configured", "%s", r.ConfiguredAddr)
		a.row("from", "%s", r.SettingsPath)
		owner := "uid " + strconv.Itoa(r.OwnerUID)
		switch r.OwnerUID {
		case 0:
			owner = "root"
		case -1:
			owner = "unknown (this OS exposes no owner to check)"
		}
		a.row("owned by", "%s", owner)
		a.row("tier", "%s", r.Tier)
		if !r.TargetsGateway {
			a.row("target", "NOT loopback; this machine is pointed at something else")
		}
		if r.Alive {
			a.row("reachable", "yes")
		} else {
			a.row("reachable", "NO; %s", r.AliveErr)
			a.row("", "model calls will FAIL rather than escape, which is the safe")
			a.row("", "direction. Start the gateway: `openbox gateway`")
		}
		// So a differing env value is reported as information, never as a fault; the
		// file above is what the tool uses.
		if r.EnvDiffersFromSettings {
			a.row("environment", "ANTHROPIC_BASE_URL=%s is also set here, and DIFFERS", r.EnvValue)
			a.row("", "The settings file above takes precedence for Claude Code, so the")
			a.row("", "file is what the tool uses. Confirm with `/status` in a session.")
		} else if r.EnvValue != "" {
			a.row("environment", "agrees (ANTHROPIC_BASE_URL=%s)", r.EnvValue)
		} else {
			fmt.Fprintf(a.stdout, "  verify with  `/status` in a Claude Code session; it prints the base URL\n")
			a.row("", "actually in force. doctor reads the file, which is the source")
			a.row("", "that wins, but only the session can confirm what it resolved.")
		}
		a.row("log", "%s", gatewayservice.LogPath(home))
	}

	a.row("bypass", "%s", map[bool]string{true: "DETECTABLE, not prevented", false: "no exposure found"}[r.BypassCapable])
	for _, note := range r.BypassNotes {
		// Not wrapped: two of these notes lead with the settings path, and the
		// managed path on macOS ("/Library/Application Support/...") carries a
		// space -- so wrapping would break the path across two lines. A long
		// line is the better failure.
		a.row("", "- %s", note)
	}
}

func (a *app) reportLanes() {
	home := a.homeDir()
	settingsPath := gatewayservice.SettingsPath(home)
	election := activation.ResolveElection(settingsPath)

	fmt.Fprintf(a.stdout, "\nLanes (which one emits model-call turns, and are they up)\n")
	undecidable := election.SettingsProblem != ""
	switch {
	case undecidable:
		a.row("elected", "CANNOT BE DECIDED; %s", election.SettingsProblem)
		a.row("", "This is NOT the same as no lane being routed. Nothing here knows")
		a.row("", "what this machine is configured to do, so treat every lane line")
		a.row("", "below as unverified. They are still printed: whether a unit is")
		a.row("", "installed and whether anything is listening do not come from the")
		a.row("", "settings file, and they are what recovery starts from.")
	case election.Elected == "":
		a.row("elected", "(none); %s", election.Reason)
		a.row("", "No lane emits model-call turns, so token counts and costs for this")
		a.row("", "machine are ABSENT rather than merely incomplete.")
	default:
		a.row("elected", "%s", election.Elected)
		a.wrapRow("because", "%s", election.Reason)
	}
	for _, lane := range election.Routed {
		if !slices.Contains(election.Candidates, lane) {
			fmt.Fprintf(a.stdout, "  NOT IN PATH  %s is configured but cannot see this machine's model calls -\n", lane)
			a.row("", "ANTHROPIC_BASE_URL sends them somewhere it does not intercept.")
		}
	}

	// A lane with nothing wrong is one row; a lane with something wrong keeps
	// the whole section it always had. Which means every line printed here is
	// either a coordinate or a problem, and the reader never learns to skip a
	// block because it is usually fine.
	var unhealthy []laneCheck
	for _, lane := range []laneCheck{
		{name: "telemetry", spec: laneservice.Telemetry(telemetry.DefaultAddr, "", false), addr: telemetry.DefaultAddr},
		{name: "transport", spec: laneservice.Transport(transport.DefaultAddr, "", false), addr: transport.DefaultAddr},
	} {
		lane.unit = lane.spec.UnitPath(runtime.GOOS, home)
		lane.routed = slices.Contains(election.Routed, activation.Lane(lane.name))
		lane.inPath = slices.Contains(election.Candidates, activation.Lane(lane.name))
		lane.listening, _ = portOccupied(lane.addr)
		if !lane.healthy(undecidable) {
			unhealthy = append(unhealthy, lane)
			continue
		}
		state := "routed"
		if election.Elected == activation.Lane(lane.name) {
			state = "routed, ELECTED"
		}
		a.row(lane.name, "listening on %s; %s", lane.addr, state)
	}
	// One line rather than the three it used to take, because it is a standing
	// caveat and not a finding: a lane can be up, routed and elected and still
	// emit nothing, with no developer DID or a posture key off.
	a.note("A reachable lane can still be recording nothing; its log says which.")
	// The one lane coordinate a healthy machine still needs by name: this is
	// the certificate the intercepted handshakes are signed with, and nothing
	// else prints it now that `init` reports lanes as a summary.
	if openboxHome, err := devconfig.Home(); err == nil {
		if caPath, _ := transport.CAPaths(openboxHome); fileExists(caPath) {
			a.row("relay CA", "%s", caPath)
		}
	}

	for _, lane := range unhealthy {
		fmt.Fprintf(a.stdout, "\n%s lane\n", strings.ToUpper(lane.name[:1])+lane.name[1:])
		switch {
		case lane.unit == "":
			a.row("unit", "(no daemon packaging on %s)", runtime.GOOS)
		case fileExists(lane.unit):
			a.row("unit", "%s", lane.unit)
		default:
			a.row("unit", "not installed")
		}
		a.row("configured", "%s", map[bool]string{true: "yes; " + settingsPath, false: "no; the tool is not pointed at it"}[lane.routed])
		if lane.listening {
			a.row("reachable", "yes (%s)", lane.addr)
		} else {
			a.row("reachable", "NO; nothing is listening on %s", lane.addr)
			if lane.routed {
				a.row("", "The tool is pointed at a port with nothing behind it.")
			}
		}
		// Lane-gated: laneCapable is provider.ClaudeCode only (initlanes.go), so
		// Codex has no lanes to elect and cannot reach this branch. The literal
		// "claude-code" below is not a missed provider case -- leave it.
		if election.Elected == activation.Lane(lane.name) && !lane.listening {
			a.row("WARNING", "this lane is ELECTED but nothing is listening, so NO lane is emitting")
			a.row("", "model-call turns on this machine. If you did not install it, something")
			a.row("", "else set its env keys; the election reads where the tool is routed,")
			a.row("", "not what OpenBox installed. `openbox init --provider claude-code`")
			a.row("", "brings it back up; `openbox uninstall` clears the routing.")
		}
		a.row("log", "%s", laneLogPath(lane.spec, home))
	}
}

// laneCheck is one lane's four observable facts, gathered before anything is
// printed so the same answer decides both the summary row and whether the full
// section is printed at all.
type laneCheck struct {
	name string
	spec laneservice.Spec
	addr string
	unit string

	routed    bool
	inPath    bool
	listening bool
}

// healthy is a lane doing its job: installed, pointed at, able to see the
// calls, and answering. An undecidable election makes every one of these
// unverified, so nothing is summarized away.
func (l laneCheck) healthy(undecidable bool) bool {
	return !undecidable && l.unit != "" && fileExists(l.unit) &&
		l.routed && l.inPath && l.listening
}

// reportSpool is where the machine-wide backlog is actionable, which
// `evidence_undelivered` is not: it counts carry-over files only.
func (a *app) reportSpool() {
	spool := hookflow.Spool{Dir: devconfig.SpoolDir(transportSpoolSubdir)}

	fmt.Fprintf(a.stdout, "\nSpooled evidence (waiting to reach the control plane)\n")
	a.row("directory", "%s", spool.Dir)

	backlog := spool.BacklogCount()
	switch {
	case backlog == 0:
		a.row("waiting", "0; delivery is self-triggering, so an empty queue is the healthy state")
	default:
		a.row("waiting", "%d event(s), of which %d are in carry-over files from a failed", backlog, spool.UndeliveredCount())
		a.row("", "delivery. A lane daemon sweeps every %s; `openbox hook claude-code", hookflow.DefaultSweepInterval)
		a.row("", "flush` does it now.")
	}

	discarded := spool.DiscardedCount()
	if discarded > 0 {
		a.row("DISCARDED", "at least %d event(s) were given up on and are GONE: past %d delivery", discarded, hookflow.MaxRecoveryAttempts)
		a.row("", "attempts, or past the %d-day retention age. This is real loss of", int(hookflow.RetireSpoolAfter.Hours()/24))
		a.row("", "governance evidence, recorded in %s. \"At least\" because", spool.DiscardPath())
		a.row("", "that record is size-capped and restarts, so it is a floor.")
	}
	if backlog > 0 || discarded > 0 {
		fmt.Fprintf(a.stdout, "  flusher log  %s\n", spool.FlusherLogPath())
	}

	a.reportCodexSpool(spool.Dir)
}

// reportCodexSpool is the additive half of the spool section (R4/R5): a
// Codex-only machine spools to "codex-spool", never "cc-spool", so the block
// above -- reading only the cc-spool directory -- reports a healthy empty
// queue while a real backlog waits in a directory it never looked at. It
// renders nothing, not even a directory row, when Codex's resolved spool path
// matches ccDir (OPENBOX_SPOOL_DIR overrides the whole path for every
// provider, so both resolve to the same directory; comparing resolved paths
// keeps that machine from double-counting its own backlog) or when Codex's
// spool has nothing waiting or discarded -- so a Claude-Code-only machine's
// output is byte-identical to before this existed (R5).
func (a *app) reportCodexSpool(ccDir string) {
	codexSpool := hookflow.Spool{Dir: providers.CodexSpoolDir()}
	if filepath.Clean(codexSpool.Dir) == filepath.Clean(ccDir) {
		return
	}

	backlog := codexSpool.BacklogCount()
	discarded := codexSpool.DiscardedCount()
	if backlog == 0 && discarded == 0 {
		return
	}

	// The row label names the owning provider (constraint: the remediation
	// must say who owns a non-zero backlog), mirroring how reportIdentities and
	// reportStoreReachability label multi-provider rows above.
	a.row(string(provider.Codex), "%s", codexSpool.Dir)
	switch {
	case backlog == 0:
		a.row("", "waiting 0; delivery is self-triggering, so an empty queue is the healthy state")
	default:
		a.row("", "waiting %d event(s), of which %d are in carry-over files from a failed delivery.", backlog, codexSpool.UndeliveredCount())
		a.row("", "A lane daemon sweeps every %s; `openbox hook %s flush` does it now.", hookflow.DefaultSweepInterval, provider.Codex)
	}
	if discarded > 0 {
		a.row("", "DISCARDED at least %d event(s): past %d delivery attempts, or past the %d-day", discarded, hookflow.MaxRecoveryAttempts, int(hookflow.RetireSpoolAfter.Hours()/24))
		a.row("", "retention age. Recorded in %s.", codexSpool.DiscardPath())
	}
	fmt.Fprintf(a.stdout, "  flusher log  %s\n", codexSpool.FlusherLogPath())
}

// reportCoverage answers what absence cannot; docs/coverage.md §1b.
func (a *app) reportCoverage() {
	home := a.homeDir()
	settingsPath := gatewayservice.SettingsPath(home)

	fmt.Fprintf(a.stdout, "\nCoverage (has anything un-routed a lane, and what is not routed at all)\n")
	switch coverage, err := activation.CoverageOf(home); {
	case err != nil:
		a.row("unknown", "%v", err)
		a.row("", "Treat the lane lines above as unverified.")
	case len(coverage) == 0:
		a.row("n/a", "no lane has written env keys on this machine")
	case allIntact(coverage):
		// Nothing has been un-routed, which is one fact however many lanes
		// there are. A lane that HAS been changed still gets its own lines.
		a.row("intact", "every managed key on this machine is still as installed")
	default:
		for _, c := range coverage {
			if c.Intact() {
				a.row(string(c.Lane), "intact; all %d managed key(s) still as installed", len(c.Managed))
				continue
			}
			label := "CHANGED"
			if c.Vanished() {
				label = "UN-ROUTED"
			}
			// Lane-gated: laneCapable is provider.ClaudeCode only (initlanes.go), so
			// every entry in `coverage` is a Claude Code lane and the literal
			// "claude-code" below is not a missed provider case -- leave it.
			a.row(string(c.Lane), "%s: %s", label, c.Describe())
			a.row("", "`openbox init --provider claude-code` rewrites them.")
			a.row("", "Note: during an install this state is normal for a few seconds -")
			a.row("", "the daemon is started BEFORE its env keys are written, on purpose.")
		}
	}

	// The machine's OWN port: the default would call a routed app unrouted.
	relayPort := activation.RelayPortFrom(activation.ReadSettingsEnv(settingsPath).Env)
	relayAddr := transport.DefaultAddr
	if relayPort == "" {
		relayPort = portOf(transport.DefaultAddr)
	} else {
		relayAddr = "127.0.0.1:" + relayPort
	}
	desktop := activation.InspectDesktop(context.Background(), relayPort)
	a.wrapRow("desktop app", "%s", desktop.Describe(relayAddr))
	if desktop.Note != "" {
		a.wrapRow("", "%s", desktop.Note)
	}
	// The three lines that used to follow said only that routing it is not
	// implemented, on every machine, forever. The coverage line above already
	// reports the gap; how to close it is not a fact about this machine.
}

// allIntact reports that no lane's managed env keys have been changed since
// they were installed.
func allIntact(coverage []activation.Coverage) bool {
	for _, c := range coverage {
		if !c.Intact() {
			return false
		}
	}
	return true
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
			a.row(level.label, "%s: could not be read; %v", level.path, err)
			continue
		case !audit.Present, len(audit.Engines) == 0:
			note := "(present, no OpenBox hooks)"
			if !audit.Present {
				note = "(absent)"
			}
			a.row(level.label, "%s  %s", level.path, note)
			if level.label == "user-wide" {
				if codexPath, present := codexHooksPresent(); present {
					fmt.Fprintf(a.stdout, "    Codex is governed; Claude Code is not. Codex hooks: %s\n", codexPath)
					fmt.Fprintf(a.stdout, "    `openbox init --provider %s` refreshes it.\n", provider.Codex)
				} else {
					fmt.Fprintf(a.stdout, "    Nothing is governed on this machine. Run `openbox init --provider %s`.\n", provider.ClaudeCode)
				}
			}
			continue
		}
		a.row(level.label, "%s  (present)", level.path)
		for _, engine := range audit.Engines {
			// Kept even for a single engine: which binary is governing this
			// machine is a coordinate, not commentary, and
			// TestDoctorDoesNotWarnOnASingleEngine exists to say so.
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

// codexHooksPresent reports whether the Codex hooks file carries an OpenBox
// registration, and where it lives, so reportHookRegistration's "nothing is
// governed" branch can name Codex instead of assuming Claude Code is the only
// tool a machine can be governed by (R4). Built on the same path providers
// already exposes (CodexHooksPath) and the same ownership parse uninstall
// already uses (carriesHookRegistration), so a renamed marker or a moved
// hooks file cannot drift between the two call sites.
func codexHooksPresent() (path string, present bool) {
	path = providers.CodexHooksPath()
	return path, carriesHookRegistration(string(provider.Codex), path)
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
	a.row("can they run", "%s", state.summary)
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
		a.row(tool, "NOT CHECKED; %v", err)
		a.row("", "Run `openbox init --provider %s`. Until then its hooks fire, fail", tool)
		a.row("", "to resolve credentials, and fail open -- governing nothing, silently.")
		return
	}

	c, err := client.New(client.Config{
		BaseURL:       creds.BaseURL,
		APIKey:        creds.APIKey,
		DID:           creds.DID,
		PrivateKeyB64: creds.PrivateKeyB64,
	})
	if err != nil {
		a.row(tool, "NOT CHECKED; the local credentials are unusable: %v", err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), doctorReachTimeout)
	defer cancel()
	if err := c.Validate(ctx); err != nil {
		// Status and guidance only. The error never carries the key, the seed,
		// the nonce or the signature (INV-1).
		a.row(tool, "NO; %v", err)
		a.row("", "Events spool locally and deliver when this clears, so a short")
		a.row("", "outage costs nothing. `openbox doctor` re-checks.")
		return
	}
	a.row(tool, "reachable; authenticated as %s @ %s", creds.DID, creds.BaseURL)
}

// doctorReachTimeout keeps the check short. Doctor is a report, and a report
// that blocks on a dead endpoint is worse than one that says it timed out.
const doctorReachTimeout = 5 * time.Second
