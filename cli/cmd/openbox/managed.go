package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/openbox-ai/openbox-shift-left/cli/internal/managed"
)

// runManaged installs the provider-level configuration that makes governance an
// org mandate rather than a per-developer opt-in (E8-S8).
func (a *app) runManaged(args []string) int {
	if len(args) == 0 {
		return a.errorf("usage: openbox managed install --provider <claude-code,codex> [--dry-run] [--force]")
	}
	if args[0] != "install" {
		return a.errorf("unknown managed subcommand %q (only `install`)", args[0])
	}

	fs := flag.NewFlagSet("managed install", flag.ContinueOnError)
	fs.SetOutput(a.stderr)
	providers := fs.String("provider", "", "comma-separated: claude-code,codex")
	dryRun := fs.Bool("dry-run", false, "print what would be written and exit")
	force := fs.Bool("force", false, "replace an existing file even if it is stricter than the template")
	binPath := fs.String("bin", "", "absolute path to the openbox binary the hooks invoke (default: this binary)")
	if err := fs.Parse(args[1:]); err != nil {
		return exitError
	}
	if strings.TrimSpace(*providers) == "" {
		return a.errorf("--provider is required (claude-code,codex)")
	}

	var wanted []managed.Provider
	for _, p := range strings.Split(*providers, ",") {
		switch p = strings.TrimSpace(p); p {
		case "claude-code":
			wanted = append(wanted, managed.ProviderClaudeCode)
		case "codex":
			wanted = append(wanted, managed.ProviderCodex)
		case "cursor":
			return a.errorf("cursor has no managed template yet — it lands with the SL-8 adapter")
		case "":
			continue
		default:
			return a.errorf("unknown provider %q", p)
		}
	}

	bin := *binPath
	if bin == "" {
		// The running binary is the honest default: whatever invoked this command
		// is what is actually installed on the machine.
		exe, err := os.Executable()
		if err != nil {
			return a.errorf("could not determine this binary's path; pass --bin: %v", err)
		}
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			exe = resolved
		}
		bin = exe
	}

	plan, err := managed.PlanInstall(wanted, bin)
	if err != nil {
		return a.errorf("%v", err)
	}
	for _, w := range plan.Warnings {
		fmt.Fprintf(a.stderr, "warning: %s\n", w)
	}

	// Printing is a first-class outcome, not a failure mode: on a machine without
	// privileges the operator's next step is to hand these files to the MDM team,
	// and exiting non-zero would make that look like an error.
	if *dryRun || !managed.Privileged(plan) {
		why := "dry run"
		if !*dryRun {
			why = "insufficient privileges to write the system paths"
		}
		fmt.Fprintf(a.stdout, "Not writing (%s). Deploy these files via your MDM or re-run with sudo:\n\n", why)
		for _, f := range plan.Files {
			fmt.Fprintf(a.stdout, "--- %s (mode %04o) ---\n%s\n", f.Path, f.Mode, f.Contents)
		}
		fmt.Fprintf(a.stdout, "See deploy/managed/README.md for what each setting does and does not guarantee.\n")
		return exitOK
	}

	outcomes, err := managed.Apply(plan, *force, nil)
	for _, o := range outcomes {
		switch o.Action {
		case "written":
			fmt.Fprintf(a.stdout, "wrote %s\n", o.Path)
			if o.BackupPath != "" {
				fmt.Fprintf(a.stdout, "  previous file backed up to %s\n", o.BackupPath)
			}
		case "unchanged":
			fmt.Fprintf(a.stdout, "unchanged %s\n", o.Path)
		case "skipped":
			fmt.Fprintf(a.stdout, "SKIPPED %s: %s\n", o.Path, o.Detail)
		}
	}
	if err != nil {
		return a.errorf("%v", err)
	}
	fmt.Fprintf(a.stdout, "\nManaged configuration installed. Verify with `openbox doctor`.\n")
	return exitOK
}
