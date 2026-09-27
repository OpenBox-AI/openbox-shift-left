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
	"github.com/openbox-ai/openbox-shift-left/internal/trace"
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
	// Paths this run tried to delete and could not, for a reason other than
	// their already being gone. Without it the identity-directory sweep finds
	// them still sitting there and reports them as files OpenBox never wrote,
	// which is the wrong diagnosis and sends the operator looking for
	// contamination instead of a delete to retry.
	undeleted []string
	backlog   int // events in the spool that no flush could deliver
	// keepPriorRecord is true when the showThinkingSummaries restore did not
	// run to completion this pass -- the settings file would not parse, so
	// removeHookSurfaces never reached the restore call, or the restore
	// itself errored. removeArtifacts must not purge the prior-value record
	// in that case: it is the only thing a later `uninstall`, once the
	// blocker is fixed, can restore the developer's original value from. A
	// restore that ran and found drift is not this case -- see
	// restoreProviderSettings. keepPriorRecordPath names the settings surface
	// involved, for the report.
	keepPriorRecord     bool
	keepPriorRecordPath string
}

func (a *app) runUninstall(args []string) int {
	fs := a.newFlagSet("uninstall")
	purgeTrace := fs.Bool("purge-trace", false,
		"also delete the local trace directory: every raw request/response body and every "+
			"secret it ever recorded, for every tool. Irreversible; everything else uninstall "+
			"removes leaves the trace directory alone, on purpose, so it survives a re-init")
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}
	if fs.NArg() > 0 {
		return a.errorf("`openbox uninstall` takes no arguments: it removes whatever is installed on this "+
			"machine, for every provider. Got %q", fs.Arg(0))
	}
	if *purgeTrace {
		defer a.purgeTraceDir()
	}
	// A home this process cannot name is not a home it may guess: every deletion
	// below would otherwise resolve against the current directory.
	home, code := a.gatewayHome()
	if home == "" {
		return code
	}
	// And that guard is about $HOME, while every credential path derives from
	// devconfig.Home(), which prefers OPENBOX_HOME. An absolute $HOME with a
	// relative OPENBOX_HOME passed the guard and then resolved nothing, so the
	// inventory came back empty and this command reported "not installed on
	// this machine" at exit 0 with every credential intact. Looking nowhere and
	// calling it clean is the one answer it must never give.
	if _, err := devconfig.Home(); err != nil {
		return a.errorf("%v\n  Refusing to report on a machine this command cannot examine: "+
			"an empty inventory here would be indistinguishable from a clean one.", err)
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
	// Unconditional, and after the credential step rather than inside it: a
	// store can hold a config and no credential file at all -- an interrupted
	// adopt leaves exactly that, because adopt writes the config first on
	// purpose -- and nesting this inside the credential step put it behind that
	// step's "nothing to delete" guard.
	a.removeEmptyIdentityDirs(st)
	a.reportUnrestorableRouting(st, home)
	a.printUninstallReport(st, inv)
	if st.failed {
		return exitError
	}
	return exitOK
}

// purgeTraceDir removes the local trace directory entirely -- the one piece
// of `uninstall`'s reversal an ordinary run deliberately skips (the trace
// survives uninstall, so a developer who reinstalls keeps their own history).
// Best-effort and reported, never fatal: a trace directory this process
// cannot remove is a much smaller problem than the credentials uninstall
// already handled by the time this runs last.
func (a *app) purgeTraceDir() {
	dir := trace.Dir()
	if dir == "" {
		return
	}
	if err := os.RemoveAll(dir); err != nil {
		fmt.Fprintf(a.stderr, "warning: could not remove the trace directory at %s: %v\n", dir, err)
		return
	}
	fmt.Fprintf(a.stdout, "purged the local trace directory at %s\n", dir)
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
	// One credential file per governed tool, plus the org-level one that holds
	// the control token. A single string here was right while a machine had one
	// identity; leaving it would orphan every per-tool key and seed behind a
	// command whose whole promise is that it reverses all of it.
	envFiles []string
	// The residue of a credential write killed between its temp file and its
	// rename. Deleted with the credentials and listed beside them, but kept out
	// of envFiles because the flush gate asks "can anything still deliver?" and
	// no adapter reads one of these -- counting it would make the gate answer
	// yes on a machine whose real credentials are gone.
	envResidue []string
	// tokenCaches is each tool's workload-token.json: a live bearer, good for
	// up to five minutes after the .env it was minted for is long gone, so it
	// is inventoried and removed independently of whether that .env still
	// exists (WriteWorkloadIdentity already deletes it on every re-init; this
	// is the same rule at uninstall).
	tokenCaches []string
	managed     []string
	// unrecordedLane is a unit file or routed env key with no activation record
	// behind it.
	unrecordedLane bool
	// codexOtelPath and codexOtelPresent are the additive config.toml surface:
	// Codex's telemetry pointer lives outside the generic activation record
	// (it has its own parsed-marker ownership model, like hooks.json), so it
	// is swept here rather than through hookSurfaces or activation.ActiveLanes.
	codexOtelPath    string
	codexOtelPresent bool
}

// laneResidue reports whether anything on this machine still looks like a
// lane, independent of the activation record. codexOwnsTelemetry is true
// when providers.HasOwnedCodexOtel already confirmed a healthy Codex-only
// telemetry install: setupCodexTelemetry never calls activation.Activate
// (Codex's [otel] ownership lives in config.toml's own parsed marker instead,
// so a mixed CC+Codex machine's two telemetry installs never collide on one
// activation-record key -- see initlane.go), so the telemetry unit existing
// with zero activation-record lanes is that install's normal, healthy shape
// on a Codex-only machine, not residue from an interrupted removal.
func laneResidue(home string, codexOwnsTelemetry bool) bool {
	specs := []laneservice.Spec{
		laneservice.Transport("", "", false),
		laneservice.Gateway("", "", "", false),
	}
	if !codexOwnsTelemetry {
		specs = append(specs, laneservice.Telemetry("", "", false))
	}
	for _, spec := range specs {
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
	// restoreFailure marks a failure from restoreProviderSettings rather than
	// from removing a hook registration. It needs its own consequence text:
	// by the time that restore runs, this surface's hooks are already gone
	// (RemoveProviderHooks above it succeeded), so "applies NO hooks" would be
	// false here even when path names a file that failed to parse. path can
	// be this surface's own settings file or the internal prior-settings
	// record RestoreProviderSettings reads first -- whichever one actually
	// blocked the restore -- and settingsPath is always the settings surface
	// the restore targeted, for a message that can name both.
	restoreFailure bool
	settingsPath   string
}

type hookSurface struct {
	provider string
	path     string
	present  bool
	// restoreSettings marks the one surface where `openbox init` also forces
	// a bare settings key (Claude Code's showThinkingSummaries): the
	// user-scope file. The project-scope file and Codex's hooks file never
	// had it, so restoring against either would look for a key that was
	// never forced there.
	restoreSettings bool
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
		len(inv.posture) == 0 && len(inv.ownedDirs) == 0 && len(inv.envFiles) == 0 &&
		len(inv.envResidue) == 0 && len(inv.tokenCaches) == 0 && len(inv.spools) == 0 && !inv.codexOtelPresent
}

func (a *app) uninstallInventory(home string) uninstallInventory {
	inv := uninstallInventory{home: home}

	// All four surfaces unconditionally, rather than branching on a detected
	// provider: a surface skipped because "this machine looks like Codex" is a
	// hook that keeps firing after the command reports success.
	for _, s := range []hookSurface{
		{provider: string(provider.ClaudeCode), path: gatewayservice.SettingsPath(home), restoreSettings: true},
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
	// Additive, and swept unconditionally like the hookSurfaces above: an
	// OpenBox-owned [otel] block is its own ownership-aware surface (parsed
	// marker, like hooks.json), not a row in the generic activation record.
	inv.codexOtelPath = providers.CodexConfigTOMLPath()
	inv.codexOtelPresent = providers.HasOwnedCodexOtel(inv.codexOtelPath)
	inv.lanes = activation.ActiveLanes(home)
	// The activation record is not the only evidence a lane exists. A previous
	// run that failed part-way, or an install from an older binary, can leave a
	// unit file or a routed env key with no record at all -- and treating that
	// machine as clean is how "nothing to remove" gets printed at a daemon that
	// is still intercepting model calls.
	if len(inv.lanes) == 0 && laneResidue(home, inv.codexOtelPresent) {
		inv.unrecordedLane = true
	}
	for _, resolve := range []func() (string, error){devconfig.DevConfigPath, devconfig.DevConfigWritePath} {
		if p, err := resolve(); err == nil && fileExists(p) {
			inv.posture = appendUnique(inv.posture, p)
		}
	}
	// And each tool's own config. These are posture, not credentials, so they
	// are swept with the rest of posture -- but they live under ~/.openbox/<tool>/,
	// which is never removed recursively, so they have to be named.
	for _, name := range provider.Supported() {
		if p, err := devconfig.DevConfigPathFor(name); err == nil && fileExists(p) {
			inv.posture = appendUnique(inv.posture, p)
		}
	}
	for _, path := range []string{hookflow.DefaultEnforcementPath(), hookflow.DefaultAdvisoryPath()} {
		if path != "" && fileExists(path) {
			inv.posture = appendUnique(inv.posture, path)
		}
	}
	// Each lane daemon's persisted DeliverPool status (hookflow.StatusPersister):
	// swept with the rest of posture so a stale drop count from a since-removed
	// daemon does not linger and mislead a later `doctor` run into reporting on
	// a lane that no longer exists.
	if openboxHome, err := devconfig.Home(); err == nil {
		for _, lane := range []string{"telemetry", "transport"} {
			if p := hookflow.DeliverStatusPath(openboxHome, lane); fileExists(p) {
				inv.posture = appendUnique(inv.posture, p)
			}
		}
	}
	// The showThinkingSummaries prior-value record: `~/.openbox` is never
	// removed recursively (safeToRemoveAll below refuses it), so this has to
	// be swept by name like the rest of posture, and only after
	// removeHookSurfaces has had a chance to restore from it.
	if p := providers.ClaudePriorSettingsPath(home); fileExists(p) {
		inv.posture = appendUnique(inv.posture, p)
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
	// Explicit accessors, never a bind: this is a loop, and a bind held across
	// it would make every iteration read the first tool's store.
	if p, err := devconfig.OrgEnvFilePath(); err == nil && fileExists(p) {
		inv.envFiles = appendUnique(inv.envFiles, p)
	}
	for _, name := range provider.Supported() {
		if p, err := devconfig.EnvFilePathFor(name); err == nil && fileExists(p) {
			inv.envFiles = appendUnique(inv.envFiles, p)
		}
	}
	// The interrupted-write residue of every store, the org's included. The org
	// one matters most and is the one a per-tool loop would miss: its directory
	// is Home() itself, which this command never removes, so nothing sweeps it
	// as a side effect -- and the credential it holds can create and rotate
	// agents across the whole organization.
	for _, tool := range append([]string{""}, provider.Supported()...) {
		for _, p := range interruptedCredentialWrites(tool) {
			inv.envResidue = appendUnique(inv.envResidue, p)
		}
	}
	// Each tool's cached workload bearer, independent of whether its .env
	// still exists: an interrupted removal, or a hand-deleted .env, can leave
	// a live token in the store directory with nothing else there to trigger
	// the credential step at all.
	for _, name := range provider.Supported() {
		if p, err := devconfig.WorkloadTokenCachePathFor(name); err == nil && fileExists(p) {
			inv.tokenCaches = appendUnique(inv.tokenCaches, p)
		}
	}
	// The org's, not ours: removing either silently downgrades a governed
	// machine, and a root-owned file is not writable here anyway.
	for _, p := range []string{devconfig.ManagedConfigPath(), claudeManagedSettingsPath()} {
		if p != "" && fileExists(p) {
			inv.managed = appendUnique(inv.managed, p)
		}
	}
	return inv
}

// residueTmpPrefixes are the literal os.CreateTemp prefixes every atomic
// writer that can leave residue in an identity directory is handed, each
// paired with the same ".tmp" suffix:
//   - ".env-": devconfig's WriteEnvFile (envfile.go), the API key and the
//     workload signing key.
//   - ".workload-token-": workloadauth.FileCache.Store (cache.go), the cached
//     bearer token.
//   - ".openbox-": internal/cli/atomicfile's Windows Write
//     (write_windows.go), used wherever this repo writes a file atomically on
//     that OS.
//
// Nothing else writes any of these three shapes into a tool's identity
// directory.
var residueTmpPrefixes = []string{".env-", ".workload-token-", ".openbox-"}

// interruptedCredentialWrites finds the residue of a credential or cache write
// that was killed between its temp file and its rename.
//
// Each of residueTmpPrefixes' writers is atomic the same way: the whole body
// goes into a 0600 temp file beside the target, then a rename, and the
// deferred cleanup that would remove the temp file does not run if the
// process dies first. So a lid closing, an OOM or a Ctrl-C at the wrong
// microsecond leaves a readable copy of a secret -- an API key, a signing key,
// or a live bearer token -- in the store directory, under a name nothing else
// looks for. `uninstall` printing that those values "cannot be re-retrieved"
// while one of them is still on disk is the one failure this command cannot
// have.
//
// The pattern is matched, not guessed: each prefix is the literal string its
// writer hands os.CreateTemp. An empty tool names the org-level directory,
// whose residue holds the organization control token.
//
// Read and filter rather than filepath.Glob, because a directory path is not a
// pattern and matching it as one fails silently. A home holding `[`, `*` or `?`
// -- which OPENBOX_HOME accepts and $HOME can legally contain -- makes Glob
// return zero matches and no error, so this would have reported success having
// looked nowhere. What it would lose is a readable signing seed.
func interruptedCredentialWrites(tool string) []string {
	dir, err := devconfig.IdentityDirFor(tool)
	if err != nil {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var found []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".tmp") {
			continue
		}
		owned := false
		for _, prefix := range residueTmpPrefixes {
			if strings.HasPrefix(name, prefix) {
				owned = true
				break
			}
		}
		if !owned {
			continue
		}
		found = append(found, filepath.Join(dir, name))
	}
	return found
}

func (a *app) printInventory(inv uninstallInventory) {
	fmt.Fprintf(a.stdout, "\nOpenBox uninstall; what is installed on this machine\n")
	for _, s := range inv.hookSurfaces {
		state := "absent"
		if s.present {
			state = "present"
		}
		a.row("hooks", "%s (%s, %s)", s.path, s.provider, state)
	}
	if inv.codexOtelPresent {
		a.row("telemetry", "%s (codex, present; an OpenBox-owned [otel] block)", inv.codexOtelPath)
	}
	if inv.pluginDir != "" {
		a.row("plugin", "%s (a bundle including a copy of the openbox binary)", inv.pluginDir)
	}
	for _, lane := range inv.lanes {
		a.row("lane", "%s (unit stopped and removed, env keys restored)", lane)
	}
	if inv.unrecordedLane {
		a.row("lane", "a unit or a routed env key with no activation record behind it,")
		a.row("", "left by an interrupted removal or an older install. The unit can be")
		a.row("", "removed; a routed key cannot, because the record of what was there")
		a.row("", "before it is gone.")
	}
	for _, p := range inv.posture {
		a.row("posture", "%s", p)
	}
	for _, d := range inv.ownedDirs {
		a.row("directory", "%s", d)
	}
	for _, s := range inv.spools {
		a.row("spool", "%s (%d undelivered event(s), %d already given up on)",
			s.dir, s.backlog, s.discarded)
	}
	for _, p := range inv.envFiles {
		a.row("credentials", "%s (deleted last, and its workload private key cannot be re-retrieved)", p)
	}
	for _, p := range inv.envResidue {
		a.row("credentials", "%s (an interrupted write; same secrets, deleted with them)", p)
	}
	for _, p := range inv.tokenCaches {
		a.row("credentials", "%s (a cached bearer token, live for up to 5 minutes)", p)
	}
	for _, p := range inv.managed {
		a.row("kept", "%s (your organization's, not OpenBox's)", p)
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
	queued := sumBacklog(inv)
	st.backlog += queued
	if !a.haveCredentials(inv) {
		fmt.Fprintf(a.stdout, "\nflushing SKIPPED: no credentials on this machine, so nothing can be delivered.\n")
		a.note(fmt.Sprintf("%d undelivered event(s) will be DESTROYED with the spool below. Run", st.backlog),
			"`openbox init --provider <tool>` and `openbox hook <tool> flush` first if that",
			"evidence matters.")
		traceUninstallStep("spool-flush", nil, map[string]any{"skipped": true, "queued": queued})
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
		fmt.Fprintf(a.stdout, "\nflushing the %s spool (up to ~65s: 60s flush, then a separate 5s retire)…\n", name)
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
	a.note(fmt.Sprintf("spool went from %d to %d event(s); some of that may be retirement, not delivery",
		queued, remaining))
	if remaining > 0 {
		a.note(fmt.Sprintf("those %d event(s) could not be delivered and will be DESTROYED with the spool.", remaining))
	}
	traceUninstallStep("spool-flush", nil, map[string]any{"queued": queued, "remaining": remaining})
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
//
// Any store, not the bound one: the flush delivers each tool's spool with that
// tool's own identity, so one usable store is enough to make flushing worth
// attempting. Asking only about a single store would print "flushing SKIPPED"
// and destroy a backlog that could have been delivered.
func (a *app) haveCredentials(inv uninstallInventory) bool {
	// The environment first, and unconditionally: an exported identity is
	// complete on its own and is the documented CI route, so a machine with no
	// credential file anywhere still has something to flush with. Asking only
	// about inventoried files meant a machine with none never asked at all, and
	// this command destroyed a deliverable backlog while printing that it had
	// no credentials to deliver it with.
	if a.credentialsPresent(nil) {
		return true
	}
	for _, path := range inv.envFiles {
		kv, err := devconfig.ParseEnvFile(path)
		if err != nil {
			continue
		}
		if a.credentialsPresent(kv) {
			return true
		}
	}
	return false
}

// removeHookSurfaces takes the gate out first, mirroring an install that
// writes activation last. A failure here is recorded rather than fatal: the
// remaining steps still run, and the final report names the surface that is
// still firing.
func (a *app) removeHookSurfaces(st *uninstallState, inv uninstallInventory) {
	fmt.Fprintf(a.stdout, "\nRemoving hook registrations\n")
	for _, s := range inv.hookSurfaces {
		removed, err := providers.RemoveProviderHooks(s.provider, s.path)
		traceUninstallStep("hook-remove", err, map[string]any{"provider": s.provider, "path": s.path})
		if err != nil {
			fmt.Fprintf(a.stderr, "warning: could not clean %s: %v\n", s.path, err)
			st.hookFailed = append(st.hookFailed, hookFailure{
				path:       s.path,
				unparsable: strings.Contains(err.Error(), "not valid JSON"),
			})
			st.failed = true
			// This surface never reaches the restore call below: the
			// prior-value record it would have restored from must survive
			// this run, or the developer's original value is unrecoverable
			// once this file is deleted unconditionally further down.
			if s.restoreSettings {
				st.keepPriorRecord = true
				st.keepPriorRecordPath = s.path
			}
			continue
		}
		for _, r := range removed {
			a.row("removed", "%s from %s", r, s.path)
			st.deleted++
		}
		if s.restoreSettings {
			a.restoreProviderSettings(st, inv.home, s)
		}
	}
	if inv.codexOtelPresent {
		removed, err := providers.RemoveCodexOtel(inv.codexOtelPath)
		traceUninstallStep("hook-remove", err, map[string]any{"provider": string(provider.Codex), "path": inv.codexOtelPath, "kind": "otel"})
		if err != nil {
			fmt.Fprintf(a.stderr, "warning: could not clean %s: %v\n", inv.codexOtelPath, err)
			st.hookFailed = append(st.hookFailed, hookFailure{path: inv.codexOtelPath})
			st.failed = true
		} else if removed {
			a.row("removed", "the OpenBox [otel] block from %s", inv.codexOtelPath)
			st.deleted++
		}
	}
	if inv.pluginDir != "" {
		// After the settings files, so nothing still references the bin/openbox
		// copy inside it. Older installs also left a plugin manifest and a second
		// copy of the hook config in there, which no longer ship: a plugin's
		// handlers do not de-duplicate against the settings-level ones.
		if err := os.RemoveAll(inv.pluginDir); err != nil {
			fmt.Fprintf(a.stderr, "warning: could not delete %s: %v\n", inv.pluginDir, err)
			st.hookFailed = append(st.hookFailed, hookFailure{path: inv.pluginDir})
			st.failed = true
		} else {
			a.row("deleted", "%s", inv.pluginDir)
			st.deleted++
		}
	}
}

// restoreProviderSettings puts a bare settings key (Claude Code's
// showThinkingSummaries) back to whatever it held before `openbox init`
// forced it. A restore failure is recorded the same way a hook-removal
// failure is, immediately above: reported, not fatal, so the remaining
// uninstall steps still run. Drift -- the developer changed the value after
// `init` -- is reported too, but is not a failure: it is not this command's
// place to overwrite a value someone deliberately changed.
func (a *app) restoreProviderSettings(st *uninstallState, home string, s hookSurface) {
	res, err := providers.RestoreProviderSettings(s.provider, s.path, home)
	defer func() {
		traceUninstallStep("settings-restore", err, map[string]any{"provider": s.provider, "path": s.path})
	}()
	if err != nil {
		// The failure can be s.path itself, or the internal prior-settings
		// record RestoreProviderSettings reads before ever touching s.path --
		// a file the developer never opens. Blaming s.path unconditionally
		// would send them to inspect a file that is perfectly fine while the
		// actual blocker, a different file, goes unnamed. Both adapter errors
		// name the file they came from in their own text, so pick whichever
		// one the message actually mentions rather than assuming s.path.
		failedPath := s.path
		if recPath := providers.ClaudePriorSettingsPath(home); recPath != "" && strings.Contains(err.Error(), recPath) {
			failedPath = recPath
		}
		fmt.Fprintf(a.stderr, "warning: %s: could not restore %s: %v\n", failedPath, providers.ClaudeThinkingSummariesKey, err)
		st.hookFailed = append(st.hookFailed, hookFailure{
			path:           failedPath,
			unparsable:     strings.Contains(err.Error(), "not valid JSON"),
			restoreFailure: true,
			settingsPath:   s.path,
		})
		st.failed = true
		// The restore did not complete -- e.g. the prior-value record itself
		// is corrupt -- so it must survive this run for a later `uninstall`
		// to finish the job. Unlike drift below, nothing here is finished.
		st.keepPriorRecord = true
		st.keepPriorRecordPath = s.path
		return
	}
	switch {
	case !res.Recorded:
		return // the ordinary case: never installed here, or already uninstalled
	case res.Drifted:
		a.row("left alone", "%s in %s (now %s; changed since `init` set it to true)",
			providers.ClaudeThinkingSummariesKey, s.path, res.Current)
	case res.Present:
		a.row("restored", "%s in %s to %s", providers.ClaudeThinkingSummariesKey, s.path, res.Value)
	default:
		a.row("removed", "%s from %s", providers.ClaudeThinkingSummariesKey, s.path)
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
	//
	// One exception: the showThinkingSummaries prior-value record. When
	// removeHookSurfaces set keepPriorRecord, the restore above did not run
	// to completion, and purging this file anyway would strand the
	// developer's original value forever -- a later `uninstall`, once the
	// blocker named above is fixed, is the only way it is ever recovered.
	// Drift (the developer changed the value after `init`) does NOT set this
	// flag: there the restore ran, found the current value authoritative,
	// and is genuinely finished, so the record purges as normal.
	recPath := providers.ClaudePriorSettingsPath(inv.home)
	for _, p := range inv.posture {
		if st.keepPriorRecord && p == recPath {
			a.row("kept", "%s", p)
			a.row("", "the showThinkingSummaries restore against %s did not complete;", st.keepPriorRecordPath)
			a.row("", "see the warning above for what to fix. Fix it, then run `openbox")
			a.row("", "uninstall` again to finish the restore and remove this record.")
			st.kept = appendUnique(st.kept, p)
			continue
		}
		a.deletePath(st, p, os.Remove)
	}
	for _, dir := range inv.ownedDirs {
		if !safeToRemoveAll(dir, inv.home) {
			a.row("kept", "%s (refusing to delete this recursively; delete it by hand)", dir)
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
		st.undeleted = appendUnique(st.undeleted, path)
		traceUninstallStep("artifact-remove", err, map[string]any{"path": path})
		return
	}
	a.row("deleted", "%s", path)
	st.deleted++
	traceUninstallStep("artifact-remove", nil, map[string]any{"path": path})
}

// removeCredentials is last, and unconditional even after a partial failure.
// Keeping .env alone would preserve nothing retryable, because the spool is
// already gone; so there is one rule, reported precisely.
func (a *app) removeCredentials(st *uninstallState, inv uninstallInventory) {
	if len(inv.envFiles) == 0 && len(inv.envResidue) == 0 && len(inv.tokenCaches) == 0 {
		return
	}
	fmt.Fprintf(a.stdout, "\nRemoving credentials\n")
	for _, path := range append(append(append([]string{}, inv.envFiles...), inv.envResidue...), inv.tokenCaches...) {
		a.deletePath(st, path, os.Remove)
	}
	// The directories follow, from runUninstall: by name first and the
	// directory after, because a directory delete racing the credential delete
	// is how a store survives an uninstall that reported success.
	a.note("The obx_ keys and workload private keys in those files cannot be re-retrieved; the backend never held the private key.",
		"`openbox init --provider <tool>` registers a NEW agent; it does not recover",
		"these. The organization control token went with them; `openbox auth` takes a",
		"new one. This is an unlink, not a secure erase: the blocks are freed, not",
		"overwritten.")
}

// removeEmptyIdentityDirs takes each ~/.openbox/<tool>/ away once nothing is
// left in it. Only when empty, and never recursively: the direction of error
// in this command is over-keep, and a directory still holding something is
// something this run did not account for.
//
// Which is exactly why a kept one is named. Every other residue this command
// cannot clear says so -- the org's managed config, a directory it refuses to
// recurse into, a lane it could not unroute -- and a store directory surviving
// silently after "Done" reads as a finished uninstall to the only person who
// could deal with it.
func (a *app) removeEmptyIdentityDirs(st *uninstallState) {
	for _, name := range provider.Supported() {
		dir, err := devconfig.IdentityDirFor(name)
		if err != nil || !dirExists(dir) {
			continue
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			a.row("kept", "%s (could not read it: %v)", dir, err)
			st.kept = appendUnique(st.kept, dir)
			continue
		}
		if len(entries) > 0 {
			a.row("kept", "%s (%s)", dir, keptDirReason(st, dir, entries))
			st.kept = appendUnique(st.kept, dir)
			continue
		}
		a.deletePath(st, dir, os.Remove)
	}
}

// keptDirReason says why a store directory survived, and the distinction
// matters: "files OpenBox did not put there" tells the operator to look for
// contamination, which is the wrong thing to do when what actually happened is
// that this run tried to delete its own file and could not. That failure is
// already a warning on stderr; saying something different about it on stdout
// would leave two accounts of one cause, one of them false.
func keptDirReason(st *uninstallState, dir string, entries []os.DirEntry) string {
	failed := 0
	for _, e := range entries {
		for _, p := range st.undeleted {
			if p == filepath.Join(dir, e.Name()) {
				failed++
				break
			}
		}
	}
	switch {
	case failed == len(entries):
		return fmt.Sprintf("%d file(s) could not be deleted; see the warnings above", failed)
	case failed > 0:
		return fmt.Sprintf("%d of %d file(s) could not be deleted; see the warnings above, then delete the rest by hand",
			failed, len(entries))
	default:
		return fmt.Sprintf("%d file(s) OpenBox did not put there; delete it by hand", len(entries))
	}
}

// reportUnrestorableRouting is the one residue this command cannot clear.
//
// A lane env key with no activation record behind it has no "before" value to
// put back: Deactivate finds no entry, returns nil, and the key survives. The
// direction of error is right -- a value we have no record of writing is not
// ours to delete or guess at -- but reporting success over it would tell the
// operator their model calls are no longer being routed when they are, at a
// daemon that is gone.
func (a *app) reportUnrestorableRouting(st *uninstallState, home string) {
	routed := activation.ResolveElection(gatewayservice.SettingsPath(home)).Routed
	if len(routed) == 0 {
		return
	}
	settings := gatewayservice.SettingsPath(home)
	fmt.Fprintf(a.stdout, "\nLEFT IN PLACE; and this machine is NOT clean\n")
	a.note(fmt.Sprintf("%s still routes model calls through %v.", settings, routed),
		"There is no activation record for it, so nothing here knows what those keys",
		"held before OpenBox set them -- and a proxy or base-URL value belongs to",
		"whoever put it there. Deleting it blind could take a corporate proxy down",
		"with it. The daemon those keys point at is gone, so model calls will fail",
		"until you edit that file by hand.")
	st.failed = true
}

func (a *app) printUninstallReport(st *uninstallState, inv uninstallInventory) {
	fmt.Fprintf(a.stdout, "\nDone. %d item(s) removed.\n", st.deleted)
	for _, p := range inv.managed {
		a.row("kept", "%s; your organization's mandate, not OpenBox's state.", p)
	}
	// Everything the steps above decided to keep, repeated here. Each was named
	// as it happened, but this block is what an operator reads to decide the
	// machine is clean -- and a store directory reaching it unmentioned is the
	// same silence the residue sweep exists to end, one level up.
	for _, p := range st.kept {
		a.row("kept", "%s; see the reason above.", p)
	}
	// Two residues, opposite severities. Conflating them would tell an operator
	// to ignore the one that breaks every tool call.
	//
	// A project hook file left in another directory is NOT inert: its entries
	// name the engine copy inside the plugin bundle this command just deleted,
	// so the tool reports a failed hook on every call there. No registry of
	// initialized projects exists to find them, but the remedy is free.
	a.row("not reached", "a .claude/settings.local.json in any project other than this one.")
	a.row("", "Those entries point at the engine copy deleted above, so the tool will")
	a.row("", "report a FAILED HOOK on every tool call in that project, and there is no")
	a.row("", "registry of initialized projects to find them: `cd` into each one and run")
	a.row("", "`openbox uninstall` again. It needs no credentials, and deletes nothing")
	a.row("", "twice.")
	// Inert, by contrast: the hook script guards on the engine being reachable
	// and exits 0, so a commit in a repo that still carries it proceeds.
	a.row("not reached", "a per-repo .git/hooks/prepare-commit-msg installed by `openbox hook")
	a.row("", "git install`. That one IS inert: the script skips its body when the")
	a.row("", "engine is not reachable and exits 0, so commits keep working. Removing")
	a.row("", "it is optional hygiene.")

	if len(st.hookFailed) == 0 && !st.laneFailed {
		return
	}
	fmt.Fprintf(a.stdout, "\nCONSEQUENCES; this machine is NOT clean\n")
	for _, f := range st.hookFailed {
		if f.restoreFailure {
			// Not the unparsable-hooks case below: this surface's hooks were
			// already removed (RemoveProviderHooks succeeded) before the
			// restore ran, so "applies NO hooks" would be false here even
			// when f.path is itself unparsable. f.path names whichever file
			// actually blocked the restore -- f.settingsPath, or the internal
			// prior-settings record.
			fmt.Fprintf(a.stdout, "  %s could not be read, so %s could not be restored in %s.\n",
				f.path, providers.ClaudeThinkingSummariesKey, f.settingsPath)
			fmt.Fprintf(a.stdout, "    %s is unchanged there. Fix or remove %s, then run `openbox uninstall`\n",
				providers.ClaudeThinkingSummariesKey, f.path)
			fmt.Fprintf(a.stdout, "    again to finish the restore.\n")
			continue
		}
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
