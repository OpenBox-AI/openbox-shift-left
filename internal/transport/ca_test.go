package transport

import (
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"os"
	"runtime"
	"testing"
	"time"
)

// TestLoadOrCreateCAGeneratesOnceAndPersists.
//
// The "once" half is the load-bearing one. A CA regenerated on every start would
// invalidate the trust the client was configured with, so every model call would
// fail its TLS handshake after a restart — and the developer would see a broken
// tool, not a governance message.
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
//
// The CA can impersonate the provider to this machine. It sits under the same
// trust boundary as ~/.openbox/.env — anything running as the developer can
// read it — but that is not a reason to widen it further.
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
//
// A key readable by group or other on a shared machine is an impersonation
// capability handed to another account. Refusing is the only safe answer: a
// silent chmod would hide that it ever happened.
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
	// A CA that constrains itself to the host it exists for cannot be repurposed
	// against another host even if the key leaks.
	if len(leaf.PermittedDNSDomains) == 0 {
		t.Error("the CA has no name constraint; a leaked key could then be used to impersonate any host, " +
			"not just the one this lane intercepts")
	}
}

// TestLeafVerifiesAgainstTheCAAlone is the evidence that the minted leaf is
// actually usable: a pool containing ONLY our CA must verify it for the host.
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
//
// The relay behind this config is HTTP/1.1-only. If ALPN negotiated h2 the
// client would speak HTTP/2 frames into an HTTP/1.1 server, and the model call
// would fail — while the failure looked like a provider or network problem.
//
// It also pins InsecureSkipVerify off. goproxy's own defaultTLSConfig sets it
// true (certs.go:27), so a config borrowed from goproxy rather than built here
// would carry it in.
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

// TestHandshakeOverAnInMemoryPipe is the one that makes the rest evidence rather
// than structure-checking: a REAL TLS handshake, client and server, with our CA
// as the only root — and no socket anywhere.
//
// This host denies bind (see memhttptest), so a socket-based version of this
// test would SKIP and the CA would ship unexercised. net.Pipe carries a TLS
// handshake perfectly well; what it does not measure is bind, listen or the
// dialer, and nothing here claims it does.
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
	// A deadline, not a bare read: a handshake failure here would otherwise HANG,
	// and a stalled `go test` reads as an environment problem rather than an
	// answer (CLAUDE.md, the goproxy spike).
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

// TestServerConfigRefusesAHostOutsideTheNameConstraint.
//
// The CA is name-constrained, so minting a leaf for another host would produce a
// certificate no verifier accepts. Failing at mint time says so; failing at
// handshake time would surface as an unexplained TLS error in the developer's
// tool.
func TestServerConfigRefusesAHostOutsideTheNameConstraint(t *testing.T) {
	dir := t.TempDir()
	ca, err := LoadOrCreateCA(dir)
	if err != nil {
		t.Fatalf("LoadOrCreateCA: %v", err)
	}
	if _, err := ca.ServerConfigFor("evil.test"); err == nil {
		t.Error("ServerConfigFor minted a leaf for a host outside the CA's name constraint")
	}
}

// TestRemoveCADeletesBothFiles is deleted with RemoveCA, which had no caller.
//
// `--remove-all` deletes the CA inline in purgeLaneData, and nothing asserts
// that it does. `--remove-transport` does not delete it at all, which by this
// package's own argument leaves a trusted signing key behind after the relay
// that used it is gone. Both are open, and neither is a regression from this
// deletion: the helper this test covered was never on either path.

// TestLoadOrCreateCARejectsACorruptFilePairWithoutOverwriting.
//
// Silently regenerating over an unreadable CA would destroy the key the client
// was configured to trust and report success.
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
