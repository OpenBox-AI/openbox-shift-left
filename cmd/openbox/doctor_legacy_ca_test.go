package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/transport"
)

// writeLegacyConstrainedCA writes a CA file pair in the exact shape this
// binary generated before the name constraint was removed:
// PermittedDNSDomains set to api.anthropic.com, critical. It stands in for a
// real older install, since transport.LoadOrCreateCA
// itself no longer produces this shape.
func writeLegacyConstrainedCA(t *testing.T, dir string) {
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
		SerialNumber:                serial,
		Subject:                     pkix.Name{Organization: []string{"OpenBox"}, CommonName: "OpenBox Transport CA (local)"},
		NotBefore:                   now.Add(-time.Hour),
		NotAfter:                    now.Add(2*365*24*time.Hour - time.Hour),
		IsCA:                        true,
		BasicConstraintsValid:       true,
		MaxPathLen:                  0,
		MaxPathLenZero:              true,
		KeyUsage:                    x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		PermittedDNSDomains:         []string{"api.anthropic.com"},
		PermittedDNSDomainsCritical: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create legacy CA certificate: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal legacy CA key: %v", err)
	}
	certPath, keyPath := transport.CAPaths(dir)
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatalf("write legacy CA key: %v", err)
	}
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		t.Fatalf("write legacy CA certificate: %v", err)
	}
}

// TestDoctorNamesALegacyConstrainedCA: a machine holding
// a CA minted under the retired name constraint must be told which hosts
// stay tunnelled, not intercepted, until it is re-issued. A
// machine with a fresh (unconstrained) CA must print nothing of the sort.
func TestDoctorNamesALegacyConstrainedCA(t *testing.T) {
	isolateHomeUnbound(t)

	openboxHome, err := devconfig.Home()
	if err != nil {
		t.Fatalf("devconfig.Home: %v", err)
	}
	writeLegacyConstrainedCA(t, openboxHome)

	out, code := runDoctorHere(t)
	if code != exitOK {
		t.Fatalf("doctor exit = %d, want %d:\n%s", code, exitOK, out)
	}
	if !strings.Contains(out, "legacy constrained CA") {
		t.Errorf("doctor did not name the legacy constrained CA:\n%s", out)
	}
	if !strings.Contains(out, "tunnelled, not intercepted") {
		t.Errorf("doctor's finding does not say what happens to the affected hosts:\n%s", out)
	}
	if !strings.Contains(out, "openbox init") {
		t.Errorf("doctor's finding does not name the remedy:\n%s", out)
	}
	if !strings.Contains(out, "claude.ai") {
		t.Errorf("doctor's finding does not name a host outside the legacy constraint:\n%s", out)
	}
	if strings.Contains(out, "api.anthropic.com") {
		t.Errorf("doctor's finding names api.anthropic.com, which the legacy CA CAN still issue for:\n%s", out)
	}
}

// TestDoctorSaysNothingAboutAFreshCA is the negative control: a freshly
// generated, unconstrained CA must not trigger the legacy-CA finding.
func TestDoctorSaysNothingAboutAFreshCA(t *testing.T) {
	isolateHomeUnbound(t)

	openboxHome, err := devconfig.Home()
	if err != nil {
		t.Fatalf("devconfig.Home: %v", err)
	}
	// isolateHomeUnbound's fakeSupervisor already stands in a stub CA cert (no
	// key) so the transport lane's NODE_EXTRA_CA_CERTS check has something to
	// find; remove it first so LoadOrCreateCA generates a real, complete pair
	// instead of finding a half-present one.
	certPath, _ := transport.CAPaths(openboxHome)
	if err := os.Remove(certPath); err != nil {
		t.Fatalf("remove the fixture's stub CA cert: %v", err)
	}
	if _, err := transport.LoadOrCreateCA(openboxHome); err != nil {
		t.Fatalf("LoadOrCreateCA: %v", err)
	}

	out, code := runDoctorHere(t)
	if code != exitOK {
		t.Fatalf("doctor exit = %d, want %d:\n%s", code, exitOK, out)
	}
	if strings.Contains(out, "legacy constrained CA") {
		t.Errorf("doctor reported a legacy CA finding against a freshly generated CA:\n%s", out)
	}
}
