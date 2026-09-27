package activation

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/trace"
)

// -- test seams --------------------------------------------------------

// withSeams pins geteuidFn and openControllingTTY for the duration of one
// test, so every test is explicit about which authorization branch it
// exercises rather than depending on whatever uid or terminal the test
// runner happens to have.
func withSeams(t *testing.T, euid int, ttyOK bool) {
	t.Helper()
	oldEUID := geteuidFn
	geteuidFn = func() int { return euid }
	t.Cleanup(func() { geteuidFn = oldEUID })

	oldTTY := openControllingTTY
	if ttyOK {
		openControllingTTY = func() error { return nil }
	} else {
		openControllingTTY = func() error { return errors.New("no controlling terminal (test)") }
	}
	t.Cleanup(func() { openControllingTTY = oldTTY })
}

// -- fake Runner ---------------------------------------------------------

type recordedCall struct {
	name string
	args []string
}

func (c recordedCall) String() string { return c.name + " " + strings.Join(c.args, " ") }

// normalized strips a "sudo -n" wrapper, so an assertion can name the
// logical command (e.g. "security", "add-trusted-cert") without caring
// whether this call ran as root (no wrapper) or via sudo (wrapped).
func (c recordedCall) normalized() (name string, args []string) {
	if c.name == "sudo" && len(c.args) > 0 && c.args[0] == "-n" {
		return c.args[1], c.args[2:]
	}
	return c.name, c.args
}

func matchesPrefix(args []string, prefix ...string) bool {
	if len(args) < len(prefix) {
		return false
	}
	for i, p := range prefix {
		if args[i] != p {
			return false
		}
	}
	return true
}

// isCall reports whether c is (however it was wrapped for privilege) cmd
// followed by argsPrefix.
func isCall(c recordedCall, cmd string, argsPrefix ...string) bool {
	name, args := c.normalized()
	return name == cmd && matchesPrefix(args, argsPrefix...)
}

// recorder is the Runner under test: every call is appended to calls, then
// handed to handle. A nil handle (a test that expects zero calls) fails
// immediately, which is what proves NotAttempted never touches the system.
type recorder struct {
	t      *testing.T
	calls  []recordedCall
	handle func(name string, args []string) ([]byte, error)
}

func (r *recorder) run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, recordedCall{name: name, args: append([]string(nil), args...)})
	if r.handle == nil {
		if r.t != nil {
			r.t.Fatalf("recorder: no handler installed; a real privileged binary would have run: %s %v", name, args)
		}
		return nil, fmt.Errorf("recorder: no handler installed for %s %v", name, args)
	}
	return r.handle(name, args)
}

// fixture is a fake macOS system: enabled/disabled network services, each
// with a current URL/enabled state, and a System keychain that "trusts" the
// certificate whose SHA-1 is sha1 once add-trusted-cert has been asked to.
// It answers reads from its own mutated state, so applyScopeDarwin's and
// restoreScopeDarwin's read-backs see the effect of writes this same test
// made.
type fixture struct {
	t        *testing.T
	services []string
	disabled []string
	urls     map[string]string
	enabled  map[string]bool
	sha1     string
	// alsoPresent are further SHA-1s find-certificate reports, e.g. an older
	// CA a re-issue replaced but whose trust is still in the keychain.
	alsoPresent []string
}

func newFixture(t *testing.T, services, disabled []string, sha1 string) *fixture {
	return &fixture{
		t: t, services: services, disabled: disabled,
		urls: map[string]string{}, enabled: map[string]bool{}, sha1: sha1,
	}
}

func (f *fixture) listing() []byte {
	var b strings.Builder
	b.WriteString("An asterisk (*) denotes that a network service is disabled.\n")
	for _, s := range f.services {
		b.WriteString(s + "\n")
	}
	for _, s := range f.disabled {
		b.WriteString("*" + s + "\n")
	}
	return []byte(b.String())
}

func (f *fixture) getAutoProxyOutput(service string) []byte {
	line := "URL: (null)"
	if url, ok := f.urls[service]; ok && url != "" {
		line = "URL: " + url
	}
	enabledLine := "Enabled: No"
	if f.enabled[service] {
		enabledLine = "Enabled: Yes"
	}
	return []byte(line + "\n" + enabledLine + "\n")
}

// handler is the fixture's default Runner behavior: the sudo-once dance
// always succeeds (a cached credential), reads answer from fixture state,
// and writes mutate it. A test overrides specific calls by wrapping this.
func (f *fixture) handler() func(name string, args []string) ([]byte, error) {
	return func(name string, args []string) ([]byte, error) {
		switch {
		case name == "sudo" && matchesPrefix(args, "-n", "-v"):
			return nil, nil
		case name == "sudo" && len(args) == 1 && args[0] == "-v":
			return nil, nil
		case name == "sudo" && matchesPrefix(args, "-n", "true"):
			return nil, nil
		}

		cmd, cmdArgs := (recordedCall{name: name, args: args}).normalized()
		switch {
		case cmd == "networksetup" && matchesPrefix(cmdArgs, "-listallnetworkservices"):
			return f.listing(), nil
		case cmd == "networksetup" && matchesPrefix(cmdArgs, "-getautoproxyurl"):
			return f.getAutoProxyOutput(cmdArgs[1]), nil
		case cmd == "networksetup" && matchesPrefix(cmdArgs, "-setautoproxyurl"):
			f.urls[cmdArgs[1]] = cmdArgs[2]
			return nil, nil
		case cmd == "networksetup" && matchesPrefix(cmdArgs, "-setautoproxystate"):
			f.enabled[cmdArgs[1]] = cmdArgs[2] == "on"
			return nil, nil
		case cmd == "security" && matchesPrefix(cmdArgs, "add-trusted-cert"):
			return nil, nil
		case cmd == "security" && matchesPrefix(cmdArgs, "find-certificate"):
			var b strings.Builder
			for _, h := range append([]string{f.sha1}, f.alsoPresent...) {
				if h != "" {
					b.WriteString("SHA-1 hash: " + h + "\nkeychain: \"" + systemKeychainPath + "\"\n")
				}
			}
			return []byte(b.String()), nil
		case cmd == "security" && matchesPrefix(cmdArgs, "remove-trusted-cert"):
			return nil, nil
		case cmd == "security" && matchesPrefix(cmdArgs, "delete-certificate"):
			return nil, nil
		}
		f.t.Fatalf("fixture: unhandled call %s %v", name, args)
		return nil, fmt.Errorf("unreachable")
	}
}

// withFailure wraps base so any call matching cmd+argsPrefix (however it was
// privilege-wrapped) fails with err instead.
func withFailure(base func(name string, args []string) ([]byte, error), cmd string, argsPrefix []string, err error) func(string, []string) ([]byte, error) {
	return func(name string, args []string) ([]byte, error) {
		if isCall(recordedCall{name: name, args: args}, cmd, argsPrefix...) {
			return nil, err
		}
		return base(name, args)
	}
}

// -- test fixtures for the CA --------------------------------------------

func testCAPEM(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatalf("generate serial: %v", err)
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: caCommonName},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(time.Hour),
		IsCA:         true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	return pemEncodeCert(der)
}

func testPlan(t *testing.T, home string) Plan {
	t.Helper()
	pem := testCAPEM(t)
	return Plan{
		HomeDir:   home,
		PACURL:    "http://127.0.0.1:8790/proxy.pac",
		CAPath:    filepath.Join(home, "transport-ca.pem"),
		CAPEM:     pem,
		Providers: []string{"claude-code"},
	}
}

// -- parser tests ----------------------------------------------------------

func TestParseListAllNetworkServices(t *testing.T) {
	raw := "An asterisk (*) denotes that a network service is disabled.\n" +
		"Wi-Fi\nLG Monitor Controls 2\n*iPhone USB\nAX88179A\nThunderbolt Bridge\n"
	got, err := parseListAllNetworkServices([]byte(raw))
	if err != nil {
		t.Fatalf("parseListAllNetworkServices: %v", err)
	}
	want := []string{"Wi-Fi", "LG Monitor Controls 2", "AX88179A", "Thunderbolt Bridge"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestParseGetAutoProxyURL(t *testing.T) {
	t.Run("a set and enabled url", func(t *testing.T) {
		got := parseGetAutoProxyURL("Wi-Fi", []byte("URL: http://127.0.0.1:8790/proxy.pac\nEnabled: Yes\n"))
		want := ScopeState{Service: "Wi-Fi", URLPresent: true, URL: "http://127.0.0.1:8790/proxy.pac", Enabled: true}
		if got != want {
			t.Errorf("got %+v, want %+v", got, want)
		}
	})
	t.Run("a null, disabled url", func(t *testing.T) {
		got := parseGetAutoProxyURL("Ethernet", []byte("URL: (null)\nEnabled: No\n"))
		want := ScopeState{Service: "Ethernet", URLPresent: false, URL: "", Enabled: false}
		if got != want {
			t.Errorf("got %+v, want %+v", got, want)
		}
	})
}

func TestValidateArgvSafeRejectsNewlineAndNUL(t *testing.T) {
	for _, s := range []string{"foo\nbar", "foo\x00bar", "Wi-Fi\nsecurity add-trusted-cert"} {
		if err := validateArgvSafe(s); err == nil {
			t.Errorf("validateArgvSafe(%q) = nil, want an error", s)
		}
	}
	if err := validateArgvSafe("Wi-Fi"); err != nil {
		t.Errorf("validateArgvSafe(%q) = %v, want nil", "Wi-Fi", err)
	}
}

func TestSHA1FingerprintMatchesFindCertificateTextForm(t *testing.T) {
	caPEM := testCAPEM(t)
	got, err := sha1Fingerprint(caPEM)
	if err != nil {
		t.Fatalf("sha1Fingerprint: %v", err)
	}
	if len(got) != 40 {
		t.Fatalf("fingerprint %q is not 40 hex characters", got)
	}
	text := []byte("SHA-1 hash: " + got + "\nkeychain: \"/Library/Keychains/System.keychain\"\n")
	if !findCertificateHasSHA1(text, got) {
		t.Error("findCertificateHasSHA1 did not recognise its own fingerprint's find-certificate text form")
	}
	if findCertificateHasSHA1(text, "0000000000000000000000000000000000000000") {
		t.Error("findCertificateHasSHA1 matched a fingerprint that was not in the text")
	}
}

// -- Activate: the four outcome classes -------------------------------------

func TestActivateFourOutcomeClasses(t *testing.T) {
	t.Run("not attempted: no controlling terminal, zero Runner calls", func(t *testing.T) {
		withSeams(t, 501, false)
		home := t.TempDir()
		rec := &recorder{t: t}
		outcome, err := ActivateSystemPAC(context.Background(), rec.run, testPlan(t, home))
		if err != nil {
			t.Fatalf("ActivateSystemPAC: %v", err)
		}
		if outcome.Class != NotAttempted {
			t.Errorf("Class = %v, want NotAttempted", outcome.Class)
		}
		if len(rec.calls) != 0 {
			t.Errorf("NotAttempted made %d Runner call(s), want 0: %v", len(rec.calls), rec.calls)
		}
	})

	t.Run("declined: the single sudo -v prompt is refused", func(t *testing.T) {
		withSeams(t, 501, true)
		home := t.TempDir()
		rec := &recorder{t: t}
		rec.handle = func(name string, args []string) ([]byte, error) {
			if name == "sudo" {
				return nil, errors.New("0:61: execution error: User canceled. (-128)")
			}
			t.Fatalf("unexpected call after a decline: %s %v", name, args)
			return nil, nil
		}
		outcome, err := ActivateSystemPAC(context.Background(), rec.run, testPlan(t, home))
		if err != nil {
			t.Fatalf("ActivateSystemPAC: %v", err)
		}
		if outcome.Class != Declined {
			t.Errorf("Class = %v, want Declined", outcome.Class)
		}
		if len(outcome.Manual) == 0 {
			t.Error("Declined carries no manual commands")
		}
		entry, err := LoadSystemEntry(home)
		if err != nil {
			t.Fatalf("LoadSystemEntry: %v", err)
		}
		if entry == nil || !entry.Declined {
			t.Errorf("record does not say Declined: %+v", entry)
		}
	})

	t.Run("failed: sudoers does not cache credentials", func(t *testing.T) {
		withSeams(t, 501, true)
		home := t.TempDir()
		rec := &recorder{t: t}
		rec.handle = func(name string, args []string) ([]byte, error) {
			switch {
			case name == "sudo" && matchesPrefix(args, "-n", "-v"):
				return nil, errors.New("not cached")
			case name == "sudo" && len(args) == 1 && args[0] == "-v":
				return nil, nil
			case name == "sudo" && matchesPrefix(args, "-n", "true"):
				return nil, errors.New("still not cached")
			}
			t.Fatalf("unexpected call: %s %v", name, args)
			return nil, nil
		}
		outcome, err := ActivateSystemPAC(context.Background(), rec.run, testPlan(t, home))
		if err != nil {
			t.Fatalf("ActivateSystemPAC: %v", err)
		}
		if outcome.Class != Failed {
			t.Errorf("Class = %v, want Failed", outcome.Class)
		}
		if outcome.Reason == "" {
			t.Error("Failed carries no reason")
		}
	})

	t.Run("activated: the full happy path", func(t *testing.T) {
		withSeams(t, 501, true)
		home := t.TempDir()
		plan := testPlan(t, home)
		sha1, err := sha1Fingerprint(plan.CAPEM)
		if err != nil {
			t.Fatalf("sha1Fingerprint: %v", err)
		}
		f := newFixture(t, []string{"Wi-Fi"}, nil, sha1)
		f.enabled["Wi-Fi"] = false
		rec := &recorder{t: t, handle: f.handler()}

		outcome, err := ActivateSystemPAC(context.Background(), rec.run, plan)
		if err != nil {
			t.Fatalf("ActivateSystemPAC: %v", err)
		}
		if outcome.Class != Activated {
			t.Fatalf("Class = %v, reason=%q", outcome.Class, outcome.Reason)
		}
		if outcome.Entry == nil || !outcome.Entry.PACActivated || outcome.Entry.Pending {
			t.Errorf("entry not finalized: %+v", outcome.Entry)
		}
		if f.urls["Wi-Fi"] != plan.PACURL || !f.enabled["Wi-Fi"] {
			t.Errorf("Wi-Fi was not actually set to the PAC URL: url=%s enabled=%v", f.urls["Wi-Fi"], f.enabled["Wi-Fi"])
		}
	})
}

// TestActivateAsRootSkipsSudoButFailsWithoutAGUISession is the measured
// shape: a root shell cannot change System-keychain trust with no GUI
// session, so this classifies Failed and never reaches networksetup.
func TestActivateAsRootSkipsSudoButFailsWithoutAGUISession(t *testing.T) {
	withSeams(t, 0, true) // euid 0: authorizeDarwin must not even check the tty
	home := t.TempDir()
	plan := testPlan(t, home)
	sha1, err := sha1Fingerprint(plan.CAPEM)
	if err != nil {
		t.Fatalf("sha1Fingerprint: %v", err)
	}
	f := newFixture(t, []string{"Wi-Fi"}, nil, sha1)
	base := f.handler()
	rec := &recorder{t: t}
	rec.handle = withFailure(base, "security", []string{"add-trusted-cert"},
		errors.New("SecTrustSettingsSetTrustSettings: The authorization was denied since no user interaction was possible."))

	outcome, err := ActivateSystemPAC(context.Background(), rec.run, plan)
	if err != nil {
		t.Fatalf("ActivateSystemPAC: %v", err)
	}
	if outcome.Class != Failed {
		t.Errorf("Class = %v, want Failed", outcome.Class)
	}
	if outcome.Entry != nil && outcome.Entry.PACActivated {
		t.Error("PACActivated is true despite the trust failure")
	}
	for _, c := range rec.calls {
		if c.name == "sudo" {
			t.Errorf("euid 0 invoked sudo: %v", c)
		}
		if isCall(c, "networksetup", "-setautoproxyurl") {
			t.Errorf("the PAC was written despite the trust failure: %v", c)
		}
	}
}

// -- record-before-write --------------------------------------------------

func TestActivatePersistsPendingBeforeTheFirstPrivilegedWrite(t *testing.T) {
	withSeams(t, 501, true)
	home := t.TempDir()
	plan := testPlan(t, home)
	sha1, err := sha1Fingerprint(plan.CAPEM)
	if err != nil {
		t.Fatalf("sha1Fingerprint: %v", err)
	}
	f := newFixture(t, []string{"Wi-Fi"}, nil, sha1)
	f.urls["Wi-Fi"] = ""
	f.enabled["Wi-Fi"] = false
	base := f.handler()

	var checked bool
	rec := &recorder{t: t}
	rec.handle = func(name string, args []string) ([]byte, error) {
		if !checked && isCall(recordedCall{name: name, args: args}, "security", "add-trusted-cert") {
			checked = true
			entry, err := LoadSystemEntry(home)
			if err != nil {
				t.Fatalf("LoadSystemEntry (from inside the Runner): %v", err)
			}
			if entry == nil || !entry.Pending {
				t.Fatalf("record is not Pending at the moment of the first privileged write: %+v", entry)
			}
			if len(entry.Scopes) != 1 || entry.Scopes[0].Service != "Wi-Fi" || entry.Scopes[0].PriorURLPresent {
				t.Fatalf("prior scopes were not recorded before the first privileged write: %+v", entry.Scopes)
			}
		}
		return base(name, args)
	}

	outcome, err := ActivateSystemPAC(context.Background(), rec.run, plan)
	if err != nil {
		t.Fatalf("ActivateSystemPAC: %v", err)
	}
	if !checked {
		t.Fatal("add-trusted-cert was never called; the assertion above never ran")
	}
	if outcome.Class != Activated {
		t.Fatalf("Class = %v, reason=%q", outcome.Class, outcome.Reason)
	}
}

// TestActivateTrustFailureYieldsZeroNetworksetupArgv is the ordering
// guarantee: trust happens before any scope is touched, so a failure there
// must produce no networksetup call at all.
func TestActivateTrustFailureYieldsZeroNetworksetupArgv(t *testing.T) {
	withSeams(t, 501, true)
	home := t.TempDir()
	plan := testPlan(t, home)
	sha1, err := sha1Fingerprint(plan.CAPEM)
	if err != nil {
		t.Fatalf("sha1Fingerprint: %v", err)
	}
	f := newFixture(t, []string{"Wi-Fi", "Ethernet"}, nil, sha1)
	base := f.handler()
	rec := &recorder{t: t}
	rec.handle = withFailure(base, "security", []string{"add-trusted-cert"}, errors.New("boom"))

	outcome, err := ActivateSystemPAC(context.Background(), rec.run, plan)
	if err != nil {
		t.Fatalf("ActivateSystemPAC: %v", err)
	}
	if outcome.Class != Failed {
		t.Fatalf("Class = %v, want Failed", outcome.Class)
	}

	writeCommands := [][]string{
		{"security", "add-trusted-cert"}, {"security", "remove-trusted-cert"}, {"security", "delete-certificate"},
		{"networksetup", "-setautoproxyurl"}, {"networksetup", "-setautoproxystate"},
	}
	var firstWrite *recordedCall
	for i := range rec.calls {
		for _, w := range writeCommands {
			if isCall(rec.calls[i], w[0], w[1]) {
				firstWrite = &rec.calls[i]
			}
		}
		if firstWrite != nil {
			break
		}
	}
	if firstWrite == nil {
		t.Fatal("no privileged write call was recorded at all")
	}
	if !isCall(*firstWrite, "security", "add-trusted-cert") {
		t.Errorf("first privileged write was %v, want add-trusted-cert", firstWrite)
	}
	for _, c := range rec.calls {
		if isCall(c, "networksetup", "-setautoproxyurl") || isCall(c, "networksetup", "-setautoproxystate") {
			t.Errorf("networksetup was called after a trust failure: %v", c)
		}
	}
}

func TestActivateFailsWhenTrustReadBackSHA1Mismatches(t *testing.T) {
	withSeams(t, 501, true)
	home := t.TempDir()
	plan := testPlan(t, home)
	f := newFixture(t, []string{"Wi-Fi"}, nil, "0000000000000000000000000000000000000000")
	rec := &recorder{t: t, handle: f.handler()}

	outcome, err := ActivateSystemPAC(context.Background(), rec.run, plan)
	if err != nil {
		t.Fatalf("ActivateSystemPAC: %v", err)
	}
	if outcome.Class != Failed {
		t.Fatalf("Class = %v, want Failed", outcome.Class)
	}
	var untrusted bool
	for _, c := range rec.calls {
		if isCall(c, "networksetup", "-setautoproxyurl") {
			t.Errorf("the PAC was written despite a trust mismatch: %v", c)
		}
		if isCall(c, "security", "remove-trusted-cert") {
			untrusted = true
		}
	}
	if !untrusted {
		t.Error("a mismatched trust was not rolled back")
	}
}

// TestActivateFailureAtScopeRollsBackInReverseAndUntrusts is the same
// ordering guarantee across multiple scopes: the ones already applied must be restored in reverse
// order, and the CA untrusted, before returning Failed.
func TestActivateFailureAtScopeRollsBackInReverseAndUntrusts(t *testing.T) {
	withSeams(t, 501, true)
	home := t.TempDir()
	plan := testPlan(t, home)
	sha1, err := sha1Fingerprint(plan.CAPEM)
	if err != nil {
		t.Fatalf("sha1Fingerprint: %v", err)
	}
	f := newFixture(t, []string{"A", "B", "C"}, nil, sha1)
	f.urls["A"], f.enabled["A"] = "http://old-a/proxy.pac", true
	f.urls["B"], f.enabled["B"] = "", false
	f.urls["C"], f.enabled["C"] = "http://old-c/proxy.pac", true
	base := f.handler()

	var order []string
	rec := &recorder{t: t}
	rec.handle = func(name string, args []string) ([]byte, error) {
		c := recordedCall{name: name, args: args}
		if isCall(c, "networksetup", "-setautoproxyurl", "C") {
			return nil, errors.New("boom at C")
		}
		if isCall(c, "networksetup") || isCall(c, "security") {
			order = append(order, c.String())
		}
		return base(name, args)
	}

	outcome, err := ActivateSystemPAC(context.Background(), rec.run, plan)
	if err != nil {
		t.Fatalf("ActivateSystemPAC: %v", err)
	}
	if outcome.Class != Failed {
		t.Fatalf("Class = %v, want Failed", outcome.Class)
	}
	if outcome.Entry == nil || outcome.Entry.PACActivated {
		t.Errorf("PACActivated should be false: %+v", outcome.Entry)
	}
	if f.urls["A"] != "http://old-a/proxy.pac" || !f.enabled["A"] {
		t.Errorf("A was not restored: url=%s enabled=%v", f.urls["A"], f.enabled["A"])
	}
	if f.enabled["B"] {
		t.Error("B (a null/disabled prior) was not turned back off")
	}
	var untrusted bool
	for _, c := range rec.calls {
		if isCall(c, "security", "remove-trusted-cert") {
			untrusted = true
		}
	}
	if !untrusted {
		t.Error("trust was not removed after the scope failure")
	}

	// B's restore (reverse order: B applied after A, so B rolls back first)
	// must precede A's.
	bIdx, aIdx := -1, -1
	for i, line := range order {
		if strings.Contains(line, "-setautoproxystate B off") && bIdx == -1 {
			bIdx = i
		}
		if strings.Contains(line, "-setautoproxyurl A http://old-a/proxy.pac") && aIdx == -1 {
			aIdx = i
		}
	}
	if bIdx == -1 || aIdx == -1 {
		t.Fatalf("could not find both restore calls in order: %v", order)
	}
	if bIdx > aIdx {
		t.Errorf("B was not restored before A (reverse-of-applied order): %v", order)
	}
}

// TestActivateReconcilesAPendingRunThatAlreadyWroteOurURL is the crash-and-
// resume case: a previous run's Pending record already shows our PAC URL
// live, and its own recorded prior (a real corporate PAC) must survive into
// this run's record rather than being overwritten with our own URL.
func TestActivateReconcilesAPendingRunThatAlreadyWroteOurURL(t *testing.T) {
	withSeams(t, 501, true)
	home := t.TempDir()
	plan := testPlan(t, home)
	sha1, err := sha1Fingerprint(plan.CAPEM)
	if err != nil {
		t.Fatalf("sha1Fingerprint: %v", err)
	}

	priorRun := &SystemEntry{
		Schema:  systemEntrySchema,
		Pending: true,
		PACURL:  plan.PACURL,
		CATrust: &CATrustState{SHA1: sha1, CAPath: plan.CAPath, Keychain: systemKeychainPath},
		Scopes: []ProxyScope{
			{Service: "Wi-Fi", PriorURLPresent: true, PriorURL: "http://corp.example/proxy.pac", PriorEnabled: true},
		},
	}
	if err := persistSystemEntry(home, priorRun); err != nil {
		t.Fatalf("persistSystemEntry: %v", err)
	}

	f := newFixture(t, []string{"Wi-Fi"}, nil, sha1)
	f.urls["Wi-Fi"], f.enabled["Wi-Fi"] = plan.PACURL, true // the crashed run's own write, still live
	rec := &recorder{t: t, handle: f.handler()}

	outcome, err := ActivateSystemPAC(context.Background(), rec.run, plan)
	if err != nil {
		t.Fatalf("ActivateSystemPAC: %v", err)
	}
	if outcome.Class != Activated {
		t.Fatalf("Class = %v, reason=%q", outcome.Class, outcome.Reason)
	}
	if len(outcome.Entry.Scopes) != 1 {
		t.Fatalf("scopes = %+v", outcome.Entry.Scopes)
	}
	got := outcome.Entry.Scopes[0]
	if !got.PriorURLPresent || got.PriorURL != "http://corp.example/proxy.pac" || !got.PriorEnabled {
		t.Errorf("reconcile lost the true prior; got %+v, want the corp URL from the earlier crashed run", got)
	}
}

// -- CA re-issue vs. an already-Activated record -----------------------

// TestActivateRemovesTheOldTrustBeforeTrustingAReissuedCA: a record naming an
// older CA's SHA-1 (whose file still exists) must have that trust dropped,
// under the SAME authorization, before the new CA is trusted -- otherwise the
// old, superseded root is left in the System keychain forever, invisible to
// LoadSystemEntry once its own file is gone.
func TestActivateRemovesTheOldTrustBeforeTrustingAReissuedCA(t *testing.T) {
	withSeams(t, 501, true)
	home := t.TempDir()
	plan := testPlan(t, home)
	newSHA1, err := sha1Fingerprint(plan.CAPEM)
	if err != nil {
		t.Fatalf("sha1Fingerprint: %v", err)
	}

	oldCAPath := filepath.Join(home, "old-transport-ca.pem")
	oldPEM := testCAPEM(t)
	if err := os.WriteFile(oldCAPath, oldPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	oldSHA1, err := sha1Fingerprint(oldPEM)
	if err != nil {
		t.Fatal(err)
	}
	existing := &SystemEntry{
		Schema:       systemEntrySchema,
		PACActivated: true,
		PACURL:       plan.PACURL,
		Scopes:       []ProxyScope{{Service: "Wi-Fi", PriorURLPresent: false}},
		CATrust:      &CATrustState{SHA1: oldSHA1, CAPath: oldCAPath, Keychain: systemKeychainPath},
	}
	if err := persistSystemEntry(home, existing); err != nil {
		t.Fatalf("seed: %v", err)
	}

	f := newFixture(t, []string{"Wi-Fi"}, nil, newSHA1)
	f.alsoPresent = []string{oldSHA1}
	rec := &recorder{t: t, handle: f.handler()}

	outcome, err := ActivateSystemPAC(context.Background(), rec.run, plan)
	if err != nil {
		t.Fatalf("ActivateSystemPAC: %v", err)
	}
	if outcome.Class != Activated {
		t.Fatalf("Class = %v, reason=%q", outcome.Class, outcome.Reason)
	}
	if outcome.Entry == nil || outcome.Entry.CATrust == nil || outcome.Entry.CATrust.SHA1 != newSHA1 {
		t.Fatalf("the record does not end with the new SHA-1: %+v", outcome.Entry)
	}

	removeIdx, deleteOldIdx, addIdx := -1, -1, -1
	for i, c := range rec.calls {
		switch {
		case isCall(c, "security", "remove-trusted-cert", "-d", oldCAPath):
			removeIdx = i
		case isCall(c, "security", "delete-certificate", "-Z", oldSHA1):
			deleteOldIdx = i
		case isCall(c, "security", "add-trusted-cert"):
			if addIdx == -1 {
				addIdx = i
			}
		}
	}
	if removeIdx == -1 {
		t.Error("the old CA's file was never removed via remove-trusted-cert -d")
	}
	if deleteOldIdx == -1 {
		t.Error("the old CA's SHA-1 was never deleted via delete-certificate -Z")
	}
	if addIdx == -1 {
		t.Fatal("the new CA was never trusted")
	}
	if !(removeIdx < deleteOldIdx && deleteOldIdx < addIdx) {
		t.Errorf("wrong order: remove=%d delete=%d add=%d, want remove < delete < add", removeIdx, deleteOldIdx, addIdx)
	}

	sudoVCount := 0
	for _, c := range rec.calls {
		if c.name == "sudo" && len(c.args) == 1 && c.args[0] == "-v" {
			sudoVCount++
		}
	}
	if sudoVCount > 1 {
		t.Errorf("more than one interactive sudo prompt for one activation run: %d", sudoVCount)
	}
}

// TestActivateSkipsRemoveTrustedCertForOldCAWhenItsFileIsGone: a legacy CA
// re-issue deletes the old cert/key files before this ever runs, so
// remove-trusted-cert (which needs that file) must be skipped, and
// delete-certificate (which matches by SHA-1 alone) must still run.
func TestActivateSkipsRemoveTrustedCertForOldCAWhenItsFileIsGone(t *testing.T) {
	withSeams(t, 501, true)
	home := t.TempDir()
	plan := testPlan(t, home)
	newSHA1, err := sha1Fingerprint(plan.CAPEM)
	if err != nil {
		t.Fatalf("sha1Fingerprint: %v", err)
	}
	const oldSHA1 = "1111111111111111111111111111111111111111"
	oldCAPath := filepath.Join(home, "already-deleted-by-reissue.pem") // never written
	existing := &SystemEntry{
		Schema:       systemEntrySchema,
		PACActivated: true,
		PACURL:       plan.PACURL,
		Scopes:       []ProxyScope{{Service: "Wi-Fi", PriorURLPresent: false}},
		CATrust:      &CATrustState{SHA1: oldSHA1, CAPath: oldCAPath, Keychain: systemKeychainPath},
	}
	if err := persistSystemEntry(home, existing); err != nil {
		t.Fatalf("seed: %v", err)
	}

	f := newFixture(t, []string{"Wi-Fi"}, nil, newSHA1)
	f.alsoPresent = []string{oldSHA1}
	rec := &recorder{t: t, handle: f.handler()}

	outcome, err := ActivateSystemPAC(context.Background(), rec.run, plan)
	if err != nil {
		t.Fatalf("ActivateSystemPAC: %v", err)
	}
	if outcome.Class != Activated {
		t.Fatalf("Class = %v, reason=%q", outcome.Class, outcome.Reason)
	}
	for _, c := range rec.calls {
		if isCall(c, "security", "remove-trusted-cert", "-d", oldCAPath) {
			t.Errorf("remove-trusted-cert was called for a CA file a re-issue already deleted: %v", c)
		}
	}
	var deletedOld bool
	for _, c := range rec.calls {
		if isCall(c, "security", "delete-certificate", "-Z", oldSHA1) {
			deletedOld = true
		}
	}
	if !deletedOld {
		t.Error("the superseded CA's SHA-1 was never deleted")
	}
}

// TestActivateDoesNotTouchOldTrustWhenSHA1Matches is the regression control:
// an ordinary re-run (no re-issue in between) must never delete any
// certificate at all.
func TestActivateDoesNotTouchOldTrustWhenSHA1Matches(t *testing.T) {
	withSeams(t, 501, true)
	home := t.TempDir()
	plan := testPlan(t, home)
	sha1, err := sha1Fingerprint(plan.CAPEM)
	if err != nil {
		t.Fatalf("sha1Fingerprint: %v", err)
	}
	existing := &SystemEntry{
		Schema:       systemEntrySchema,
		PACActivated: true,
		PACURL:       plan.PACURL,
		Scopes:       []ProxyScope{{Service: "Wi-Fi", PriorURLPresent: false}},
		CATrust:      &CATrustState{SHA1: sha1, CAPath: plan.CAPath, Keychain: systemKeychainPath},
	}
	if err := persistSystemEntry(home, existing); err != nil {
		t.Fatalf("seed: %v", err)
	}
	f := newFixture(t, []string{"Wi-Fi"}, nil, sha1)
	rec := &recorder{t: t, handle: f.handler()}

	outcome, err := ActivateSystemPAC(context.Background(), rec.run, plan)
	if err != nil {
		t.Fatalf("ActivateSystemPAC: %v", err)
	}
	if outcome.Class != Activated {
		t.Fatalf("Class = %v, reason=%q", outcome.Class, outcome.Reason)
	}
	for _, c := range rec.calls {
		if isCall(c, "security", "delete-certificate") {
			t.Errorf("an ordinary re-run with a matching SHA-1 must never delete any certificate: %v", c)
		}
	}
}

// TestFailedPastPreflightCarriesManualCommands: a Failed outcome after the
// preflight stage (here, the trust write itself) must still hand the
// developer the commands to finish activation by hand, matching the
// Declined/preflight-Failed paths.
func TestFailedPastPreflightCarriesManualCommands(t *testing.T) {
	withSeams(t, 501, true)
	home := t.TempDir()
	plan := testPlan(t, home)
	f := newFixture(t, []string{"Wi-Fi"}, nil, "irrelevant-because-the-trust-write-itself-fails")
	base := f.handler()
	rec := &recorder{t: t}
	rec.handle = withFailure(base, "security", []string{"add-trusted-cert"}, errors.New("boom"))

	outcome, err := ActivateSystemPAC(context.Background(), rec.run, plan)
	if err != nil {
		t.Fatalf("ActivateSystemPAC: %v", err)
	}
	if outcome.Class != Failed {
		t.Fatalf("Class = %v, want Failed", outcome.Class)
	}
	if len(outcome.Manual) == 0 {
		t.Fatal("a Failed outcome past preflight carries no manual recovery commands")
	}
	var found bool
	for _, cmd := range outcome.Manual {
		if strings.Contains(cmd, "add-trusted-cert") {
			found = true
		}
	}
	if !found {
		t.Errorf("Manual does not include the trust command: %v", outcome.Manual)
	}
}

// -- Deactivate --------------------------------------------------------

func TestDeactivateRestoresNullPriorViaStateOffNeverEmptyURL(t *testing.T) {
	withSeams(t, 501, true)
	entry := SystemEntry{
		PACURL: "http://127.0.0.1:8790/proxy.pac",
		Scopes: []ProxyScope{{Service: "Wi-Fi", PriorURLPresent: false, PriorEnabled: false}},
	}
	f := newFixture(t, []string{"Wi-Fi"}, nil, "")
	f.urls["Wi-Fi"], f.enabled["Wi-Fi"] = entry.PACURL, true
	rec := &recorder{t: t, handle: f.handler()}

	report, err := DeactivateSystemPAC(context.Background(), rec.run, entry)
	if err != nil {
		t.Fatalf("DeactivateSystemPAC: %v", err)
	}
	if len(report.Restored) != 1 || report.Restored[0] != "Wi-Fi" {
		t.Errorf("Restored = %v", report.Restored)
	}
	for _, c := range rec.calls {
		if isCall(c, "networksetup", "-setautoproxyurl") {
			t.Errorf("restore called -setautoproxyurl for a null prior: %v", c)
		}
	}
	if f.enabled["Wi-Fi"] {
		t.Error("Wi-Fi was not turned off")
	}
}

func TestDeactivateLeavesADriftedScopeUntouched(t *testing.T) {
	withSeams(t, 501, true)
	entry := SystemEntry{
		PACURL: "http://127.0.0.1:8790/proxy.pac",
		Scopes: []ProxyScope{
			{Service: "Wi-Fi", PriorURLPresent: true, PriorURL: "http://corp.example/proxy.pac", PriorEnabled: true},
			{Service: "Ethernet", PriorURLPresent: false, PriorEnabled: false},
		},
	}
	f := newFixture(t, []string{"Wi-Fi", "Ethernet"}, nil, "")
	f.urls["Wi-Fi"], f.enabled["Wi-Fi"] = entry.PACURL, true
	f.urls["Ethernet"], f.enabled["Ethernet"] = "http://someone-else-changed-it/proxy.pac", true
	rec := &recorder{t: t, handle: f.handler()}

	report, err := DeactivateSystemPAC(context.Background(), rec.run, entry)
	if err != nil {
		t.Fatalf("DeactivateSystemPAC: %v", err)
	}
	if len(report.Drift) != 1 || report.Drift[0] != "Ethernet" {
		t.Errorf("Drift = %v", report.Drift)
	}
	if len(report.Restored) != 1 || report.Restored[0] != "Wi-Fi" {
		t.Errorf("Restored = %v", report.Restored)
	}
	if f.urls["Ethernet"] != "http://someone-else-changed-it/proxy.pac" {
		t.Errorf("the drifted scope was modified: %s", f.urls["Ethernet"])
	}
	for _, c := range rec.calls {
		if isCall(c, "networksetup", "-setautoproxyurl", "Ethernet") || isCall(c, "networksetup", "-setautoproxystate", "Ethernet") {
			t.Errorf("a write touched the drifted scope: %v", c)
		}
	}
}

func TestDeactivateOrderIsNetworksetupThenUntrust(t *testing.T) {
	withSeams(t, 501, true)
	caPath := filepath.Join(t.TempDir(), "ca.pem")
	caPEM := testCAPEM(t)
	if err := os.WriteFile(caPath, caPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	caSHA1, err := sha1Fingerprint(caPEM)
	if err != nil {
		t.Fatal(err)
	}
	entry := SystemEntry{
		PACURL:  "http://127.0.0.1:8790/proxy.pac",
		Scopes:  []ProxyScope{{Service: "Wi-Fi", PriorURLPresent: false, PriorEnabled: false}},
		CATrust: &CATrustState{SHA1: caSHA1, CAPath: caPath, Keychain: systemKeychainPath},
	}
	f := newFixture(t, []string{"Wi-Fi"}, nil, entry.CATrust.SHA1)
	f.urls["Wi-Fi"], f.enabled["Wi-Fi"] = entry.PACURL, true
	rec := &recorder{t: t, handle: f.handler()}

	if _, err := DeactivateSystemPAC(context.Background(), rec.run, entry); err != nil {
		t.Fatalf("DeactivateSystemPAC: %v", err)
	}

	var order []string
	for _, c := range rec.calls {
		switch {
		case isCall(c, "networksetup", "-setautoproxystate", "Wi-Fi", "off"):
			order = append(order, "networksetup")
		case isCall(c, "security", "remove-trusted-cert"):
			order = append(order, "remove-trusted-cert")
		case isCall(c, "security", "delete-certificate"):
			order = append(order, "delete-certificate")
		}
	}
	want := []string{"networksetup", "remove-trusted-cert", "delete-certificate"}
	if !reflect.DeepEqual(order, want) {
		t.Errorf("order = %v, want %v", order, want)
	}
}

func TestDeactivateDeclinedReportsManualCommands(t *testing.T) {
	withSeams(t, 501, true)
	entry := SystemEntry{
		PACURL:  "http://127.0.0.1:8790/proxy.pac",
		Scopes:  []ProxyScope{{Service: "Wi-Fi", PriorURLPresent: true, PriorURL: "http://corp.example/proxy.pac", PriorEnabled: true}},
		CATrust: &CATrustState{SHA1: "ABCDEF0123456789ABCDEF0123456789ABCDEF01", CAPath: "/tmp/ca.pem"},
	}
	rec := &recorder{t: t}
	rec.handle = func(name string, args []string) ([]byte, error) {
		if name == "sudo" {
			return nil, errors.New("declined")
		}
		t.Fatalf("unexpected call after a decline: %s %v", name, args)
		return nil, nil
	}
	report, err := DeactivateSystemPAC(context.Background(), rec.run, entry)
	if err != nil {
		t.Fatalf("DeactivateSystemPAC: %v", err)
	}
	if report.Class != Declined {
		t.Errorf("Class = %v, want Declined", report.Class)
	}
	if len(report.Manual) == 0 {
		t.Error("a declined deactivate carries no manual commands")
	}
}

// TestDeactivateSkipsRemoveTrustedCertWhenTheCAFileIsGone: a CA re-issue (or
// a prior uninstall attempt) can leave the record naming a file that no
// longer exists; remove-trusted-cert needs that file, so it must be skipped,
// while delete-certificate (SHA-1 alone) must still run.
func TestDeactivateSkipsRemoveTrustedCertWhenTheCAFileIsGone(t *testing.T) {
	withSeams(t, 501, true)
	entry := SystemEntry{
		PACURL: "http://127.0.0.1:8790/proxy.pac",
		Scopes: []ProxyScope{{Service: "Wi-Fi", PriorURLPresent: false}},
		CATrust: &CATrustState{
			SHA1: "ABCDEF0123456789ABCDEF0123456789ABCDEF01", CAPath: "/nonexistent/ca.pem", Keychain: systemKeychainPath,
		},
	}
	f := newFixture(t, []string{"Wi-Fi"}, nil, entry.CATrust.SHA1)
	f.urls["Wi-Fi"], f.enabled["Wi-Fi"] = entry.PACURL, true
	rec := &recorder{t: t, handle: f.handler()}

	if _, err := DeactivateSystemPAC(context.Background(), rec.run, entry); err != nil {
		t.Fatalf("DeactivateSystemPAC: %v", err)
	}
	var sawRemove, sawDelete bool
	for _, c := range rec.calls {
		if isCall(c, "security", "remove-trusted-cert") {
			sawRemove = true
		}
		if isCall(c, "security", "delete-certificate") {
			sawDelete = true
		}
	}
	if sawRemove {
		t.Error("remove-trusted-cert was called for a CA file that does not exist")
	}
	if !sawDelete {
		t.Error("delete-certificate was never called")
	}
}

// TestDeactivatePreservesPartialProgressOnAMidLoopError: a scope's read
// failing partway through must not discard the scopes already restored.
func TestDeactivatePreservesPartialProgressOnAMidLoopError(t *testing.T) {
	withSeams(t, 501, true)
	entry := SystemEntry{
		PACURL: "http://127.0.0.1:8790/proxy.pac",
		Scopes: []ProxyScope{
			{Service: "A", PriorURLPresent: false},
			{Service: "B", PriorURLPresent: false},
		},
	}
	f := newFixture(t, []string{"A", "B"}, nil, "")
	f.urls["A"], f.enabled["A"] = entry.PACURL, true
	f.urls["B"], f.enabled["B"] = entry.PACURL, true
	base := f.handler()
	rec := &recorder{t: t}
	rec.handle = func(name string, args []string) ([]byte, error) {
		if isCall(recordedCall{name: name, args: args}, "networksetup", "-getautoproxyurl", "B") {
			return nil, errors.New("boom reading B")
		}
		return base(name, args)
	}

	report, err := DeactivateSystemPAC(context.Background(), rec.run, entry)
	if err == nil {
		t.Fatal("DeactivateSystemPAC succeeded despite the injected failure reading B")
	}
	if len(report.Restored) != 1 || report.Restored[0] != "A" {
		t.Errorf("Restored = %v, want [A]: a mid-loop error must not discard already-restored scopes", report.Restored)
	}
}

// TestManualDeactivateCommandsNeverReferenceAFileAboutToBeDeleted:
// manualDeactivateCommandsDarwin's output is pasted by a developer AFTER the
// caller (uninstall) has already deleted the CA file it names, so it must
// never print a command that needs that file.
func TestManualDeactivateCommandsNeverReferenceAFileAboutToBeDeleted(t *testing.T) {
	entry := SystemEntry{
		Scopes: []ProxyScope{{Service: "Wi-Fi", PriorURLPresent: false}},
		CATrust: &CATrustState{
			SHA1: "ABCDEF0123456789ABCDEF0123456789ABCDEF01", CAPath: "/x/transport-ca.pem", Keychain: systemKeychainPath,
		},
	}
	cmds := manualDeactivateCommandsDarwin(entry)
	var sawDelete bool
	for _, c := range cmds {
		if strings.Contains(c, "remove-trusted-cert") {
			t.Errorf("manual deactivate commands must not name a file the caller is about to delete: %q", c)
		}
		if strings.Contains(c, "delete-certificate") {
			sawDelete = true
			if !strings.Contains(c, entry.CATrust.SHA1) || !strings.Contains(c, systemKeychainPath) {
				t.Errorf("delete-certificate command missing the SHA-1 or keychain: %q", c)
			}
		}
	}
	if !sawDelete {
		t.Error("no delete-certificate command was printed at all")
	}
}

// TestActivateTracesCATrustReadbackAndPACWriteSteps closes phase 3's gap for
// this file: activateSystemPACDarwin ran three privileged steps (trust the
// CA, read its trust back, write each scope's PAC url/state) and traced none
// of them. Each must now leave a StageActivation record, in order, carrying
// its own step name.
func TestActivateTracesCATrustReadbackAndPACWriteSteps(t *testing.T) {
	withSeams(t, 501, true)
	home := t.TempDir()
	plan := testPlan(t, home)
	sha1, err := sha1Fingerprint(plan.CAPEM)
	if err != nil {
		t.Fatalf("sha1Fingerprint: %v", err)
	}
	f := newFixture(t, []string{"Wi-Fi"}, nil, sha1)
	rec := &recorder{t: t, handle: f.handler()}

	dir := t.TempDir()
	restore := trace.SetDefault(&trace.Writer{Dir: dir})
	defer restore()

	outcome, err := ActivateSystemPAC(context.Background(), rec.run, plan)
	if err != nil {
		t.Fatalf("ActivateSystemPAC: %v", err)
	}
	if outcome.Class != Activated {
		t.Fatalf("Class = %v, reason=%q", outcome.Class, outcome.Reason)
	}

	recs, skipped, err := trace.Read(dir, nil)
	if err != nil {
		t.Fatalf("trace.Read: %v", err)
	}
	if skipped != 0 {
		t.Fatalf("skipped = %d, want 0", skipped)
	}
	var steps []string
	for _, r := range recs {
		if r.Stage != trace.StageActivation {
			continue
		}
		if r.Outcome != "ok" {
			t.Errorf("record outcome = %q, want ok: %+v", r.Outcome, r)
		}
		step, _ := r.Detail["step"].(string)
		steps = append(steps, step)
	}
	want := []string{"ca-trust", "ca-trust-readback", "pac-write"}
	if len(steps) != len(want) {
		t.Fatalf("steps = %v, want exactly %v", steps, want)
	}
	for i, w := range want {
		if steps[i] != w {
			t.Errorf("steps[%d] = %q, want %q (full: %v)", i, steps[i], w, steps)
		}
	}
}

// -- shared helpers used only by tests --------------------------------------

func pemEncodeCert(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}
