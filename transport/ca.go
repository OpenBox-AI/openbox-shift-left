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
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

// ca.go — the project CA this lane terminates TLS with.
//
// This is the highest-value secret this product places on a developer machine
// after the signing key: a CA that can impersonate the provider to THIS HOST. It
// lives under the same trust boundary as ~/.openbox/.env (ADR-0015) — anything
// running as the developer, the governed agent included, can read it — so the
// controls here are about not making that boundary WORSE, not about pretending
// it is tamper-proof. Four of them:
//
//   - the key is 0600 and the loader REFUSES to run when it is looser;
//   - the CA is NAME-CONSTRAINED to the intercepted host, so even a leaked key
//     cannot mint a usable certificate for anything else;
//   - it is never added to any system trust store — the client is pointed at the
//     PEM directly (phase 11 requirement 5, "trusted by the client only");
//   - removal deletes it (OD2, one command out).
//
// Leaves are minted HERE with stdlib crypto/x509 rather than through goproxy's
// TLSConfigFromCA, for two measured reasons. goproxy's helper clones a
// defaultTLSConfig that sets InsecureSkipVerify (certs.go:27), and it returns a
// config with no ALPN of ours; building the config here keeps both properties
// visible and asserted instead of inherited.

// caCertFile and caKeyFile are the on-disk names, under the OpenBox config dir.
const (
	caCertFile = "transport-ca.pem"
	caKeyFile  = "transport-ca.key"
)

// caLifetime is how long a generated CA is valid. Long enough that a developer
// does not hit an expiry mid-sprint, short enough to bound a leak.
const caLifetime = 2 * 365 * 24 * time.Hour

// ErrCAPermissions is returned when the CA key is readable beyond its owner.
//
// A distinct sentinel because the caller has to tell it apart from a corrupt
// file: one is fixed with chmod, the other by deleting and reinstalling, and a
// single opaque error would send the developer down the wrong path.
var ErrCAPermissions = errors.New("transport: CA key permissions are too open")

// CA is the project certificate authority plus its minted leaves.
type CA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte

	// leaves caches one tls.Config per host. The allowlist holds a single host in
	// practice, so this is at most one entry — but Claude Code opens many
	// connections and re-signing per CONNECT would burn CPU on the model-call
	// path for no gain.
	mu     sync.Mutex
	leaves map[string]*tls.Config
}

// CAPaths returns where the CA certificate and key live under dir.
//
// dir is a parameter rather than resolved from devconfig.Home() because this
// module's dependency guard allows exactly one direct require (goproxy), and
// because the CLI already owns path resolution for every other lane.
func CAPaths(dir string) (certPath, keyPath string) {
	return filepath.Join(dir, caCertFile), filepath.Join(dir, caKeyFile)
}

// LoadOrCreateCA returns the CA in dir, generating one if absent.
//
// Generating ONCE is the property that matters. A CA regenerated on each start
// would invalidate the trust the client was configured with, so every model call
// would fail its handshake after a restart — and the developer would see a
// broken tool rather than a governance message.
//
// A file pair that exists but cannot be read is an ERROR, never a reason to
// regenerate: overwriting would destroy the key the client already trusts and
// report success.
func LoadOrCreateCA(dir string) (*CA, error) {
	certPath, keyPath := CAPaths(dir)

	_, certErr := os.Stat(certPath)
	_, keyErr := os.Stat(keyPath)
	switch {
	case certErr == nil && keyErr == nil:
		return loadCA(certPath, keyPath)
	case os.IsNotExist(certErr) && os.IsNotExist(keyErr):
		return createCA(dir, certPath, keyPath)
	default:
		// Exactly one half present. Regenerating would silently replace whichever
		// half survived; say what is wrong instead.
		return nil, fmt.Errorf("transport: the CA is half-present in %s (cert: %v, key: %v); "+
			"remove both files and reinstall", dir, certErr, keyErr)
	}
}

// loadCA reads an existing CA, refusing a key other accounts can read.
func loadCA(certPath, keyPath string) (*CA, error) {
	if err := requireOwnerOnly(keyPath); err != nil {
		return nil, err
	}
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, fmt.Errorf("transport: read CA certificate: %w", err)
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("transport: read CA key: %w", err)
	}

	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil || certBlock.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("transport: %s is not a PEM certificate", certPath)
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("transport: parse CA certificate: %w", err)
	}
	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return nil, fmt.Errorf("transport: %s is not a PEM key", keyPath)
	}
	key, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("transport: parse CA key: %w", err)
	}
	return &CA{cert: cert, key: key, pem: certPEM, leaves: map[string]*tls.Config{}}, nil
}

// createCA generates a fresh name-constrained CA and persists it.
func createCA(dir, certPath, keyPath string) (*CA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("transport: generate CA key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("transport: generate CA serial: %w", err)
	}

	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			Organization: []string{"OpenBox"},
			CommonName:   "OpenBox Transport CA (local)",
		},
		// Backdated by an hour so a machine whose clock is slightly behind the
		// generating process does not reject a certificate it just made.
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(caLifetime - time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		// The name constraint is the control that survives a key leak. Without it
		// this key can impersonate ANY host to anything that trusts it; with it,
		// only the hosts named here, whatever else the holder attempts.
		PermittedDNSDomains:         constrainedDomains(),
		PermittedDNSDomainsCritical: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("transport: create CA certificate: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("transport: parse generated CA certificate: %w", err)
	}

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("transport: marshal CA key: %w", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("transport: create %s: %w", dir, err)
	}
	// The KEY first, and at 0600 from the moment it exists: os.WriteFile applies
	// the mode at create time, so there is no window where it is world-readable.
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return nil, fmt.Errorf("transport: write CA key: %w", err)
	}
	// 0644: the client has to READ this to trust it. It is a public certificate;
	// only the key above is a secret.
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return nil, fmt.Errorf("transport: write CA certificate: %w", err)
	}
	return &CA{cert: cert, key: key, pem: certPEM, leaves: map[string]*tls.Config{}}, nil
}

// constrainedDomains is what a generated CA may ever sign for.
//
// It is the DEFAULT allowlist host rather than the configured one, deliberately:
// the constraint is baked into a certificate at generation and cannot be widened
// later without regenerating, so deriving it from mutable configuration would
// mean a config edit silently produced a CA with a broader reach than the one
// the developer already trusted.
func constrainedDomains() []string { return []string{DefaultInterceptHost} }

// Certificate returns the CA certificate.
func (c *CA) Certificate() *x509.Certificate { return c.cert }

// CertPEM returns the PEM the client is pointed at to trust this CA.
func (c *CA) CertPEM() []byte { return c.pem }

// ServerConfigFor returns the TLS server configuration for one intercepted host.
//
// Built here rather than borrowed from goproxy's TLSConfigFromCA so that both
// properties the tests assert are set explicitly: ALPN advertises http/1.1 and
// nothing else, because the relay behind this is HTTP/1.1-only and a negotiated
// h2 would make the model call fail in a way that looks like a network fault;
// and InsecureSkipVerify stays false, which goproxy's defaultTLSConfig does not.
func (c *CA) ServerConfigFor(host string) (*tls.Config, error) {
	host = normalizeHost(host)

	c.mu.Lock()
	defer c.mu.Unlock()
	if cfg, ok := c.leaves[host]; ok {
		return cfg, nil
	}

	leaf, err := c.mintLeaf(host)
	if err != nil {
		return nil, err
	}
	cfg := &tls.Config{
		Certificates: []tls.Certificate{*leaf},
		MinVersion:   tls.VersionTLS12,
		// http/1.1 ONLY. See the doc comment; the assertion lives in
		// TestServerConfigNeverNegotiatesHTTP2.
		NextProtos: []string{"http/1.1"},
	}
	c.leaves[host] = cfg
	return cfg, nil
}

// mintLeaf signs a server certificate for host with the CA.
//
// It verifies the result against the CA before returning it. That check is not
// ceremony: the CA is name-constrained, so a host outside the constraint yields a
// certificate that every verifier rejects — and failing HERE says which host and
// why, where failing at handshake time surfaces in the developer's tool as an
// unexplained TLS error with no mention of OpenBox.
func (c *CA) mintLeaf(host string) (*tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("transport: generate leaf key for %s: %w", host, err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("transport: generate leaf serial: %w", err)
	}

	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: host},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(caLifetime - time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		// IsCA stays false: a leaf that can sign further certificates would widen
		// the blast radius for no benefit.
	}
	if ip := net.ParseIP(host); ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	} else {
		tmpl.DNSNames = []string{host}
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, &key.PublicKey, c.key)
	if err != nil {
		return nil, fmt.Errorf("transport: sign leaf for %s: %w", host, err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("transport: parse leaf for %s: %w", host, err)
	}

	pool := x509.NewCertPool()
	pool.AddCert(c.cert)
	if _, err := leaf.Verify(x509.VerifyOptions{
		DNSName:   host,
		Roots:     pool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		return nil, fmt.Errorf("transport: the CA cannot issue a usable certificate for %s "+
			"(it is name-constrained to %v): %w", host, c.cert.PermittedDNSDomains, err)
	}

	return &tls.Certificate{
		Certificate: [][]byte{der},
		PrivateKey:  key,
		Leaf:        leaf,
	}, nil
}

// RemoveCA deletes the CA files. Idempotent: `--remove-transport` has to be
// runnable on a machine where it already ran, exactly like --remove-gateway.
func RemoveCA(dir string) error {
	certPath, keyPath := CAPaths(dir)
	for _, p := range []string{keyPath, certPath} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("transport: remove %s: %w", p, err)
		}
	}
	return nil
}

// requireOwnerOnly refuses a CA key any other account can read.
//
// Windows is exempt, and that is ADR-0015's stated posture rather than an
// oversight: ~/.openbox has no at-rest protection there, and a unix-mode check
// against a Windows file mode would refuse to start on every Windows machine
// while protecting nothing.
func requireOwnerOnly(keyPath string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	info, err := os.Stat(keyPath)
	if err != nil {
		return fmt.Errorf("transport: stat CA key: %w", err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("%w: %s is %04o; anything that can read it can impersonate the "+
			"provider to this machine. Run: chmod 600 %s", ErrCAPermissions, keyPath, perm, keyPath)
	}
	return nil
}
