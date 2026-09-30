package muse

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
	providerspi "github.com/openbox-ai/openbox-shift-left/internal/provider"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// CredentialRef is the install-time seam's credential coordinate type: the
// non-secret coordinates only, never a key value.
type CredentialRef = providerspi.CredentialRef

// Handler ceilings, in seconds. A gated handler may hold for a real approval
// decision, so its ceiling is the engine's own gating ceiling; a tighter one
// would kill the hook mid-decision and let the call through. SessionEnd is
// short because nothing waits on it, and the successor only ever exits.
const (
	otherHookTimeoutSec      = 5
	sessionEndHookTimeoutSec = 3
	successorTimeoutSec      = 5
)

func gatedHookTimeoutSec() int { return int(Engine{}.HookCeilings().Gating.Seconds()) }

func timeoutFor(ev HookName) int {
	switch {
	case ev == HookSessionEnd:
		return sessionEndHookTimeoutSec
	case ev.Gated():
		return gatedHookTimeoutSec()
	}
	return otherHookTimeoutSec
}

// installedEvents are the events this installer registers a handler for: every
// event the adapter produces something for (HookName.Observed), in name order.
// It is derived from the adapter's own table, so an event added there reaches
// the installer, the uninstall and doctor's expected count together. The five
// events with no contract type are never registered, and neither are Stop and
// SubagentStop, whose handler would start a process to report nothing.
func installedEvents() []HookName {
	events := make([]HookName, 0, len(hookNames))
	for h := range hookNames {
		if h.Observed() {
			events = append(events, h)
		}
	}
	sort.Slice(events, func(a, b int) bool { return events[a] < events[b] })
	return events
}

// ExpectedHandlers is how many OpenBox handlers a complete install registers.
func ExpectedHandlers() int { return len(installedEvents()) }

// Installer writes OpenBox's hook handlers into Muse's settings.json and the
// non-secret dev config, delegated from `openbox init` (the provider seam).
type Installer struct {
	// EngineBinary is the absolute path of the openbox binary the handlers run.
	EngineBinary string
	// SettingsPath is Muse's settings file (default: SettingsPath()).
	SettingsPath string
	// ConfigPath is where the dev config is written (default: the bound tool's).
	ConfigPath string
	// Runner runs `muse` for the version gate (default: ExecRunner).
	Runner Runner
}

// Name reports the provider this installer serves.
func (Installer) Name() providerspi.Name { return providerspi.Muse }

// Preflight is the version gate `openbox init` runs before it registers
// anything. Muse older than MinVersion refuses: its onFailure successor does
// not cover an answer Muse rejects, so the gate would fail open, and so does a
// muse whose version cannot be read, since nothing proves it is new enough. A
// muse that is not on PATH (the hooks may precede it) or is newer than the
// tested range installs and returns a warning instead; doctor repeats it.
func (i Installer) Preflight() (warning string, err error) {
	c := CheckVersion(i.Runner)
	switch c.State {
	case VersionTooOld:
		return "", fmt.Errorf("%s. Upgrade muse to %s or newer and run `openbox init --provider muse` again; "+
			"older releases let a failed or malformed gate answer through as an allow. Nothing was written", c.Detail, MinVersion)
	case VersionNotOnPath:
		return "muse is not on PATH, so its version was not checked; the hooks are installed and take effect once muse runs. `openbox doctor` checks the version", nil
	case VersionUnreadable:
		return "", fmt.Errorf("could not read muse's version (%s), so it cannot be shown to be %s or newer; "+
			"older releases let a failed or malformed gate answer through as an allow. Fix `muse --version` and run "+
			"`openbox init --provider muse` again. Nothing was written", c.Detail, MinVersion)
	case VersionUntested:
		return c.Detail + "; the hooks are installed anyway, but nothing here has been tested on it", nil
	}
	return "", nil
}

// Install merges the OpenBox handlers into settings.json and writes the dev
// config. Idempotent: a re-run replaces the owned handlers in place, never
// duplicates them, and never modifies or removes a foreign one. A settings file
// Muse could not read is refused untouched.
func (i Installer) Install(ref CredentialRef) error {
	if ref.AgentID == "" {
		return errors.New("muse install: CredentialRef.AgentID is required")
	}
	if _, err := i.Preflight(); err != nil {
		return fmt.Errorf("muse install: %w", err)
	}
	// The settings are prepared (read, validated, merged, checked) before
	// anything is written, and committed only after the posture is: a failure in
	// either must never leave live hooks with no dev.json behind them, nor a
	// posture written for hooks that were refused.
	plan, err := i.prepareSettings()
	if err != nil {
		return err
	}
	if err := devconfig.WriteConfig(i.configPath(), providerspi.ConfigUpdate(ref)); err != nil {
		return fmt.Errorf("muse install: %w", err)
	}
	return i.commitSettings(plan)
}

// ownedHandler is the JSON of one handler the installer writes. Field order is
// the order the file reads in.
type ownedHandler struct {
	Type      string          `json:"type"`
	Command   string          `json:"command"`
	Timeout   int             `json:"timeout"`
	OnFailure *ownedSuccessor `json:"onFailure,omitempty"`
}

type ownedSuccessor struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout"`
}

// handlerFor builds the catch-all group holding one event's handler. No
// matcher: a matcher would leave every tool it does not name ungoverned, and
// MCP tools carry names no list could anticipate.
func (i Installer) handlerFor(ev HookName, home string) ([]byte, error) {
	cmd, err := formatInvocation(i.EngineBinary, home, false, ev)
	if err != nil {
		return nil, err
	}
	h := ownedHandler{Type: "command", Command: cmd, Timeout: timeoutFor(ev)}
	if ev.Gated() {
		succ, err := formatInvocation(i.EngineBinary, home, true, ev)
		if err != nil {
			return nil, err
		}
		h.OnFailure = &ownedSuccessor{Type: "command", Command: succ, Timeout: successorTimeoutSec}
	}
	return json.Marshal(struct {
		Hooks []ownedHandler `json:"hooks"`
	}{[]ownedHandler{h}})
}

// settingsPlan is a merged settings document ready to commit.
type settingsPlan struct {
	path    string
	before  []byte
	out     []byte
	existed bool
	perm    os.FileMode
	home    string
}

func (i Installer) prepareSettings() (*settingsPlan, error) {
	path := i.settingsPath()
	// A dotfiles manager often makes settings.json a symlink; renaming over the
	// link would replace it with a regular file, so write through it.
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}

	before, err := os.ReadFile(path)
	existed := err == nil
	perm := os.FileMode(0o600)
	switch {
	case err == nil:
		if info, statErr := os.Stat(path); statErr == nil {
			perm = info.Mode().Perm()
		}
		if _, vErr := ValidateSettings(before); vErr != nil {
			return nil, fmt.Errorf("muse install: refusing to modify %s: %w; Muse drops every hook in a file it cannot read, so fix or remove it and run init again. Nothing was written", path, vErr)
		}
	case os.IsNotExist(err):
		before = nil
	default:
		return nil, fmt.Errorf("muse install: read %s: %w", path, err)
	}

	out := before
	if !existed {
		out, err = sjson.SetBytes([]byte("{}"), "schema_version", settingsSchemaVersion)
		if err != nil {
			return nil, fmt.Errorf("muse install: schema_version: %w", err)
		}
	}
	home := BakedHome()
	for _, ev := range installedEvents() {
		group, err := i.handlerFor(ev, home)
		if err != nil {
			return nil, fmt.Errorf("muse install: %w", err)
		}
		if out, err = mergeEvent(out, ev, group); err != nil {
			return nil, fmt.Errorf("muse install: settings.json event %s: %w", ev, err)
		}
	}
	if existed {
		out = keepTrailingSpace(out, before)
	} else {
		var indented bytes.Buffer
		if json.Indent(&indented, out, "", "  ") == nil {
			out = append(indented.Bytes(), '\n')
		}
	}
	if err := i.verify(out, home); err != nil {
		return nil, fmt.Errorf("muse install: refusing to write %s: %w", path, err)
	}
	return &settingsPlan{path: path, before: before, out: out, existed: existed, perm: perm, home: home}, nil
}

func (i Installer) commitSettings(p *settingsPlan) error {
	path, before, out, existed, perm, home := p.path, p.before, p.out, p.existed, p.perm, p.home
	if existed && bytes.Equal(out, before) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("muse install: settings dir: %w", err)
	}
	if err := hookflow.AtomicWriteFile(path, out, perm); err != nil {
		return fmt.Errorf("muse install: commit %s: %w", path, err)
	}
	// Re-read what landed on disk under the same rules: a short or altered write
	// is a machine that reads as governed and is not.
	written, rerr := os.ReadFile(path)
	if rerr == nil {
		rerr = i.verify(written, home)
	}
	if rerr != nil {
		i.restore(path, before, existed, perm)
		return fmt.Errorf("muse install: %s did not read back as a valid install (%w); the previous file was restored", path, rerr)
	}
	return nil
}

// restore puts the previous settings back, or removes the file this run made.
func (i Installer) restore(path string, before []byte, existed bool, perm os.FileMode) {
	if !existed {
		_ = os.Remove(path)
		return
	}
	_ = hookflow.AtomicWriteFile(path, before, perm)
}

// verify is the post-merge check: the document reads under Muse's rules, and
// it holds exactly the handlers an install owes, each pointing at this engine.
func (i Installer) verify(doc []byte, home string) error {
	parsed, err := parseSettings(doc)
	if err != nil {
		return err
	}
	audit := auditDoc(parsed, home)
	if problems := audit.Problems(); len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	for _, o := range audit.Owned {
		if o.Engine != i.EngineBinary {
			return fmt.Errorf("%s handler runs %q, not %q", o.Event, o.Engine, i.EngineBinary)
		}
	}
	return nil
}

// mergeEvent replaces one event's owned handlers with group. Owned handlers are
// removed wherever they sit, then the group is appended; the splice is skipped
// when the event already says what it should, so a re-run leaves the file
// byte-identical rather than reformatting an event it owns.
func mergeEvent(doc []byte, ev HookName, group []byte) ([]byte, error) {
	path := "hooks." + escapeKey(string(ev))
	existing := gjson.GetBytes(doc, path)
	next, _, err := dropOwned(doc, string(ev))
	if err != nil {
		return nil, err
	}
	next, err = sjson.SetRawBytes(next, path+".-1", group)
	if err != nil {
		return nil, err
	}
	if existing.Exists() && canonicalJSONEqual([]byte(existing.Raw), []byte(gjson.GetBytes(next, path).Raw)) {
		return doc, nil
	}
	return next, nil
}

// keepTrailingSpace gives out the whitespace after the closing brace that
// before had. sjson drops it when it adds a key to the root object, and a file
// that lost its final newline is a diff in the developer's own settings.
func keepTrailingSpace(out, before []byte) []byte {
	const space = " \t\r\n"
	trailing := before[len(bytes.TrimRight(before, space)):]
	return append(bytes.TrimRight(out, space), trailing...)
}

// canonicalJSONEqual compares two JSON values by content, not bytes.
func canonicalJSONEqual(a, b []byte) bool {
	var av, bv any
	if json.Unmarshal(a, &av) != nil || json.Unmarshal(b, &bv) != nil {
		return false
	}
	ac, aErr := json.Marshal(av)
	bc, bErr := json.Marshal(bv)
	return aErr == nil && bErr == nil && bytes.Equal(ac, bc)
}

// escapeKey makes a key safe inside a gjson/sjson path, so an event name read
// from a file can never be taken as path syntax.
func escapeKey(key string) string {
	var b strings.Builder
	for _, r := range key {
		if strings.ContainsRune(`.*?|#@\!:<>=%"`, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

func (i Installer) settingsPath() string {
	if i.SettingsPath != "" {
		return i.SettingsPath
	}
	return SettingsPath()
}

func (i Installer) configPath() string {
	if i.ConfigPath != "" {
		return i.ConfigPath
	}
	if p, err := devconfig.DevConfigWritePath(); err == nil {
		return p
	}
	return devconfig.DefaultConfigPath()
}

var _ providerspi.Installer = Installer{}
