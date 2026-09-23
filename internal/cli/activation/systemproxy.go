package activation

import (
	"context"
	"crypto/sha1" //nolint:gosec // identity match against `security`'s own SHA-1-keyed
	// fingerprint format, never a security boundary: matching the wrong hash
	// algorithm here means the read-back check below never confirms anything.
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
)

// systemEntrySchema versions SystemEntry independently of Record's own
// recordSchema: a machine's first system-scope activation introduces this
// field from nothing, so there is no prior shape to migrate and no reason to
// tie its version to the per-lane Entry's.
const systemEntrySchema = "openbox.dev-runtime.system-activation/v1"

// Runner executes one OS command as argv, privileged or not, and never
// through a shell: every caller in this file goes through it, so a test can
// record exactly what would have run without sudo, networksetup, security,
// osascript or a shell ever executing. A non-nil error means the command
// exited non-zero (or could not start); this package never inspects an exit
// code directly, matching what exec.Cmd.CombinedOutput's error already
// encodes.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// ExecRunner is the production Runner: os/exec, argv only, no shell layer.
// This package's own tests never call it -- every test supplies a fake that
// fails the test if a real privileged binary would run -- so `go test` here
// never shells out to sudo, networksetup or security.
func ExecRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// OutcomeClass is one of the four shapes a privileged system-PAC step can
// end in. A `switch` over it with no default is meant to fail loudly at
// compile time (govet's exhaustive checks aside) if a fifth ever gets added
// without every caller being taught about it.
type OutcomeClass string

const (
	// NotAttempted means the preflight found no controlling terminal
	// (`/dev/tty`), so nothing was even asked: no Runner call happened.
	NotAttempted OutcomeClass = "not_attempted"
	// Declined means the one elevation prompt (`sudo -v`) was refused.
	Declined OutcomeClass = "declined"
	// Failed means an attempt was made and something after that point did
	// not work: sudo's credential cache, the trust write, its read-back, a
	// scope's PAC write, or its read-back. Reason says which.
	Failed OutcomeClass = "failed"
	// Activated means every enabled scope carries the PAC URL and the CA is
	// trusted, both confirmed by a read-back.
	Activated OutcomeClass = "activated"
)

// Plan is what ActivateSystemPAC needs to know before it touches anything.
type Plan struct {
	// HomeDir is where activation.json lives (RecordPath(HomeDir)).
	HomeDir string
	// PACURL is the relay's PAC endpoint, e.g. "http://127.0.0.1:8790/proxy.pac"
	// -- never "localhost": some clients resolve that name differently than
	// the loopback IP the relay actually listens on.
	PACURL string
	// CAPath is the file path passed to `security add-trusted-cert` /
	// `remove-trusted-cert`, recorded verbatim so Deactivate can name the
	// same file later, before the caller (a later phase's uninstall) deletes
	// it.
	CAPath string
	// CAPEM is CAPath's own bytes, so this package computes the SHA-1
	// fingerprint itself rather than trusting a caller-supplied one. This
	// package treats CAPEM as advisory input from the caller's environment,
	// not attacker-controlled, matching every other lane's own trust of its
	// caller.
	CAPEM []byte
	// Providers is the union recorded verbatim; it does not affect what this
	// package does with the OS, only what the record and report say a
	// developer can act on later (a shrink-on-uninstall becomes a later
	// Activate call with a smaller Providers, not this package's job).
	Providers []string
}

// ProxyScope is one network service's prior auto-proxy state, captured
// before the first privileged write and carried in the record for Restore.
// The three-state shape (present-and-set / present-and-disabled / never set)
// mirrors activation.Original for the same reason: `networksetup` cannot
// write an empty URL back, so a prior of "(null)" must restore via
// `-setautoproxystate off`, never `-setautoproxyurl ""`.
type ProxyScope struct {
	Service         string `json:"service"`
	PriorURLPresent bool   `json:"prior_url_present"`
	PriorURL        string `json:"prior_url,omitempty"`
	PriorEnabled    bool   `json:"prior_enabled"`
}

// ScopeState is one network service's LIVE auto-proxy reading -- the
// read-only shape LiveSystemPAC and doctor consume, never persisted.
type ScopeState struct {
	Service    string
	URLPresent bool
	URL        string
	Enabled    bool
}

// CATrustState is what OpenBox trusted and where, so Deactivate can name the
// exact same file and fingerprint later without re-deriving either.
type CATrustState struct {
	SHA1     string `json:"sha1"`
	CAPath   string `json:"ca_path"`
	Keychain string `json:"keychain"`
}

// SystemEntry is the machine-wide PAC/CA-trust activation, Record's `System`
// field. Pending is true from the moment this is first persisted (before the
// first privileged write) until the run either finishes or fails; a process
// killed in between leaves Pending true, which a later run's reconcile step
// reads to recognise its own half-applied state rather than recording its
// own PAC URL as some prior value that was never really there.
type SystemEntry struct {
	Schema       string        `json:"schema"`
	ActivatedAt  string        `json:"activated_at,omitempty"`
	Pending      bool          `json:"pending,omitempty"`
	Declined     bool          `json:"declined,omitempty"`
	Failed       bool          `json:"failed,omitempty"`
	Reason       string        `json:"reason,omitempty"`
	PACActivated bool          `json:"pac_activated"`
	PACURL       string        `json:"pac_url,omitempty"`
	Providers    []string      `json:"providers,omitempty"`
	Scopes       []ProxyScope  `json:"scopes,omitempty"`
	CATrust      *CATrustState `json:"ca_trust,omitempty"`
}

// Outcome is ActivateSystemPAC's result.
type Outcome struct {
	Class OutcomeClass
	// Reason is set for Failed (and, for the unsupported-OS arm, for
	// NotAttempted) explaining what did not work or why nothing was tried.
	Reason string
	// Manual is the exact command sequence a developer can run by hand,
	// non-nil for NotAttempted/Declined/Failed on an OS whose shapes are
	// measured (never for the unsupported-OS arm: those shapes are
	// unmeasured, and printing a guess is worse than printing nothing).
	Manual []string
	// Entry is the record written for this outcome; nil only when nothing
	// was persisted (the unsupported-OS arm, which touches no file either).
	Entry *SystemEntry
}

// Report is DeactivateSystemPAC's result.
type Report struct {
	// Class is set only when deactivation itself could not be attempted or
	// failed (NotAttempted/Declined/Failed); empty on a completed run,
	// successful or not for every scope -- Drift is how a partial success is
	// reported, not a Report-level failure.
	Class    OutcomeClass
	Reason   string
	Manual   []string
	Restored []string
	// Drift names a scope whose LIVE URL was not ours; left untouched rather
	// than overwritten, and reported so a developer can see it.
	Drift []string
}

// currentGOOS is a seam: sysmacos.go's and sysunsupported.go's logic are
// both plain Go (argv strings through Runner, no OS-specific syscalls), so
// nothing here needs a build tag to compile on every platform -- only this
// switch decides which arm actually runs, and a test can override it to
// exercise the unsupported arm from a macOS development machine.
var currentGOOS = runtime.GOOS

// ActivateSystemPAC runs the macOS system PAC/CA-trust activation (or, on
// every other OS in this build, reports NotAttempted with no manual
// commands: those OSes' shapes are unmeasured). See sysmacos.go for the
// darwin sequence and sysunsupported.go for the rest.
func ActivateSystemPAC(ctx context.Context, run Runner, plan Plan) (Outcome, error) {
	if currentGOOS != "darwin" {
		return activateSystemPACUnsupported(), nil
	}
	return activateSystemPACDarwin(ctx, run, plan)
}

// DeactivateSystemPAC restores entry's scopes and untrusts its CA. It must
// run, and return successfully, before the caller deletes the CA files
// entry.CATrust names -- this package never deletes them itself, and
// ClearSystemEntry (the record-side half of a full removal) is a separate
// call so a caller doing a partial shrink instead of a full uninstall never
// has to touch it.
func DeactivateSystemPAC(ctx context.Context, run Runner, entry SystemEntry) (Report, error) {
	if currentGOOS != "darwin" {
		return deactivateSystemPACUnsupported(), nil
	}
	return deactivateSystemPACDarwin(ctx, run, entry)
}

// LiveSystemPAC reads every enabled network service's current auto-proxy
// state. Read-only: it never requests elevation and never calls Runner with
// anything other than `networksetup -listallnetworkservices` and
// `-getautoproxyurl`, so `doctor` can call it on every run.
func LiveSystemPAC(ctx context.Context, run Runner) ([]ScopeState, error) {
	if currentGOOS != "darwin" {
		return liveSystemPACUnsupported(), nil
	}
	return liveSystemPACDarwin(ctx, run)
}

// CATrustPresent reports whether a certificate carrying sha1 is currently
// present in the System keychain's trust store, independent of what any
// activation record claims. Read-only: it never requests elevation, and asks
// nothing on an OS whose shapes are unmeasured -- matching LiveSystemPAC, it
// is safe for `doctor` or a second `init` to call on every run.
func CATrustPresent(ctx context.Context, run Runner, sha1 string) (bool, error) {
	if currentGOOS != "darwin" {
		return false, nil
	}
	return catTrustPresentDarwin(ctx, run, sha1)
}

// LoadSystemEntry returns the machine's current system-scope activation, or
// nil if none exists yet.
func LoadSystemEntry(homeDir string) (*SystemEntry, error) {
	record, err := loadRecord(homeDir)
	if err != nil {
		return nil, err
	}
	return record.System, nil
}

// ClearSystemEntry forgets the system-scope activation after a full
// DeactivateSystemPAC has completed. Separate from Deactivate itself so a
// shrink-on-one-provider-uninstall (a fresh Activate with a smaller
// Providers, not a Deactivate at all) never touches it, and so a caller
// controls exactly when the record stops naming a CA file it is about to
// delete.
func ClearSystemEntry(homeDir string) error {
	record, err := loadRecord(homeDir)
	if err != nil {
		return err
	}
	if record.System == nil {
		return nil
	}
	record.System = nil
	return saveRecord(homeDir, record)
}

// persistSystemEntry writes entry as the record's System field, preserving
// every lane entry already there.
func persistSystemEntry(homeDir string, entry *SystemEntry) error {
	record, err := loadRecord(homeDir)
	if err != nil {
		return err
	}
	record.System = entry
	return saveRecord(homeDir, record)
}

// sha1Fingerprint is the uppercase, colon-free hex SHA-1 of pemBytes' DER
// content -- the same value/format `security find-certificate -Z` reports,
// so a read-back can compare them directly rather than trusting whatever the
// command line happened to be given.
func sha1Fingerprint(pemBytes []byte) (string, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return "", fmt.Errorf("activation: the CA certificate is not valid PEM")
	}
	sum := sha1.Sum(block.Bytes) //nolint:gosec // see the import-level note: identity match, not a security boundary
	return strings.ToUpper(hex.EncodeToString(sum[:])), nil
}

// SHA1Fingerprint is sha1Fingerprint, exported so a caller outside this
// package can compute a CA file's fingerprint the same way this package does
// and compare it against a recorded one, without duplicating the computation
// or trusting a caller-supplied hash.
func SHA1Fingerprint(pemBytes []byte) (string, error) {
	return sha1Fingerprint(pemBytes)
}

// validateArgvSafe rejects a string this package is about to place in an
// argv element or in the record if it contains a newline or NUL. Argv needs
// no shell quoting -- neither character can smuggle a second argument or
// truncate a C string the way it could through a shell -- but the record and
// the report that reads it back must stay parseable, and a newline inside a
// service or path name would let one entry masquerade as two lines.
func validateArgvSafe(s string) error {
	if strings.ContainsAny(s, "\n\x00") {
		return fmt.Errorf("activation: %q contains a newline or NUL; refusing to use it in a command", s)
	}
	return nil
}
