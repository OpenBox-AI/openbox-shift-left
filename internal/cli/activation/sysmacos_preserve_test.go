package activation

import (
	"context"
	"errors"
	"os"
	"testing"
)

// TestActivateDeclinedRerunKeepsAnEarlierActivation: a second init whose sudo
// prompt is declined must not overwrite the record of an activation that is
// still live. Replacing it with a bare entry would orphan the trusted CA and
// every PAC scope, leaving uninstall nothing to restore.
func TestActivateDeclinedRerunKeepsAnEarlierActivation(t *testing.T) {
	withSeams(t, 501, true)
	home := t.TempDir()
	live := &SystemEntry{
		PACURL:       "http://127.0.0.1:8790/proxy.pac",
		PACActivated: true,
		Scopes:       []ProxyScope{{Service: "Wi-Fi", PriorURLPresent: false}},
		CATrust:      &CATrustState{SHA1: "ABC", CAPath: "/x/transport-ca.pem"},
	}
	if err := persistSystemEntry(home, live); err != nil {
		t.Fatalf("seed: %v", err)
	}
	rec := &recorder{t: t, handle: func(name string, args []string) ([]byte, error) {
		if name == "sudo" {
			return nil, errors.New("sudo: a password is required")
		}
		t.Fatalf("unexpected call after a decline: %s %v", name, args)
		return nil, nil
	}}
	outcome, err := ActivateSystemPAC(context.Background(), rec.run, testPlan(t, home))
	if err != nil {
		t.Fatalf("ActivateSystemPAC: %v", err)
	}
	if outcome.Class != Declined {
		t.Fatalf("Class = %v, want Declined", outcome.Class)
	}
	got, err := LoadSystemEntry(home)
	if err != nil || got == nil {
		t.Fatalf("LoadSystemEntry: %v, %v", got, err)
	}
	if len(got.Scopes) != 1 || got.CATrust == nil || got.CATrust.SHA1 != "ABC" || !got.PACActivated {
		t.Errorf("a declined re-run replaced the live activation record: %+v", got)
	}
}

// TestDeactivateRestoresADisabledPriorURL: a service that had its own PAC URL
// switched off before init gets that URL back, still switched off, rather
// than keeping OpenBox's URL.
func TestDeactivateRestoresADisabledPriorURL(t *testing.T) {
	withSeams(t, 501, true)
	const prior = "http://corp.example/proxy.pac"
	entry := SystemEntry{
		PACURL: "http://127.0.0.1:8790/proxy.pac",
		Scopes: []ProxyScope{{Service: "Wi-Fi", PriorURLPresent: true, PriorURL: prior, PriorEnabled: false}},
	}
	f := newFixture(t, []string{"Wi-Fi"}, nil, "")
	f.urls["Wi-Fi"], f.enabled["Wi-Fi"] = entry.PACURL, true
	rec := &recorder{t: t, handle: f.handler()}

	if _, err := DeactivateSystemPAC(context.Background(), rec.run, entry); err != nil {
		t.Fatalf("DeactivateSystemPAC: %v", err)
	}
	if f.urls["Wi-Fi"] != prior {
		t.Errorf("Wi-Fi URL = %q, want the prior %q restored", f.urls["Wi-Fi"], prior)
	}
	if f.enabled["Wi-Fi"] {
		t.Error("Wi-Fi auto-proxy was left on; the prior state was off")
	}
}

// TestDeactivateDeletesTheCertFromTheSystemKeychainByName: the delete names
// the System keychain explicitly, the measured form, rather than relying on
// root's keychain search list.
func TestDeactivateDeletesTheCertFromTheSystemKeychainByName(t *testing.T) {
	withSeams(t, 501, true)
	entry := SystemEntry{
		PACURL:  "http://127.0.0.1:8790/proxy.pac",
		CATrust: &CATrustState{SHA1: "ABC", CAPath: "/x/transport-ca.pem"},
	}
	f := newFixture(t, nil, nil, "ABC")
	rec := &recorder{t: t, handle: f.handler()}
	if _, err := DeactivateSystemPAC(context.Background(), rec.run, entry); err != nil {
		t.Fatalf("DeactivateSystemPAC: %v", err)
	}
	for _, c := range rec.calls {
		if name, args := c.normalized(); name == "security" && len(args) > 0 && args[0] == "delete-certificate" {
			if args[len(args)-1] != systemKeychainPath {
				t.Errorf("delete-certificate argv = %v; want it to end with %s", args, systemKeychainPath)
			}
			return
		}
	}
	t.Fatal("no delete-certificate call was made")
}

// TestActivateReissueAtTheSamePathDeletesTheOldTrustBySHA1Only: the CA
// filenames are constants, so after a re-issue the file at the recorded path
// is the NEW certificate. remove-trusted-cert on it would fail (it holds no
// trust yet) and strand the old root; the old trust goes by SHA-1 instead.
func TestActivateReissueAtTheSamePathDeletesTheOldTrustBySHA1Only(t *testing.T) {
	withSeams(t, 501, true)
	home := t.TempDir()
	plan := testPlan(t, home)
	if err := os.WriteFile(plan.CAPath, plan.CAPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	newSHA1, err := sha1Fingerprint(plan.CAPEM)
	if err != nil {
		t.Fatal(err)
	}
	oldSHA1, err := sha1Fingerprint(testCAPEM(t))
	if err != nil {
		t.Fatal(err)
	}
	existing := &SystemEntry{
		Schema: systemEntrySchema, PACActivated: true, PACURL: plan.PACURL,
		Scopes:  []ProxyScope{{Service: "Wi-Fi", PriorURLPresent: false}},
		CATrust: &CATrustState{SHA1: oldSHA1, CAPath: plan.CAPath, Keychain: systemKeychainPath},
	}
	if err := persistSystemEntry(home, existing); err != nil {
		t.Fatal(err)
	}
	f := newFixture(t, []string{"Wi-Fi"}, nil, newSHA1)
	f.alsoPresent = []string{oldSHA1}
	rec := &recorder{t: t, handle: f.handler()}

	outcome, err := ActivateSystemPAC(context.Background(), rec.run, plan)
	if err != nil || outcome.Class != Activated {
		t.Fatalf("ActivateSystemPAC = %v, %v (reason %q)", outcome.Class, err, outcome.Reason)
	}
	deletedOld := false
	for _, c := range rec.calls {
		if isCall(c, "security", "remove-trusted-cert") {
			t.Errorf("remove-trusted-cert ran against the re-issued file: %v", c)
		}
		if isCall(c, "security", "delete-certificate", "-Z", oldSHA1) {
			deletedOld = true
		}
	}
	if !deletedOld {
		t.Error("the old CA's trust was not deleted by SHA-1")
	}
}

// TestDeactivateSkipsUntrustWhenTheCertWasNeverTrusted: a run killed before
// its trust write landed leaves a record naming a certificate the keychain
// does not hold. Uninstall has nothing to untrust and must not report a
// failure for it.
func TestDeactivateSkipsUntrustWhenTheCertWasNeverTrusted(t *testing.T) {
	withSeams(t, 501, true)
	entry := SystemEntry{
		PACURL:  "http://127.0.0.1:8790/proxy.pac",
		CATrust: &CATrustState{SHA1: "ABC", CAPath: "/nonexistent/transport-ca.pem"},
	}
	f := newFixture(t, nil, nil, "")
	rec := &recorder{t: t, handle: f.handler()}
	if _, err := DeactivateSystemPAC(context.Background(), rec.run, entry); err != nil {
		t.Fatalf("DeactivateSystemPAC: %v", err)
	}
	for _, c := range rec.calls {
		if isCall(c, "security", "remove-trusted-cert") || isCall(c, "security", "delete-certificate") {
			t.Errorf("untrust ran for a certificate the keychain does not hold: %v", c)
		}
	}
}
