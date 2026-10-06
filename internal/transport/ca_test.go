package transport

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"os"
	"runtime"
	"testing"
	"time"
)

// TestLoadOrCreateCAGeneratesOnceAndPersists. The "once" half is the load-
// bearing one.
func TestLoadOrCreateCAGeneratesOnceAndPersists(t *testing.T) {
	dir := t.TempDir()

	first, err := LoadOrCreateCA(dir)
	if err != nil {
		t.Fatalf("LoadOrCreateCA: %v", err)
	}
	second, err := LoadOrCreateCA(dir)
	if err != nil {
		t.Fatalf("LoadOrCreateCA (second call): %v", err)
	}

	if !first.Certificate().Equal(second.Certificate()) {
		t.Error("the second LoadOrCreateCA returned a DIFFERENT CA: a regenerated CA invalidates " +
			"the trust the client was configured with, so every model call fails its handshake after a restart")
	}
}

// TestCAKeyIsOwnerOnly holds the file-permission half of the security note.
func TestCAKeyIsOwnerOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix file modes; Windows has no at-rest protection for ~/.openbox ")
	}
	dir := t.TempDir()
	if _, err := LoadOrCreateCA(dir); err != nil {
		t.Fatalf("LoadOrCreateCA: %v", err)
	}
	certPath, keyPath := CAPaths(dir)

	keyInfo, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("stat key: %v", err)
	}
	if got := keyInfo.Mode().Perm(); got != 0o600 {
		t.Errorf("CA key mode = %04o, want 0600", got)
	}
	certInfo, err := os.Stat(certPath)
	if err != nil {
		t.Fatalf("stat cert: %v", err)
	}
	if got := certInfo.Mode().Perm(); got != 0o644 {
		t.Errorf("CA cert mode = %04o, want 0644 (the client has to read it to trust it)", got)
	}
}

// TestLoadOrCreateCARefusesALooseKey is the refuse-to-run half.
func TestLoadOrCreateCARefusesALooseKey(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix file modes; Windows has no at-rest protection for ~/.openbox ")
	}
	for _, mode := range []os.FileMode{0o640, 0o604, 0o644, 0o666} {
		dir := t.TempDir()
		if _, err := LoadOrCreateCA(dir); err != nil {
			t.Fatalf("LoadOrCreateCA: %v", err)
		}
		_, keyPath := CAPaths(dir)
		if err := os.Chmod(keyPath, mode); err != nil {
			t.Fatalf("chmod: %v", err)
		}
		_, err := LoadOrCreateCA(dir)
		if err == nil {
			t.Errorf("mode %04o: LoadOrCreateCA succeeded on a key readable beyond its owner; it must refuse", mode)
			continue
		}
		if !errors.Is(err, ErrCAPermissions) {
			t.Errorf("mode %04o: error %v does not match ErrCAPermissions, so a caller cannot tell "+
				"a permission refusal from a corrupt file", mode, err)
		}
	}
}

// TestCAShapeIsWhatThePhaseSpecifies: P-256, a CA, ~2 years.
func TestCAShapeIsWhatThePhaseSpecifies(t *testing.T) {
	dir := t.TempDir()
	ca, err := LoadOrCreateCA(dir)
	if err != nil {
		t.Fatalf("LoadOrCreateCA: %v", err)
	}
	leaf := ca.Certificate()

	if _, ok := leaf.PublicKey.(*ecdsa.PublicKey); !ok {
		t.Errorf("CA public key is %T, want *ecdsa.PublicKey (P-256)", leaf.PublicKey)
	}
	if !leaf.IsCA || leaf.BasicConstraintsValid != true {
		t.Error("the CA certificate does not assert IsCA with valid basic constraints, so it cannot sign a leaf")
	}
	if leaf.KeyUsage&x509.KeyUsageCertSign == 0 {
		t.Error("the CA certificate lacks KeyUsageCertSign")
	}
	life := leaf.NotAfter.Sub(leaf.NotBefore)
	if life < 2*365*24*time.Hour-48*time.Hour || life > 2*365*24*time.Hour+48*time.Hour {
		t.Errorf("CA lifetime = %v, want ~2 years", life)
	}
	// Deliberate: a freshly generated CA must carry NO
	// PermittedDNSDomains. The blast radius this widens is real and accepted --
	// a leaked ~/.openbox/transport-ca.key can now mint a certificate for ANY
	// host this machine trusts it for, not just the six the table names -- and
	// containment moves to the allowlist instead (an unlisted CONNECT is
	// blind-tunnelled, never decrypted). A PR re-adding this field is changing
	// that design, not fixing a regression.
	if len(leaf.PermittedDNSDomains) != 0 {
		t.Errorf("the CA carries a name constraint (%v); it must be unconstrained, "+
			"or every host outside it fails its handshake in a way that looks like the "+
			"provider being down", leaf.PermittedDNSDomains)
	}
}

// TestLeafVerifiesAgainstTheCAAlone is the evidence that the minted leaf is
// actually usable: a pool containing only our CA must verify it for the host.
func TestLeafVerifiesAgainstTheCAAlone(t *testing.T) {
	dir := t.TempDir()
	ca, err := LoadOrCreateCA(dir)
	if err != nil {
		t.Fatalf("LoadOrCreateCA: %v", err)
	}
	const host = "api.anthropic.com"
	cfg, err := ca.ServerConfigFor(host)
	if err != nil {
		t.Fatalf("ServerConfigFor: %v", err)
	}
	if len(cfg.Certificates) != 1 {
		t.Fatalf("ServerConfigFor returned %d certificates, want 1", len(cfg.Certificates))
	}
	leaf, err := x509.ParseCertificate(cfg.Certificates[0].Certificate[0])
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}

	pool := x509.NewCertPool()
	pool.AddCert(ca.Certificate())
	if _, err := leaf.Verify(x509.VerifyOptions{
		DNSName:   host,
		Roots:     pool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		t.Errorf("leaf does not verify for %s against our own CA: %v", host, err)
	}
	if leaf.IsCA {
		t.Error("the minted leaf asserts IsCA; a leaf that can sign further certificates widens the blast radius")
	}
}

// TestServerConfigNeverNegotiatesHTTP2 pins the ALPN set.
func TestServerConfigNeverNegotiatesHTTP2(t *testing.T) {
	dir := t.TempDir()
	ca, err := LoadOrCreateCA(dir)
	if err != nil {
		t.Fatalf("LoadOrCreateCA: %v", err)
	}
	cfg, err := ca.ServerConfigFor("api.anthropic.com")
	if err != nil {
		t.Fatalf("ServerConfigFor: %v", err)
	}
	for _, proto := range cfg.NextProtos {
		if proto != "http/1.1" {
			t.Errorf("ALPN advertises %q; this relay speaks HTTP/1.1 only, and negotiating anything else "+
				"makes the client's model call fail in a way that looks like a network fault", proto)
		}
	}
	if len(cfg.NextProtos) == 0 {
		t.Error("ALPN advertises nothing; a client that offers only h2 would then have no shared protocol")
	}
	if cfg.InsecureSkipVerify {
		t.Error("InsecureSkipVerify is set on the server config")
	}
	if cfg.MinVersion < tls.VersionTLS12 {
		t.Errorf("MinVersion = %#x, want at least TLS 1.2", cfg.MinVersion)
	}
}

// TestHandshakeOverAnInMemoryPipe is the one that makes the rest evidence
// rather than structure-checking: a real TLS handshake, client and server,
// with our CA as the only root; and no socket anywhere.
func TestHandshakeOverAnInMemoryPipe(t *testing.T) {
	dir := t.TempDir()
	ca, err := LoadOrCreateCA(dir)
	if err != nil {
		t.Fatalf("LoadOrCreateCA: %v", err)
	}
	const host = "api.anthropic.com"
	serverCfg, err := ca.ServerConfigFor(host)
	if err != nil {
		t.Fatalf("ServerConfigFor: %v", err)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca.CertPEM()) {
		t.Fatal("CertPEM() did not parse as PEM; the client cannot be configured to trust this CA")
	}

	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() { clientConn.Close(); serverConn.Close() })

	const payload = "hello over the intercepted tunnel"
	errc := make(chan error, 1)
	go func() {
		s := tls.Server(serverConn, serverCfg)
		if err := s.Handshake(); err != nil {
			errc <- err
			return
		}
		_, err := io.WriteString(s, payload)
		errc <- err
	}()

	c := tls.Client(clientConn, &tls.Config{ServerName: host, RootCAs: pool})
	if err := c.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatalf("SetDeadline: %v", err)
	}
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(c, got); err != nil {
		t.Fatalf("client read: %v (server: %v)", err, <-errc)
	}
	if string(got) != payload {
		t.Errorf("read %q, want %q", got, payload)
	}
	if err := <-errc; err != nil {
		t.Fatalf("server: %v", err)
	}
}

// TestServerConfigIssuesForAnyHostNowThatTheCAIsUnconstrained is deliberate:
// the CA itself no longer refuses a host outside the table, on
// purpose -- containment is the allowlist's job now (an unlisted CONNECT is
// never hijacked, so ServerConfigFor is never even called for it in
// production). This asserts the CA's own new blast radius directly, so a
// later "restore" of the constraint fails a named test rather than an
// abstract review comment.
func TestServerConfigIssuesForAnyHostNowThatTheCAIsUnconstrained(t *testing.T) {
	dir := t.TempDir()
	ca, err := LoadOrCreateCA(dir)
	if err != nil {
		t.Fatalf("LoadOrCreateCA: %v", err)
	}
	if _, err := ca.ServerConfigFor("evil.test"); err != nil {
		t.Errorf("ServerConfigFor refused a host outside the old table (%v); the CA is meant to be "+
			"unconstrained, with the allowlist as the sole containment", err)
	}
}

// TestCANeedsReissue: a fresh CA needs nothing, and a legacy CA minted under
// the retired name constraint is flagged so `doctor` and a later `init` can
// act on it. LoadOrCreateCA must not reissue anything itself -- a running
// relay swapping its CA mid-flight breaks in-flight handshakes -- so this
// only checks the fact, never the file on disk.
func TestCANeedsReissue(t *testing.T) {
	t.Run("a fresh CA needs no reissue", func(t *testing.T) {
		ca, err := LoadOrCreateCA(t.TempDir())
		if err != nil {
			t.Fatalf("LoadOrCreateCA: %v", err)
		}
		if CANeedsReissue(ca) {
			t.Error("CANeedsReissue is true for a freshly generated, unconstrained CA")
		}
	})

	t.Run("nil CA needs no reissue", func(t *testing.T) {
		if CANeedsReissue(nil) {
			t.Error("CANeedsReissue is true for a nil CA")
		}
	})

	t.Run("a legacy constrained CA needs a reissue", func(t *testing.T) {
		dir := t.TempDir()
		writeLegacyConstrainedCA(t, dir)

		ca, err := LoadOrCreateCA(dir)
		if err != nil {
			t.Fatalf("LoadOrCreateCA on a legacy CA file pair: %v", err)
		}
		if !CANeedsReissue(ca) {
			t.Error("CANeedsReissue is false for a CA that still carries PermittedDNSDomains " +
				"(the legacy constrained shape); a machine holding it will fail every handshake for a " +
				"host outside the old constraint in a way that looks like the provider being down")
		}
	})
}

// writeLegacyConstrainedCA writes a CA file pair in the exact shape this
// package generated before CAs became unconstrained: PermittedDNSDomains
// set to api.anthropic.com, critical. It stands in for a real legacy
// install, since createCA itself no longer produces this shape.
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
	certPath, keyPath := CAPaths(dir)
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatalf("write legacy CA key: %v", err)
	}
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		t.Fatalf("write legacy CA certificate: %v", err)
	}
}

// `--remove-transport` does not delete it at all, which by this package's own
// argument leaves a trusted signing key behind after the relay that used it is
// gone. Both are open, and neither is a regression from this deletion: the
// helper this test covered was never on either path.

// TestLoadOrCreateCARejectsACorruptFilePairWithoutOverwriting. Silently
// regenerating over an unreadable CA would destroy the key the client was
// configured to trust and report success.
func TestLoadOrCreateCARejectsACorruptFilePairWithoutOverwriting(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadOrCreateCA(dir); err != nil {
		t.Fatalf("LoadOrCreateCA: %v", err)
	}
	certPath, _ := CAPaths(dir)
	if err := os.WriteFile(certPath, []byte("not a certificate"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := LoadOrCreateCA(dir); err == nil {
		t.Fatal("LoadOrCreateCA accepted a corrupt certificate file")
	}
	raw, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(raw) != "not a certificate" {
		t.Error("LoadOrCreateCA overwrote the existing CA files after failing to read them")
	}
}
