package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/providers"
	"github.com/openbox-ai/openbox-shift-left/internal/trace"
)

// This file is doctor's Muse Code section. It reports what can be read without
// running a model: the binary, the hooks file under Muse's own rules, whether
// Muse actually loads the handlers, whether they ran, and what the org policy
// says. Every `muse` subprocess goes through providers.MuseRunner, so a test
// scripts each answer and never reaches a real binary.
//
// Muse 1.4.1 prints the load probe's `Hooks: N runnable · M warnings` line only
// when there is a warning, and `muse config status` lists policy sources but
// not what a policy requires. The local-tracing log is still documented by a
// third party only; the parsers fail soft, to "unverified" or a warning, never
// to a false "ok".

// museProbeTimeout bounds the echo-provider load probe: a full muse start-up
// plus every handler's process.
var museProbeTimeout = 60 * time.Second

// museConfigTimeout bounds `muse config status`.
var museConfigTimeout = 10 * time.Second

// museLocalTracingDir is where Muse logs each hook run, as a third party
// documented it. A seam so a test points it at a fixture.
var museLocalTracingDir = func() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".local", "share", "muse", "local-tracing", "bootstrap")
}

// museHookLogBytes caps how much of one local-tracing log is read.
const museHookLogBytes = 1 << 20

// reportMuse prints the Muse section, or nothing on a machine where Muse was
// never set up: a Claude-Code-only machine's output stays as it was.
func (a *app) reportMuse() {
	if !a.museInPlay() {
		return
	}
	fmt.Fprintf(a.stdout, "\nMuse Code (hooks)\n")
	ver := providers.CheckMuseVersion()
	a.reportMuseBinary(ver)
	audit, auditErr := a.reportMuseSettings()
	a.reportMuseLoadProbe(ver, audit, auditErr)
	a.reportMuseHookRuns(ver)
	a.reportMusePolicy(ver)
	a.reportMuseModelCalls()
	a.reportMuseSpool()
	a.reportMuseEvidenceGaps()
}

// museInPlay reports whether this machine has been set up for Muse: an
// identity for it, or a hooks file carrying an OpenBox registration. Muse
// merely being installed is not enough to add a section.
func (a *app) museInPlay() bool {
	if cfgPath, err := devconfig.DevConfigPathFor("muse"); err == nil {
		if cfg, _ := devconfig.Load(cfgPath); cfg.AgentID != "" {
			return true
		}
	}
	return carriesHookRegistration("muse", providers.MuseSettingsPath())
}

func (a *app) museFinding(name, status, label, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	a.wrapRow(label, "%s", msg)
	traceDoctorFinding("muse:"+name, status, msg)
}

// 1. binary and version against the supported range.
func (a *app) reportMuseBinary(v providers.MuseVersionCheck) {
	rng := providers.MuseVersionRange()
	switch v.State {
	case providers.MuseVersionSupported:
		a.museFinding("binary", "ok", "binary", "ok: muse %s (supported: %s)", v.Version, rng)
	case providers.MuseVersionNotOnPath:
		a.museFinding("binary", "warning", "binary", "WARNING: muse is not on PATH, so its version and the hook load cannot be checked. The hooks are installed and take effect once muse runs (supported: %s)", rng)
	case providers.MuseVersionTooOld:
		a.museFinding("binary", "fail", "binary", "FAIL: %s. A failed or malformed gate answers as an allow on this release; upgrade muse to the supported range (%s)", v.Detail, rng)
	case providers.MuseVersionUntested:
		a.museFinding("binary", "warning", "binary", "WARNING: %s. The hooks may still work; nothing here has been tested on it (supported: %s)", v.Detail, rng)
	default:
		a.museFinding("binary", "fail", "binary", "FAIL: muse's version could not be read (%s), so it cannot be shown to be in the supported range (%s). Older releases let a failed gate answer as an allow", v.Detail, rng)
	}
}

// 2. the hooks file under Muse's rules, and the owned-handler count.
func (a *app) reportMuseSettings() (providers.MuseSettingsAudit, error) {
	path := providers.MuseSettingsPath()
	audit, err := providers.AuditMuseSettings(path)
	a.row("settings", "%s", withPresence(path))
	switch {
	case err != nil:
		a.museFinding("settings", "fail", "", "FAIL: Muse cannot read this file (%v). Muse drops EVERY hook in a file it cannot read, with only a startup warning, so nothing is governed. Fix or remove it, then run `openbox init --provider muse`", err)
		return audit, err
	case !audit.Present:
		a.museFinding("settings", "fail", "", "FAIL: no settings file, so no hook is registered. Run `openbox init --provider muse`")
		return audit, nil
	}
	problems := audit.Problems()
	engines := map[string]bool{}
	for _, o := range audit.Owned {
		engines[o.Engine] = true
	}
	var missingEngines []string
	for engine := range engines {
		if _, statErr := os.Stat(engine); statErr != nil {
			missingEngines = append(missingEngines, engine)
		}
	}
	sort.Strings(missingEngines)
	switch {
	case len(missingEngines) > 0:
		a.museFinding("handlers", "fail", "", "FAIL: %d of %d OpenBox handlers registered, but the engine they run does not exist: %s. Every handler fails, and so does its successor. Run `openbox init --provider muse`", len(audit.Owned), audit.Expected, strings.Join(missingEngines, ", "))
	case len(audit.Owned) == 0:
		a.museFinding("handlers", "fail", "", "FAIL: 0 of %d OpenBox handlers registered. Run `openbox init --provider muse`", audit.Expected)
	case len(audit.GatedFailures()) > 0:
		a.museFinding("handlers", "fail", "", "FAIL: %d of %d OpenBox handlers registered, but a gated one can fail open or be skipped: %s. Run `openbox init --provider muse`", len(audit.Owned), audit.Expected, strings.Join(audit.GatedFailures(), "; "))
	case len(problems) > 0:
		a.museFinding("handlers", "warning", "", "WARNING: %d of %d OpenBox handlers registered; %s. Run `openbox init --provider muse`", len(audit.Owned), audit.Expected, strings.Join(problems, "; "))
	default:
		a.museFinding("handlers", "ok", "", "ok: %d of %d OpenBox handlers registered, every gated one with a deny-only successor", len(audit.Owned), audit.Expected)
	}
	for _, w := range audit.Warnings {
		a.museFinding("settings-warning", "warning", "", "note: %s", w)
	}
	return audit, nil
}

// museLoadCounts is what the echo probe's summary line said.
type museLoadCounts struct {
	Runnable, Warnings int
	// OpenBoxWarnings counts warning lines that name an OpenBox handler.
	OpenBoxWarnings []string
}

var museHooksSummary = regexp.MustCompile(`Hooks:\s*(\d+)\s+runnable\D{1,8}(\d+)\s+warning`)

// parseMuseLoadProbe reads `Hooks: N runnable · M warnings` from a probe's
// output, and the warning lines that name OpenBox. Muse 1.4.1 prints that line
// ONLY when at least one hook warning exists; a clean load prints nothing, so
// ok=false means "no warnings", never "unknown", and it is up to the caller to
// confirm the handlers ran.
func parseMuseLoadProbe(out string) (museLoadCounts, bool) {
	var c museLoadCounts
	found := false
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		if m := museHooksSummary.FindStringSubmatch(line); m != nil && !found {
			c.Runnable, _ = strconv.Atoi(m[1])
			c.Warnings, _ = strconv.Atoi(m[2])
			found = true
			continue
		}
		low := strings.ToLower(line)
		if (strings.Contains(low, "warn") || strings.Contains(low, "hook")) &&
			(strings.Contains(low, "openbox") || strings.Contains(low, "hook muse")) {
			c.OpenBoxWarnings = append(c.OpenBoxWarnings, strings.TrimSpace(line))
		}
	}
	return c, found
}

// museHookRanSince reports whether a muse hook invocation was recorded in the
// local trace (a `hook.in` record, written by every `openbox hook muse` run
// before it reads anything else) at or after since. It is how doctor knows the
// handlers really ran during the probe, which a silent clean load cannot show.
func museHookRanSince(since time.Time) bool {
	recs, _, err := trace.ReadFiltered(trace.Dir(),
		func(line []byte) bool {
			return bytes.Contains(line, []byte(`"provider":"muse"`)) && bytes.Contains(line, []byte(`"`+trace.StageHookIn+`"`))
		},
		func(r trace.Record) bool {
			return r.Provider == "muse" && r.Stage == trace.StageHookIn && !r.TS.Before(since)
		})
	return err == nil && len(recs) > 0
}

// 3. does Muse load the handlers, and do they run. A Hooks summary line (only
// printed when a warning exists) is judged as before: a warning naming OpenBox
// fails, fewer runnable than registered fails. No summary line with exit 0 is a
// load with 0 warnings, and is then only believed once a muse hook invocation
// shows up in the trace after the probe began.
//
// The probe is an echo-provider session: it makes no model call, but it is a
// real governed session, so its hooks run and its events reach the control
// plane like any other session. It runs in a throwaway directory, and nothing
// in it is keyed on tool-controlled input, so it cannot become a bypass.
func (a *app) reportMuseLoadProbe(v providers.MuseVersionCheck, audit providers.MuseSettingsAudit, auditErr error) {
	const label = "load probe"
	switch {
	case v.State == providers.MuseVersionNotOnPath:
		a.museFinding("probe", "unverified", label, "not run: muse is not on PATH")
		return
	case v.State == providers.MuseVersionTooOld, v.State == providers.MuseVersionUnreadable:
		a.museFinding("probe", "unverified", label, "not run: muse is not shown to be in the supported range")
		return
	case auditErr != nil || len(audit.Owned) == 0:
		a.museFinding("probe", "unverified", label, "not run: there are no readable OpenBox handlers to load")
		return
	}
	dir, err := os.MkdirTemp("", "openbox-doctor-muse-")
	if err != nil {
		a.museFinding("probe", "warning", label, "WARNING: could not make a scratch directory for the probe: %v", err)
		return
	}
	defer os.RemoveAll(dir)
	ctx, cancel := context.WithTimeout(context.Background(), museProbeTimeout)
	defer cancel()
	began := time.Now()
	res, runErr := providers.MuseRunner(ctx, dir, "exec", "--provider", "echo", "--trust-workspace", "openbox doctor hook probe")
	if runErr != nil {
		a.museFinding("probe", "warning", label, "WARNING: the echo probe did not finish: %v", runErr)
		return
	}
	counts, ok := parseMuseLoadProbe(string(res.Stderr) + "\n" + string(res.Stdout))
	switch {
	case res.ExitCode != 0 && !ok:
		a.museFinding("probe", "warning", label, "WARNING: the echo probe exited %d without a `Hooks: N runnable` summary, so the load could not be confirmed", res.ExitCode)
	case ok && len(counts.OpenBoxWarnings) > 0:
		a.museFinding("probe", "fail", label, "FAIL: Muse loaded %d hooks and warned about OpenBox's: %s", counts.Runnable, counts.OpenBoxWarnings[0])
	case ok && counts.Runnable < audit.Expected:
		a.museFinding("probe", "fail", label, "FAIL: Muse found %d runnable hooks, fewer than the %d OpenBox registered; some are not loading", counts.Runnable, audit.Expected)
	case ok && counts.Warnings > 0:
		a.museFinding("probe", "warning", label, "WARNING: Muse found %d runnable hooks (%d expected) and %d warning(s), none naming OpenBox", counts.Runnable, audit.Expected, counts.Warnings)
	case !museHookRanSince(began):
		a.museFinding("probe", "warning", label, "WARNING: hooks did not run during the probe: Muse reported no hook warning, but no OpenBox hook invocation was recorded in the local trace after it began")
	default:
		a.museFinding("probe", "ok", label, "ok: Muse loaded the hooks with 0 warnings and OpenBox's handlers ran during the probe. The probe is a model-free echo session and shows up as one session in the control plane")
	}
}

// museHookRunCounts is what the local-tracing log says about hook runs.
type museHookRunCounts struct{ Terminal, Completed int }

// parseMuseHookRuns counts `hook.execution.terminal` records and how many of
// them ended `completed`.
func parseMuseHookRuns(log string) museHookRunCounts {
	var c museHookRunCounts
	sc := bufio.NewScanner(strings.NewReader(log))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.Contains(line, "hook.execution.terminal") {
			continue
		}
		c.Terminal++
		if strings.Contains(line, `status="completed"`) || strings.Contains(line, `"status":"completed"`) {
			c.Completed++
		}
	}
	return c
}

// 4. did hooks actually run, from Muse's own local-tracing log. The path comes
// from a third party for one release line, so anything else is "unverified",
// and this row never fails.
func (a *app) reportMuseHookRuns(v providers.MuseVersionCheck) {
	const label = "hook runs"
	dir := museLocalTracingDir()
	if v.State != providers.MuseVersionSupported {
		a.museFinding("hook-runs", "unverified", label, "unverified: Muse's local-tracing log is documented for the tested versions only")
		return
	}
	logs, _ := filepath.Glob(filepath.Join(dir, "*.log"))
	if dir == "" || len(logs) == 0 {
		a.museFinding("hook-runs", "unverified", label, "unverified: no local-tracing log at %s; a missing log is not a failure", orUnset(dir))
		return
	}
	newest, newestTime := "", time.Time{}
	for _, l := range logs {
		if info, err := os.Stat(l); err == nil && info.ModTime().After(newestTime) {
			newest, newestTime = l, info.ModTime()
		}
	}
	raw, err := readTail(newest, museHookLogBytes)
	if err != nil {
		a.museFinding("hook-runs", "unverified", label, "unverified: could not read %s: %v", newest, err)
		return
	}
	c := parseMuseHookRuns(raw)
	if c.Terminal == 0 {
		a.museFinding("hook-runs", "unverified", label, "unverified: %s records no hook run yet", filepath.Base(newest))
		return
	}
	a.museFinding("hook-runs", "ok", label, "ok: %d hook run(s) recorded in %s, %d completed", c.Terminal, filepath.Base(newest), c.Completed)
}

// readTail returns the last max bytes of a file.
func readTail(path string, max int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if info.Size() > max {
		if _, err := f.Seek(info.Size()-max, io.SeekStart); err != nil {
			return "", err
		}
	}
	raw, err := io.ReadAll(f)
	return string(raw), err
}

var museManagedLane = regexp.MustCompile(`(?i)managed[ _-]*hooks?[ _-]*(?:lane)?[ _-]*required\W{0,4}(true|yes|on|enabled|false|no|off|disabled)`)

// parseMuseManagedLane reads whether the policy requires the managed hook
// lane from `muse config status` output: "yes", "no" or "unknown". Muse 1.4.1
// prints no such line; the parse exists for a release that does, and answers
// "unknown" otherwise.
func parseMuseManagedLane(out string) string {
	m := museManagedLane.FindStringSubmatch(out)
	if m == nil {
		return "unknown"
	}
	switch strings.ToLower(m[1]) {
	case "true", "yes", "on", "enabled":
		return "yes"
	}
	return "no"
}

// museConfigSource is one `plane=... source_class=... state=...` line of
// `muse config status`.
type museConfigSource struct{ Plane, Class, State string }

var museConfigSourceLine = regexp.MustCompile(`plane=(\S+)\s+source_class=(\S+)\s+state=(\S+)`)

// parseMuseConfigStatus reads the source lines of `muse config status`:
//
//	Enterprise configuration status
//	Generation: sha256:...
//	Sources:
//	  plane=policy source_class=system_file state=absent
func parseMuseConfigStatus(out string) []museConfigSource {
	var srcs []museConfigSource
	for _, line := range strings.Split(out, "\n") {
		if m := museConfigSourceLine.FindStringSubmatch(line); m != nil {
			srcs = append(srcs, museConfigSource{Plane: m[1], Class: m[2], State: m[3]})
		}
	}
	return srcs
}

// 5. the org policy plane, read from `muse config status`. `muse config
// validate` needs `--plane` and `--file` and checks one enterprise document,
// not the machine, so it is not called. The status output names each policy
// source and whether it is present; it does not say what a policy requires, so
// the managed lane is "unknown" unless a policy source is present, and then
// only what a status line says. The only state observed on a binary is
// `absent`; any other state is reported as Muse printed it, and one that names
// an error is a FAIL, because a rejected policy can leave the managed lane
// unenforced.
func (a *app) reportMusePolicy(v providers.MuseVersionCheck) {
	const label = "policy"
	if v.State == providers.MuseVersionNotOnPath {
		a.museFinding("policy", "unverified", label, "not checked: muse is not on PATH")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), museConfigTimeout)
	defer cancel()
	st, err := providers.MuseRunner(ctx, "", "config", "status")
	if err != nil {
		a.museFinding("policy", "warning", label, "WARNING: `muse config status` did not finish: %v", err)
		return
	}
	out := string(st.Stdout) + "\n" + string(st.Stderr)
	if st.ExitCode != 0 {
		a.museFinding("policy", "warning", label, "WARNING: `muse config status` exited %d, so the policy plane could not be read. managed lane required: unknown", st.ExitCode)
		return
	}
	var policy []museConfigSource
	for _, src := range parseMuseConfigStatus(out) {
		if src.Plane == "policy" {
			policy = append(policy, src)
		}
	}
	if len(policy) == 0 {
		a.museFinding("policy", "unverified", label, "unverified: `muse config status` listed no policy source. managed lane required: unknown")
		return
	}
	var parts []string
	present, broken := false, false
	for _, src := range policy {
		parts = append(parts, fmt.Sprintf("%s %s", src.Class, src.State))
		low := strings.ToLower(src.State)
		if low != "absent" {
			present = true
		}
		if strings.Contains(low, "invalid") || strings.Contains(low, "error") || strings.Contains(low, "reject") {
			broken = true
		}
	}
	lane := "unknown"
	if present {
		lane = parseMuseManagedLane(out)
	}
	switch {
	case broken:
		a.museFinding("policy", "fail", label, "FAIL: a Muse policy source is not valid (%s); a rejected policy can leave the managed hook lane unenforced. managed lane required: %s", strings.Join(parts, ", "), lane)
	case present:
		a.museFinding("policy", "ok", label, "ok: policy present (%s). managed lane required: %s", strings.Join(parts, ", "), lane)
		if lane == "yes" {
			a.row("", "a required managed lane is an org mandate; OpenBox's user-level hooks are a")
			a.row("", "second registration, see deployments/managed/muse/README-mdm.md")
		}
	default:
		a.museFinding("policy", "ok", label, "ok: no policy present (%s). managed lane required: %s", strings.Join(parts, ", "), lane)
	}
}

// 6. what is and is not recorded. Said plainly, and never as "not installed":
// the hooks are installed and gate; there is simply no lane that can see a
// Muse model call's body.
func (a *app) reportMuseModelCalls() {
	// Unwrapped: the sentence is a fixed claim and is searched for verbatim.
	const modelCalls = "Muse model calls: not recorded (no proxy or telemetry lane can see them); tool, prompt and model-call gating still enforced"
	a.row("model calls", "%s", modelCalls)
	traceDoctorFinding("muse:model-calls", "info", modelCalls)
	// Why no proxy lane: on an OS with no system proxy activation the relay
	// never sees Muse's traffic; on macOS the relay could, but Muse was never
	// shown to trust its CA or follow the PAC, and without a session carrier
	// a relayed Muse call could not be attributed anyway.
	proxy := "none; this OS has no system proxy activation, so the relay cannot see Muse"
	if systemPACSupportedFn() {
		proxy = "not built; Muse is not shown to trust the relay's CA or follow the system PAC, and sends no session carrier the relay could attribute"
	}
	a.row("", "proxy: %s", proxy)
	traceDoctorFinding("muse:proxy-lane", "info", proxy)
	// Why no telemetry lane: Muse documents no OpenTelemetry exporter that
	// could be pointed at the loopback receiver. A third party claims one
	// exists; until a real install shows its settings, nothing is written.
	const telemetry = "not supported by this tool (no documented OpenTelemetry exporter to point at the loopback receiver)"
	a.row("", "telemetry: %s", telemetry)
	traceDoctorFinding("muse:telemetry-lane", "info", telemetry)
	const mcp = "MCP gating: doc-verified, not empirically confirmed"
	a.row("mcp", "%s", mcp)
	traceDoctorFinding("muse:mcp", "info", mcp)
}

// 7. the spool backlog the hooks left. The details are in the machine-wide
// "Spooled evidence" block, so this row is the Muse-specific pointer.
func (a *app) reportMuseSpool() {
	dir := providers.SpoolDirFor("muse")
	if dir == "" {
		return
	}
	sp := hookflow.Spool{Dir: dir}
	backlog, discarded := sp.BacklogCount(), sp.DiscardedCount()
	switch {
	case backlog == 0 && discarded == 0:
		a.museFinding("spool", "ok", "spool", "ok: 0 events waiting in %s", dir)
	case backlog == 0:
		a.museFinding("spool", "warning", "spool", "WARNING: nothing waiting, but at least %d event(s) were discarded in %s", discarded, dir)
	default:
		a.museFinding("spool", "warning", "spool", "WARNING: %d event(s) waiting in %s; `openbox hook muse flush` delivers them now", backlog, dir)
	}
}

// 8. tool actions Muse ran that no hook gated, found by reading Muse's own
// session journal at Stop and SessionEnd. A count only: the findings are in
// the local trace, and nothing here reaches core. Detection, not prevention.
func (a *app) reportMuseEvidenceGaps() {
	const label = "ungated"
	sum, err := providers.SummarizeMuseEvidence(trace.Dir(), time.Now())
	switch {
	case err != nil:
		a.museFinding("evidence-gaps", "unverified", label, "unverified: the local trace could not be read (%v)", err)
	case sum.Unverified:
		a.museFinding("evidence-gaps", "unverified", label, "unverified: the session-log reconciler stopped on a journal line it does not recognise, because Muse's session.jsonl format is not verified; %d ungated Muse action(s) in the last 7 days before that", sum.Gaps)
	case sum.Gaps == 0:
		a.museFinding("evidence-gaps", "ok", label, "ok: 0 ungated Muse actions in the last 7 days (see `openbox trace <session>`)")
	default:
		a.museFinding("evidence-gaps", "warning", label, "WARNING: %d ungated Muse actions in the last 7 days (see `openbox trace <session>`). The usual cause is a tool payload over Muse's 256 KiB hook limit, which skips every hook; this is found after the fact and cannot be prevented", sum.Gaps)
	}
}
