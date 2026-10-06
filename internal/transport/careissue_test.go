package transport

import (
	"bytes"
	"os"
	"testing"
)

// TestReissueIfNeededLeavesAFreshCAAlone: CANeedsReissue is false for
// anything this package mints today, so ReissueIfNeeded must be a no-op --
// no file touched, same certificate returned.
func TestReissueIfNeededLeavesAFreshCAAlone(t *testing.T) {
	dir := t.TempDir()
	original, err := LoadOrCreateCA(dir)
	if err != nil {
		t.Fatalf("LoadOrCreateCA: %v", err)
	}
	certPath, keyPath := CAPaths(dir)
	certBefore, err := os.Stat(certPath)
	if err != nil {
		t.Fatalf("stat cert: %v", err)
	}

	got, err := ReissueIfNeeded(dir)
	if err != nil {
		t.Fatalf("ReissueIfNeeded: %v", err)
	}
	if !bytes.Equal(original.CertPEM(), got.CertPEM()) {
		t.Error("ReissueIfNeeded replaced a fresh CA that never needed it")
	}
	certAfter, err := os.Stat(certPath)
	if err != nil {
		t.Fatalf("stat cert after: %v", err)
	}
	if !certAfter.ModTime().Equal(certBefore.ModTime()) {
		t.Error("ReissueIfNeeded rewrote the cert file for a CA that did not need reissuing")
	}
	if CANeedsReissue(got) {
		t.Error("CANeedsReissue is true after ReissueIfNeeded on an already-fresh CA")
	}
	if _, err := os.Stat(keyPath); err != nil {
		t.Errorf("key file missing after ReissueIfNeeded: %v", err)
	}
}

// TestReissueIfNeededReplacesALegacyConstrainedCA is the one-time migration:
// a CA minted under the retired name constraint gets a new, unconstrained
// pair under the same filenames.
func TestReissueIfNeededReplacesALegacyConstrainedCA(t *testing.T) {
	dir := t.TempDir()
	writeLegacyConstrainedCA(t, dir)

	legacy, err := LoadOrCreateCA(dir)
	if err != nil {
		t.Fatalf("LoadOrCreateCA on the legacy pair: %v", err)
	}
	if !CANeedsReissue(legacy) {
		t.Fatal("test setup: the legacy CA should need a reissue")
	}

	fresh, err := ReissueIfNeeded(dir)
	if err != nil {
		t.Fatalf("ReissueIfNeeded: %v", err)
	}
	if CANeedsReissue(fresh) {
		t.Error("the reissued CA still needs a reissue")
	}
	if bytes.Equal(legacy.CertPEM(), fresh.CertPEM()) {
		t.Error("ReissueIfNeeded returned the same certificate as the legacy one")
	}

	certPath, keyPath := CAPaths(dir)
	if _, err := os.Stat(certPath); err != nil {
		t.Errorf("cert file missing at its original path after reissue: %v", err)
	}
	if _, err := os.Stat(keyPath); err != nil {
		t.Errorf("key file missing at its original path after reissue: %v", err)
	}

	reloaded, err := LoadOrCreateCA(dir)
	if err != nil {
		t.Fatalf("LoadOrCreateCA after reissue: %v", err)
	}
	if !bytes.Equal(reloaded.CertPEM(), fresh.CertPEM()) {
		t.Error("the file on disk does not match what ReissueIfNeeded returned")
	}
}

// TestReissueIfNeededIsIdempotent: a second call must not reissue again.
func TestReissueIfNeededIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	writeLegacyConstrainedCA(t, dir)

	first, err := ReissueIfNeeded(dir)
	if err != nil {
		t.Fatalf("first ReissueIfNeeded: %v", err)
	}
	second, err := ReissueIfNeeded(dir)
	if err != nil {
		t.Fatalf("second ReissueIfNeeded: %v", err)
	}
	if !bytes.Equal(first.CertPEM(), second.CertPEM()) {
		t.Error("a second ReissueIfNeeded call reissued again instead of being a no-op")
	}
}
