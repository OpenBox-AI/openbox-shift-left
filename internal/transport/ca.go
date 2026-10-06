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

//   - The key is 0600 and the loader refuses to run when it is looser; - the
//     CA is

const (
	caCertFile = "transport-ca.pem"
	caKeyFile  = "transport-ca.key"
)

const caLifetime = 2 * 365 * 24 * time.Hour

// ErrCAPermissions is returned when the CA key is readable beyond its owner.
var ErrCAPermissions = errors.New("transport: CA key permissions are too open")

// CA is the project certificate authority plus its minted leaves.
type CA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte

	mu     sync.Mutex
	leaves map[string]*tls.Config
}

// CAPaths returns where the CA certificate and key live under dir.
func CAPaths(dir string) (certPath, keyPath string) {
	return filepath.Join(dir, caCertFile), filepath.Join(dir, caKeyFile)
}

// LoadOrCreateCA returns the CA in dir, generating one if absent. A file pair
// that exists but cannot be read is an error, never a reason to regenerate:
// overwriting would destroy the key the client already trusts and report
// success.
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
		// Regenerating would silently replace whichever half survived; say what is
		// wrong instead.
		return nil, fmt.Errorf("transport: the CA is half-present in %s (cert: %v, key: %v); "+
			"remove both files and reinstall", dir, certErr, keyErr)
	}
}

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
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(caLifetime - time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		// No PermittedDNSDomains: the CA is unconstrained. It may sign a leaf
		// for any host; the allowlist, not the CA, is what a widening provider
		// union or a leaked key is contained by. See CANeedsReissue for the
		// one-time migration a CA minted with the older name constraint still
		// needs.
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
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return nil, fmt.Errorf("transport: write CA key: %w", err)
	}
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return nil, fmt.Errorf("transport: write CA certificate: %w", err)
	}
	return &CA{cert: cert, key: key, pem: certPEM, leaves: map[string]*tls.Config{}}, nil
}

// CANeedsReissue reports whether ca is a legacy CA minted with a name
// constraint, before CAs became unconstrained: a machine holding one of those
// still carries PermittedDNSDomains, and every host outside it stays
// blind-tunnelled rather than intercepted (Proxy.intercepts guards on
// CanIssueFor for exactly this reason) until the CA is reissued.
// LoadOrCreateCA does not auto-reissue -- swapping a running relay's CA
// mid-flight breaks in-flight handshakes and silently invalidates the
// client's trusted copy -- so the caller (doctor, and a later `init` run)
// decides when to act on this.
func CANeedsReissue(ca *CA) bool {
	return ca != nil && ca.cert != nil && len(ca.cert.PermittedDNSDomains) > 0
}

// CanIssueFor reports whether ca can mint a usable leaf for host: always true
// for an unconstrained CA (the shape every newly minted CA has), and for a
// legacy CA still carrying PermittedDNSDomains, true
// only when host equals or is a label-boundary subdomain of one of them --
// the same boundary rule Allowlist.Allows uses, so a legacy CA constrained to
// "api.anthropic.com" does not also claim "evilapi.anthropic.com.evil.test".
// This is what keeps a machine holding today's CA from ever attempting a
// handshake for a host outside its constraint: Proxy.intercepts gates on it
// so that host stays blind-tunnelled -- exactly today's behaviour -- instead
// of hitting a mintLeaf failure that closes the connection and looks like the
// provider being down.
func (c *CA) CanIssueFor(host string) bool {
	if c == nil || c.cert == nil || len(c.cert.PermittedDNSDomains) == 0 {
		return true
	}
	n := normalizeHost(host)
	if n == "" {
		return false
	}
	for _, d := range c.cert.PermittedDNSDomains {
		if onLabelBoundary(n, asciiLower(d)) {
			return true
		}
	}
	return false
}

// Certificate returns the CA certificate.
func (c *CA) Certificate() *x509.Certificate { return c.cert }

// CertPEM returns the PEM the client is pointed at to trust this CA.
func (c *CA) CertPEM() []byte { return c.pem }

// ServerConfigFor returns the TLS server configuration for one intercepted
// host.
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
		NextProtos:   []string{"http/1.1"},
	}
	c.leaves[host] = cfg
	return cfg, nil
}

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
		return nil, fmt.Errorf("transport: the CA cannot issue a usable certificate for %s: %w", host, err)
	}

	return &tls.Certificate{
		Certificate: [][]byte{der},
		PrivateKey:  key,
		Leaf:        leaf,
	}, nil
}

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
