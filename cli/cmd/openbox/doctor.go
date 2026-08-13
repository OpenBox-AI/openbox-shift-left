package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/openbox-ai/openbox-shift-left/adapters/common/hookflow"
	"os"
	"sort"
	"strings"

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
	fs := a.newFlagSet("doctor")
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}

	p := devconfig.EffectivePosture()
	// Bundle coordinates are provider-independent (one shared bundle), so doctor
	// resolves them directly rather than through an adapter.
	bundlePath := hookflow.ResolveBundlePath()
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
