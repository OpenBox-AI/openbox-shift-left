package main

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"sort"

	claudecode "github.com/openbox-ai/openbox-shift-left/adapters/claude-code"
	"github.com/openbox-ai/openbox-shift-left/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/managed"
	"github.com/openbox-ai/openbox-shift-left/decision"
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
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(a.stderr)
	if err := fs.Parse(args); err != nil {
		return exitError
	}

	p := devconfig.EffectivePosture()
	// Bundle coordinates are provider-independent (one shared bundle), so doctor
	// resolves them directly rather than through an adapter.
	bundlePath := claudecode.ResolveBundlePath()
	pubKeyB64, _ := devconfig.ResolveOrgSigningKey()
	if b, integrity := decision.VerifyBundleFile(bundlePath, decision.VerifyOptions{
		PublicKey: decision.DecodePublicKey(pubKeyB64),
		MinEpoch:  decision.ReadEpochPin(bundlePath),
	}); b != nil {
		p.BundlePolicyID, p.BundleIntegrity = b.PolicyID, string(integrity)
		p.BundleVersion = b.Version
	} else {
		p.BundleIntegrity = string(integrity)
	}
	if raw, err := os.ReadFile(bundlePath); err == nil {
		sum := sha256.Sum256(raw)
		p.BundleSHA256 = hex.EncodeToString(sum[:])
	}
	fmt.Fprintf(a.stdout, "OpenBox developer-runtime posture\n\n")

	fmt.Fprintf(a.stdout, "Enforcement\n")
	flags := map[string]bool{
		"enforce": p.Enforce, "fail_closed": p.FailClosed, "tier2": p.Tier2,
		"secret_detection": p.SecretDetection, "content_capture": p.ContentCapture,
		"findings": p.Findings, "finops": p.Finops,
	}
	names := make([]string, 0, len(flags))
	for n := range flags {
		names = append(names, n)
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
		fmt.Fprintf(a.stdout, "  %-18s %-5v  from %s%s\n", n, flags[n], source, note)
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
	}

	fmt.Fprintf(a.stdout, "\nPolicy bundle\n")
	fmt.Fprintf(a.stdout, "  path       %s\n", claudecode.ResolveBundlePath())
	fmt.Fprintf(a.stdout, "  policy id  %s\n", orUnset(p.BundlePolicyID))
	fmt.Fprintf(a.stdout, "  sha256     %s\n", orUnset(p.BundleSHA256))
	fmt.Fprintf(a.stdout, "  integrity  %s%s\n", orUnset(p.BundleIntegrity), integrityNote(p.BundleIntegrity))

	fmt.Fprintf(a.stdout, "\nProvider managed configuration\n")
	for _, prov := range []managed.Provider{managed.ProviderClaudeCode, managed.ProviderCodex} {
		state := managed.ProviderState(prov)
		fmt.Fprintf(a.stdout, "  %-12s %s\n", prov, state)
	}

	fmt.Fprintf(a.stdout, "\nWhat this does and does not prove\n")
	fmt.Fprintf(a.stdout, "  Settings sourced from `user` or `env` can be changed by whoever runs this\n")
	fmt.Fprintf(a.stdout, "  command, so they are not assurance. Only `managed` values, and only with the\n")
	fmt.Fprintf(a.stdout, "  provider config deployed, survive a developer who does not want them.\n")
	return exitOK
}

func integrityNote(integrity string) string {
	switch integrity {
	case "verified":
		return "  (signature, epoch and expiry all checked)"
	case "unsigned":
		return "  (no signature — a local edit would not be detectable)"
	case "no_key":
		return "  (signed, but no org key pinned — pin org_signing_pubkey to check it)"
	case "":
		return ""
	default:
		return "  (NOT TRUSTED — run `openbox dev sync`)"
	}
}
