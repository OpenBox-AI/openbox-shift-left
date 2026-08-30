// Package managed installs the provider-level configuration that turns OpenBox
// governance from a per-developer opt-in into an org mandate (E8-S8).
package managed

import (
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
)

//go:embed templates
var templates embed.FS

// Provider is a coding tool whose managed configuration we can render.
type Provider string

const (
	ProviderClaudeCode Provider = "claude-code"
	ProviderCodex      Provider = "codex"
)

const binPlaceholder = "OPENBOX_BIN"

// File is one rendered managed-config file and where it belongs.
type File struct {
	Path     string // absolute target path for this OS
	Contents []byte
	// Mode is deliberately world-readable but owner-only-writable: the developer
	// must be able to read the config the tool applies, and must not be able to
	// change it.
	Mode os.FileMode
}

// Plan is what an install would do, without doing any of it.
type Plan struct {
	Files []File
	// Warnings are non-fatal notes for the operator (an unsupported OS, a
	// provider with no template yet).
	Warnings []string
}

// PlanInstall renders the managed configuration for the given providers.
// OpenboxBin is the absolute path the hooks should invoke.
func PlanInstall(providers []Provider, openboxBin string) (Plan, error) {
	if strings.TrimSpace(openboxBin) == "" {
		return Plan{}, fmt.Errorf("the openbox binary path is required (managed hooks cannot use a relative command)")
	}
	if !filepath.IsAbs(openboxBin) {
		return Plan{}, fmt.Errorf("openbox binary path %q must be absolute", openboxBin)
	}

	var plan Plan
	for _, p := range providers {
		files, warns, err := planProvider(p, openboxBin)
		if err != nil {
			return Plan{}, err
		}
		plan.Files = append(plan.Files, files...)
		plan.Warnings = append(plan.Warnings, warns...)
	}
	if len(plan.Files) == 0 {
		return plan, fmt.Errorf("no managed configuration to install for the requested providers")
	}
	return plan, nil
}

func planProvider(p Provider, openboxBin string) ([]File, []string, error) {
	switch p {
	case ProviderClaudeCode:
		dir, warn := claudeCodeDir()
		if dir == "" {
			return nil, []string{warn}, nil
		}
		body, err := render("templates/claude-code/managed-settings.json", openboxBin)
		if err != nil {
			return nil, nil, err
		}
		return []File{{Path: filepath.Join(dir, "managed-settings.json"), Contents: body, Mode: 0o644}}, nil, nil

	case ProviderCodex:
		dir, warn := codexDir()
		if dir == "" {
			return nil, []string{warn}, nil
		}
		var out []File
		for _, name := range []string{"requirements.toml", "managed_config.toml"} {
			body, err := render("templates/codex/"+name, openboxBin)
			if err != nil {
				return nil, nil, err
			}
			out = append(out, File{Path: filepath.Join(dir, name), Contents: body, Mode: 0o644})
		}
		return out, nil, nil

	default:
		return nil, nil, fmt.Errorf("no managed configuration template for provider %q "+
			"(Cursor lands with the SL-8 adapter)", p)
	}
}

func render(name, openboxBin string) ([]byte, error) {
	raw, err := templates.ReadFile(name)
	if err != nil {
		return nil, fmt.Errorf("read template %s: %w", name, err)
	}
	return []byte(strings.ReplaceAll(string(raw), binPlaceholder, openboxBin)), nil
}

// ClaudeCodeManagedDir exposes the managed-settings directory so `openbox
// doctor` resolves it through this package rather than re-deriving the path.
func ClaudeCodeManagedDir() string {
	dir, _ := claudeCodeDir()
	return dir
}

func claudeCodeDir() (dir, warning string) {
	switch runtime.GOOS {
	case "linux":
		return "/etc/claude-code", ""
	case "darwin":
		return "/Library/Application Support/ClaudeCode", ""
	case "windows":
		return `C:\ProgramData\ClaudeCode`, ""
	default:
		return "", fmt.Sprintf("claude-code: no known managed-settings path for %s — "+
			"deploy the template by hand", runtime.GOOS)
	}
}

func codexDir() (dir, warning string) {
	switch runtime.GOOS {
	case "linux", "darwin":
		return "/etc/codex", ""
	default:
		return "", fmt.Sprintf("codex: no known managed-config path for %s — "+
			"deploy the template by hand", runtime.GOOS)
	}
}

// Outcome describes what Apply did to one file.
type Outcome struct {
	Path       string
	Action     string // "written" | "unchanged" | "skipped"
	BackupPath string // non-empty when an existing file was preserved
	Detail     string
}

// Apply writes the planned files.
//   - Idempotent: a file whose contents already match is left alone, so a
//     config-management loop does not churn timestamps or trigger reload
//     storms.
//   - Backed up: an existing different file is copied aside before being
//     replaced, because this overwrites org security configuration and "I can
//     put it back" has to be true.
//   - Refuses to weaken: if what is already there is stricter than the
//     template, the file is skipped rather than relaxed.
func Apply(plan Plan, force bool, now func() time.Time) ([]Outcome, error) {
	if now == nil {
		now = time.Now
	}
	var outcomes []Outcome
	for _, f := range plan.Files {
		outcome, err := applyFile(f, force, now)
		if err != nil {
			return outcomes, err
		}
		outcomes = append(outcomes, outcome)
	}
	return outcomes, nil
}

func applyFile(f File, force bool, now func() time.Time) (Outcome, error) {
	existing, readErr := os.ReadFile(f.Path)
	switch {
	case readErr == nil && string(existing) == string(f.Contents):
		return Outcome{Path: f.Path, Action: "unchanged"}, nil

	case readErr == nil:
		if !force {
			if weaker, why := wouldWeaken(existing, f.Contents); weaker {
				return Outcome{
					Path:   f.Path,
					Action: "skipped",
					Detail: why + " — re-run with --force to replace it anyway",
				}, nil
			}
		}
		backup := f.Path + ".openbox-backup-" + now().UTC().Format("20060102T150405Z")
		if err := os.WriteFile(backup, existing, 0o600); err != nil {
			return Outcome{}, fmt.Errorf("back up %s: %w", f.Path, err)
		}
		if err := writeFile(f); err != nil {
			return Outcome{}, err
		}
		return Outcome{Path: f.Path, Action: "written", BackupPath: backup}, nil

	case os.IsNotExist(readErr):
		if err := writeFile(f); err != nil {
			return Outcome{}, err
		}
		return Outcome{Path: f.Path, Action: "written"}, nil

	default:
		return Outcome{}, fmt.Errorf("read %s: %w", f.Path, readErr)
	}
}

func writeFile(f File) error {
	if err := os.MkdirAll(filepath.Dir(f.Path), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(f.Path), err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(f.Path), ".openbox-managed-*")
	if err != nil {
		return fmt.Errorf("stage %s: %w", f.Path, err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(f.Contents); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %w", f.Path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write %s: %w", f.Path, err)
	}
	if err := os.Chmod(tmp.Name(), f.Mode); err != nil {
		return fmt.Errorf("chmod %s: %w", f.Path, err)
	}
	if err := os.Rename(tmp.Name(), f.Path); err != nil {
		return fmt.Errorf("install %s: %w", f.Path, err)
	}
	return nil
}

var strictnessMarkers = []struct {
	marker string
	why    string
}{
	{`"allowManagedHooksOnly": true`, "the existing file already restricts hooks to managed ones"},
	{`allow_managed_hooks_only = true`, "the existing file already restricts hooks to managed ones"},
	{`"allowManagedPermissionRulesOnly": true`, "the existing file already restricts permission rules to managed ones"},
	{`"failIfUnavailable": true`, "the existing file already requires a working sandbox"},
	{`"allowUnsandboxedCommands": false`, "the existing file already forbids unsandboxed commands"},
	{`"disableBypassPermissionsMode": "disable"`, "the existing file already disables bypass-permissions mode"},
	{`"strictPluginOnlyCustomization": true`, "the existing file already restricts customization to plugins"},
}

// wouldWeaken reports whether replacing existing with incoming would drop a
// strictness marker. The two sides are matched asymmetrically, and
// deliberately so.
func wouldWeaken(existing, incoming []byte) (bool, string) {
	have, want := string(existing), uncommented(incoming)
	for _, m := range strictnessMarkers {
		if strings.Contains(have, m.marker) && !strings.Contains(want, m.marker) {
			return true, m.why
		}
	}
	return false, ""
}

func uncommented(raw []byte) string {
	var b strings.Builder
	for _, line := range strings.Split(string(raw), "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "#") || strings.HasPrefix(t, "//") {
			continue
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// Privileged reports whether this process can write the planned paths, so the
// CLI can fall back to printing instead of failing.
func Privileged(plan Plan) bool {
	for _, f := range plan.Files {
		dir := filepath.Dir(f.Path)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return false
		}
		probe, err := os.CreateTemp(dir, ".openbox-probe-*")
		if err != nil {
			return false
		}
		probe.Close()
		os.Remove(probe.Name())
	}
	return true
}

// ProviderState reports whether a provider's managed configuration is deployed
// on this machine, for `openbox doctor` and for posture.provider_managed
// (E8-S8).
func ProviderState(p Provider) string {
	var dir string
	var files []string
	switch p {
	case ProviderClaudeCode:
		dir, _ = claudeCodeDir()
		files = []string{"managed-settings.json"}
	case ProviderCodex:
		dir, _ = codexDir()
		files = []string{"requirements.toml"}
	default:
		return "unknown (no template for this provider)"
	}
	if dir == "" {
		return "unknown (no managed path known for this OS)"
	}
	for _, name := range files {
		path := filepath.Join(dir, name)
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if mandates(p, raw) {
			return "managed (" + path + ")"
		}
		return "present but imposes no OpenBox mandate (" + path + ")"
	}
	return "not managed (no " + filepath.Join(dir, files[0]) + ")"
}

var codexRequirementKeys = []string{
	"allow_managed_hooks_only",
	"allowed_approval_policies",
	"allowed_sandbox_modes",
}

func mandates(p Provider, raw []byte) bool {
	if p == ProviderCodex {
		keys := devconfig.TopLevelTOMLKeys(raw)
		for _, k := range codexRequirementKeys {
			if keys[k] {
				return true
			}
		}
		return false
	}
	return strings.Contains(string(raw), "hook "+string(p))
}
