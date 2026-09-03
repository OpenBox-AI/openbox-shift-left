package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	obgit "github.com/openbox-ai/openbox-shift-left/internal/adapters/common/git"
	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/activation"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/gatewayservice"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/laneservice"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/providers"
	"github.com/openbox-ai/openbox-shift-left/internal/provider"
)

// uninstall is the full reversal of `openbox init`, in the reverse of install
// ordering: the gate stops firing first, then the daemons come down, then the
// artifacts go, and the credentials last.
//
// It detects what is installed rather than being told, because a machine that
// was installed by an older binary or a different flag set still has to come
// off, and because a `--provider` here would let an operator "uninstall" while
// leaving another provider's hooks firing.
//
// Two rules shape the output. Removal is effective immediately in both
// directions: Claude Code's file watcher picks up settings edits at once, so a
// successful removal ends governance of live sessions with no restart to wait
// for — and a surface whose removal FAILED keeps firing from the moment this
// command exits, against a machine whose credentials it just deleted. And the
// purge destroys evidence that cannot be recovered, so the loss is printed
// rather than implied.

// uninstallState accumulates what happened, so the final report can name
// consequences instead of implying a clean machine.
type uninstallState struct {
	deleted    int
	failed     bool
	hookFailed []hookFailure // surfaces this run could not clean
	laneFailed bool          // a lane whose deactivate conflicted: still routed, still running
	kept       []string      // org-owned files, reported and left
	backlog    int           // events in the spool that no flush could deliver
}

func (a *app) runUninstall(args []string) int {
	fs := a.newFlagSet("uninstall")
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}
	if fs.NArg() > 0 {
		return a.errorf("`openbox uninstall` takes no arguments: it removes whatever is installed on this "+
			"machine, for every provider. Got %q", fs.Arg(0))
	}
	// A home this process cannot name is not a home it may guess: every deletion
	// below would otherwise resolve against the current directory.
	home, code := a.gatewayHome()
	if home == "" {
		return code
	}

	st := &uninstallState{}
	inv := a.uninstallInventory(home)
	if inv.empty() {
		fmt.Fprintf(a.stdout, "\nOpenBox is not installed on this machine; nothing to remove.\n")
		return exitOK
	}
	a.printInventory(inv)
	a.flushSpools(st, inv)
	a.removeHookSurfaces(st, inv)
	a.removeLanes(st, home)
	a.removeArtifacts(st, inv)
	a.removeCredentials(st, inv)
	a.printUninstallReport(st, inv)
	if st.failed {
		return exitError
	}
	return exitOK
}

// uninstallInventory is every path this command owns, resolved once. Resolving
// up front is what lets the operator see the blast radius before a byte is
// lost, and keeps the deletion steps from re-deriving a path that a previous
// step changed.
type uninstallInventory struct {
	home         string
	hookSurfaces []hookSurface
	pluginDir    string
	lanes        []activation.Lane
	posture      []string
	ownedDirs    []string
	spools       []spoolState
	envFile      string
	managed      []string
	// unrecordedLane is a unit file or routed env key with no activation record
	// behind it.
	unrecordedLane bool
}

// laneResidue reports whether anything on this machine still looks like a lane,
// independent of the activation record.
func laneResidue(home string) bool {
	for _, spec := range []laneservice.Spec{
		laneservice.Telemetry("", "", false),
		laneservice.Transport("", "", false),
		laneservice.Gateway("", "", "", false),
	} {
		if p := spec.UnitPath(runtime.GOOS, home); p != "" && fileExists(p) {
			return true
		}
	}
	return len(activation.ResolveElection(gatewayservice.SettingsPath(home)).Routed) > 0
}

// hookFailure separates the two residues, which have opposite consequences: a
// surface we could not edit keeps firing, while one we could not PARSE applies
// no hooks at all.
type hookFailure struct {
	path       string
	unparsable bool
}

type hookSurface struct {
	provider string
	path     string
	present  bool
}

type spoolState struct {
	dir       string
	backlog   int
	discarded int
}

func (inv uninstallInventory) empty() bool {
	for _, s := range inv.hookSurfaces {
		if s.present {
			return false
		}
	}
	return inv.pluginDir == "" && len(inv.lanes) == 0 && !inv.unrecordedLane &&
		len(inv.posture) == 0 && len(inv.ownedDirs) == 0 && inv.envFile == "" && len(inv.spools) == 0
}

func (a *app) uninstallInventory(home string) uninstallInventory {
	inv := uninstallInventory{home: home}

	// All four surfaces unconditionally, rather than branching on a detected
	// provider: a surface skipped because "this machine looks like Codex" is a
	// hook that keeps firing after the command reports success.
	for _, s := range []hookSurface{
		{provider: string(provider.ClaudeCode), path: gatewayservice.SettingsPath(home)},
		{provider: string(provider.ClaudeCode), path: filepath.Join(".claude", "settings.local.json")},
		{provider: string(provider.Codex), path: providers.CodexHooksPath()},
	} {
		if abs, err := filepath.Abs(s.path); err == nil {
			s.path = abs
		}
		s.present = carriesHookRegistration(s.provider, s.path)
		inv.hookSurfaces = append(inv.hookSurfaces, s)
	}
	if dir := providers.ClaudePluginDir(); dirExists(dir) {
		inv.pluginDir = dir
	}
	inv.lanes = activation.ActiveLanes(home)
	// The activation record is not the only evidence a lane exists. A previous
	// run that failed part-way, or an install from an older binary, can leave a
	// unit file or a routed env key with no record at all -- and treating that
	// machine as clean is how "nothing to remove" gets printed at a daemon that
	// is still intercepting model calls.
	if len(inv.lanes) == 0 && laneResidue(home) {
		inv.unrecordedLane = true
	}
	for _, resolve := range []func() (string, error){devconfig.DevConfigPath, devconfig.ApproverConfigPath} {
		if p, err := resolve(); err == nil && fileExists(p) {
			inv.posture = appendUnique(inv.posture, p)
		}
	}
	for _, resolve := range []func() (string, error){devconfig.DevConfigWritePath, devconfig.ApproverConfigWritePath} {
		if p, err := resolve(); err == nil && fileExists(p) {
			inv.posture = appendUnique(inv.posture, p)
		}
	}
	for _, path := range []string{hookflow.DefaultEnforcementPath(), hookflow.DefaultAdvisoryPath()} {
		if path != "" && fileExists(path) {
			inv.posture = appendUnique(inv.posture, path)
		}
	}
	for _, dir := range append(providers.OwnedSpoolDirs(),
		obgit.DefaultSessionDir(), hookflow.PendingApprovalDir(), hookflow.DefaultHaltDir()) {
		if dirExists(dir) {
			inv.ownedDirs = appendUnique(inv.ownedDirs, filepath.Clean(dir))
		}
	}
	for _, dir := range providers.OwnedSpoolDirs() {
		spool := hookflow.Spool{Dir: dir}
		if b, d := spool.BacklogCount(), spool.DiscardedCount(); b > 0 || d > 0 {
			inv.spools = append(inv.spools, spoolState{dir: dir, backlog: b, discarded: d})
		}
	}
	if p, err := devconfig.EnvFilePath(); err == nil && fileExists(p) {
		inv.envFile = p
	}
	// The org's, not ours: removing either silently downgrades a governed
	// machine, and a root-owned file is not writable here anyway.
	for _, p := range []string{devconfig.ManagedConfigPath(), managedSettingsPathForDoctor()} {
		if p != "" && fileExists(p) {
			inv.managed = appendUnique(inv.managed, p)
		}
	}
	return inv
}

func (a *app) printInventory(inv uninstallInventory) {
	fmt.Fprintf(a.stdout, "\nOpenBox uninstall; what is installed on this machine\n")
	for _, s := range inv.hookSurfaces {
		state := "absent"
		if s.present {
			state = "present"
		}
		fmt.Fprintf(a.stdout, "  hooks          %s (%s, %s)\n", s.path, s.provider, state)
	}
	if inv.pluginDir != "" {
		fmt.Fprintf(a.stdout, "  plugin bundle  %s (includes a copy of the openbox binary)\n", inv.pluginDir)
	}
	for _, lane := range inv.lanes {
		fmt.Fprintf(a.stdout, "  lane           %s (unit stopped and removed, env keys restored)\n", lane)
	}
	if inv.unrecordedLane {
		fmt.Fprintf(a.stdout, "  lane           a unit or a routed env key with no activation record behind it;\n")
		fmt.Fprintf(a.stdout, "                 left by an interrupted removal or an older install. Removed anyway.\n")
	}
	for _, p := range inv.posture {
		fmt.Fprintf(a.stdout, "  posture        %s\n", p)
	}
	for _, d := range inv.ownedDirs {
		fmt.Fprintf(a.stdout, "  directory      %s\n", d)
	}
	for _, s := range inv.spools {
		fmt.Fprintf(a.stdout, "  spool          %s (%d undelivered event(s), %d already given up on)\n",
			s.dir, s.backlog, s.discarded)
	}
	if inv.envFile != "" {
		fmt.Fprintf(a.stdout, "  credentials    %s (deleted last, and its signing seed cannot be re-retrieved)\n", inv.envFile)
	}
	for _, p := range inv.managed {
		fmt.Fprintf(a.stdout, "  kept           %s (your organization's, not OpenBox's)\n", p)
	}
}

// flushSpools tries to deliver what is queued before the purge destroys it.
// It runs while .env still exists, because the adapters resolve their own
// credentials and a flush without them delivers nothing.
//
// A missing credential downgrades this to reported loss rather than refusing
// the command: removal must not require the thing being removed to still work.
func (a *app) flushSpools(st *uninstallState, inv uninstallInventory) {
	if len(inv.spools) == 0 {
		return
	}
	for _, s := range inv.spools {
		st.backlog += s.backlog
	}
	if !a.haveCredentials() {
		fmt.Fprintf(a.stdout, "\nflushing SKIPPED: no credentials on this machine, so nothing can be delivered.\n")
		fmt.Fprintf(a.stdout, "  %d undelivered event(s) will be DESTROYED with the spool below. Run `openbox auth`\n", st.backlog)
		fmt.Fprintf(a.stdout, "  and `openbox hook claude-code flush` first if that evidence matters.\n")
		return
	}
	for _, name := range provider.Supported() {
		engine, err := providers.Engine(name)
		if err != nil {
			continue
		}
		// Announced before it runs: an in-process call has no session id, so the
		// engine sweeps every session and then retires, and the two budgets are
		// granted separately.
		fmt.Fprintf(a.stdout, "\nflushing the %s spool (up to ~17s: 12s flush, then a separate 5s retire)…\n", name)
		engine.RunHook("flush", strings.NewReader(""), io.Discard, log.New(a.stderr, "openbox uninstall: ", 0))
	}
	remaining := 0
	for _, dir := range providers.OwnedSpoolDirs() {
		remaining += (hookflow.Spool{Dir: dir}).BacklogCount()
	}
	st.backlog = remaining
	// Reported as "cleared", not "delivered": the same call retires events past
	// their attempt limit or retention age, and a retired event left the spool
	// without reaching the control plane.
	fmt.Fprintf(a.stdout, "  spool went from %d to %d event(s); some of that may be retirement, not delivery\n",
		sumBacklog(inv), remaining)
	if remaining > 0 {
		fmt.Fprintf(a.stdout, "  those %d event(s) could not be delivered and will be DESTROYED with the spool.\n", remaining)
	}
}

func sumBacklog(inv uninstallInventory) int {
	total := 0
	for _, s := range inv.spools {
		total += s.backlog
	}
	return total
}

// haveCredentials is requireCredentials' check without its refusal:
// `uninstall` must never gate on a credential it is about to delete.
func (a *app) haveCredentials() bool {
	envPath, err := devconfig.EnvFilePath()
	if err != nil {
		return false
	}
	kv, err := devconfig.ParseEnvFile(envPath)
	if err != nil {
		return false
	}
	haveKey := a.getenv(devconfig.EnvAPIKeyDirect) != "" || kv[devconfig.EnvAPIKeyDirect] != ""
	havePrivateKey := a.getenv(devconfig.EnvAgentPrivateKey) != "" || kv[devconfig.EnvAgentPrivateKey] != ""
	for _, alias := range []string{"OPENBOX_ED25519_SEED", "OPENBOX_SEED"} {
		if a.getenv(alias) != "" || kv[alias] != "" {
			havePrivateKey = true
		}
	}
	return haveKey && havePrivateKey
}

// removeHookSurfaces takes the gate out first, mirroring an install that
// writes activation last. A failure here is recorded rather than fatal: the
// remaining steps still run, and the final report names the surface that is
// still firing.
func (a *app) removeHookSurfaces(st *uninstallState, inv uninstallInventory) {
	fmt.Fprintf(a.stdout, "\nRemoving hook registrations\n")
	for _, s := range inv.hookSurfaces {
		removed, err := providers.RemoveProviderHooks(s.provider, s.path)
		if err != nil {
			fmt.Fprintf(a.stderr, "warning: could not clean %s: %v\n", s.path, err)
			st.hookFailed = append(st.hookFailed, hookFailure{
				path:       s.path,
				unparsable: strings.Contains(err.Error(), "not valid JSON"),
			})
			st.failed = true
			continue
		}
		for _, r := range removed {
			fmt.Fprintf(a.stdout, "  removed        %s from %s\n", r, s.path)
			st.deleted++
		}
	}
	if inv.pluginDir != "" {
		// After the settings files, so nothing still references the bin/openbox
		// copy inside it. The bundle carries its own hooks.json, which is the one
		// hook path Claude Code does not de-duplicate.
		if err := os.RemoveAll(inv.pluginDir); err != nil {
			fmt.Fprintf(a.stderr, "warning: could not delete %s: %v\n", inv.pluginDir, err)
			st.hookFailed = append(st.hookFailed, hookFailure{path: inv.pluginDir})
			st.failed = true
		} else {
			fmt.Fprintf(a.stdout, "  deleted        %s\n", inv.pluginDir)
			st.deleted++
		}
	}
}

// removeLanes backs all three out and purges their data. Gateway is included
// unconditionally: a machine that once ran `--gateway` still has a unit to
// unload, and leaving it loaded leaves a daemon intercepting model calls.
//
// force stays false. A value that changed after OpenBox set it belongs to
// whoever changed it, and a conflicted lane is reported rather than overwritten
// — it is also still routed and still running, which the report has to say.
func (a *app) removeLanes(st *uninstallState, home string) {
	res := a.runRemovals(home, removalRequest{
		gateway: true, telemetry: true, transport: true,
		purge: true, force: false, uninstall: true,
	})
	if !res.ok() {
		st.failed = true
	}
	// Only a refused deactivate leaves a lane routed AND running. A unit that
	// could not be deleted is residue, not an interception, and saying "still
	// routed" about it would send an operator hunting for traffic that is not
	// flowing.
	st.laneFailed = res.stillRouted
}

func (a *app) removeArtifacts(st *uninstallState, inv uninstallInventory) {
	fmt.Fprintf(a.stdout, "\nRemoving posture and owned directories\n")
	// Posture after hooks: a hook without posture fails open, so posture must not
	// outlive the hooks that read it.
	for _, p := range inv.posture {
		a.deletePath(st, p, os.Remove)
	}
	for _, dir := range inv.ownedDirs {
		if !safeToRemoveAll(dir, inv.home) {
			fmt.Fprintf(a.stdout, "  kept           %s (refusing to delete this recursively; delete it by hand)\n", dir)
			st.kept = appendUnique(st.kept, dir)
			continue
		}
		a.deletePath(st, dir, os.RemoveAll)
	}
}

// safeToRemoveAll guards every recursive delete. Each target already came from
// an owned-path accessor, so this is the second line rather than the first: it
// refuses the shapes where a coordinate went wrong instead of where it named
// somewhere unexpected. OPENBOX_SPOOL_DIR legitimately points anywhere, so a
// basename allowlist would refuse the very directory the operator named.
//
// This repo's stated direction of error for this shape is over-keep, never
// over-delete.
func safeToRemoveAll(dir, home string) bool {
	if !filepath.IsAbs(dir) {
		return false // would resolve against the current directory
	}
	clean := filepath.Clean(dir)
	if clean == filepath.Dir(clean) {
		return false // a filesystem root
	}
	if clean == filepath.Clean(home) {
		return false
	}
	if openboxHome, err := devconfig.Home(); err == nil && clean == filepath.Clean(openboxHome) {
		return false // holds .env, which is deleted last and by name
	}
	return true
}

func (a *app) deletePath(st *uninstallState, path string, remove func(string) error) {
	if err := remove(path); err != nil {
		if os.IsNotExist(err) {
			return
		}
		fmt.Fprintf(a.stderr, "warning: could not delete %s: %v\n", path, err)
		st.failed = true
		return
	}
	fmt.Fprintf(a.stdout, "  deleted        %s\n", path)
	st.deleted++
}

// removeCredentials is last, and unconditional even after a partial failure.
// Keeping .env alone would preserve nothing retryable, because the spool is
// already gone; so there is one rule, reported precisely.
func (a *app) removeCredentials(st *uninstallState, inv uninstallInventory) {
	if inv.envFile == "" {
		return
	}
	fmt.Fprintf(a.stdout, "\nRemoving credentials\n")
	a.deletePath(st, inv.envFile, os.Remove)
	fmt.Fprintf(a.stdout, "  the obx_ key and the Ed25519 signing seed in that file cannot be re-retrieved.\n")
	fmt.Fprintf(a.stdout, "  `openbox auth` registers a NEW agent; it does not recover this one.\n")
	fmt.Fprintf(a.stdout, "  This is an unlink, not a secure erase: the blocks are freed, not overwritten.\n")
}

func (a *app) printUninstallReport(st *uninstallState, inv uninstallInventory) {
	fmt.Fprintf(a.stdout, "\nDone. %d item(s) removed.\n", st.deleted)
	for _, p := range inv.managed {
		fmt.Fprintf(a.stdout, "  kept           %s; your organization's mandate, not OpenBox's state.\n", p)
	}
	// Two residues, opposite severities. Conflating them would tell an operator
	// to ignore the one that breaks every tool call.
	//
	// A project hook file left in another directory is NOT inert: its entries
	// name the engine copy inside the plugin bundle this command just deleted,
	// so the tool reports a failed hook on every call there. No registry of
	// initialized projects exists to find them, but the remedy is free.
	fmt.Fprintf(a.stdout, "  could not reach  a .claude/settings.local.json in any project other than this "+
		"directory.\n")
	fmt.Fprintf(a.stdout, "                   Those entries point at the engine copy deleted above, so the tool "+
		"will report a\n")
	fmt.Fprintf(a.stdout, "                   FAILED HOOK on every tool call in that project. There is no "+
		"registry of\n")
	fmt.Fprintf(a.stdout, "                   initialized projects to find them: `cd` into each one and run "+
		"`openbox uninstall`\n")
	fmt.Fprintf(a.stdout, "                   again. It needs no credentials and deletes nothing twice.\n")
	// Inert, by contrast: the hook script guards on the engine being reachable
	// and exits 0, so a commit in a repo that still carries it proceeds.
	fmt.Fprintf(a.stdout, "  could not reach  a per-repo .git/hooks/prepare-commit-msg installed by "+
		"`openbox hook git install`.\n")
	fmt.Fprintf(a.stdout, "                   That one IS inert: the script skips its body when the engine is "+
		"not reachable\n")
	fmt.Fprintf(a.stdout, "                   and exits 0, so commits keep working. Removing it is optional "+
		"hygiene.\n")

	if len(st.hookFailed) == 0 && !st.laneFailed {
		return
	}
	fmt.Fprintf(a.stdout, "\nCONSEQUENCES; this machine is NOT clean\n")
	for _, f := range st.hookFailed {
		if f.unparsable {
			// Not "still firing": a tool applies no hooks at all from a file it
			// cannot parse, including the developer's own.
			fmt.Fprintf(a.stdout, "  %s could not be parsed, so it was left untouched.\n", f.path)
			fmt.Fprintf(a.stdout, "    While it stays malformed the tool applies NO hooks from it -- neither ours\n")
			fmt.Fprintf(a.stdout, "    nor the developer's own. Fix the JSON, then run `openbox uninstall` again to\n")
			fmt.Fprintf(a.stdout, "    take our entries out.\n")
			continue
		}
		fmt.Fprintf(a.stdout, "  %s still carries an OpenBox hook, and it keeps firing IMMEDIATELY:\n", f.path)
		fmt.Fprintf(a.stdout, "    settings edits are picked up by the tool's own file watcher, so there is no\n")
		fmt.Fprintf(a.stdout, "    restart to wait for. With the credentials now gone it fails open on every tool\n")
		fmt.Fprintf(a.stdout, "    call, logging that it has no identity. Remove the entry by hand.\n")
	}
	if st.laneFailed {
		fmt.Fprintf(a.stdout, "  a lane did not come down: its env key changed after OpenBox set it, so the value\n")
		fmt.Fprintf(a.stdout, "    was left alone. That lane is STILL ROUTED and its daemon is STILL RUNNING, so\n")
		fmt.Fprintf(a.stdout, "    model calls keep going through it. The warning above names the key; resolve it\n")
		fmt.Fprintf(a.stdout, "    and run `openbox uninstall` again.\n")
	}
}

// carriesHookRegistration reports whether a hook file holds anything of ours.
// File presence is not ownership: these documents belong to the developer and
// their org, they survive the removal, and a second uninstall on a clean
// machine has to be able to report nothing to do. Markers come from the
// adapters, so a renamed event cannot silently escape detection.
func carriesHookRegistration(providerName, path string) bool {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	for _, marker := range providers.HookMarkers(providerName) {
		if strings.Contains(string(raw), marker) {
			return true
		}
	}
	return false
}

func dirExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}

func appendUnique(list []string, v string) []string {
	for _, existing := range list {
		if existing == v {
			return list
		}
	}
	return append(list, v)
}
