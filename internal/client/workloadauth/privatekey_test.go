package workloadauth

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"strings"
	"sync"
	"testing"
)

// testKeyOnce/testKeySingleton generate the RSA key once per test binary:
// generating 2048-bit RSA per test would slow the -race suite for no benefit,
// and a PEM literal in this file would collide with the local redactor that
// rewrites secret-shaped literals on disk.
var (
	testKeyOnce      sync.Once
	testKeySingleton *rsa.PrivateKey
)

func testRSAKey(t testing.TB) *rsa.PrivateKey {
	t.Helper()
	testKeyOnce.Do(func() {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatalf("generating test RSA key: %v", err)
		}
		testKeySingleton = k
	})
	return testKeySingleton
}

// testKeyPEM PKCS#8-PEM-encodes k, entirely at runtime: never a literal.
func testKeyPEM(t testing.TB, k *rsa.PrivateKey) string {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatalf("marshal PKCS#8: %v", err)
	}
	block := &pem.Block{Type: "PRIVATE KEY", Bytes: der}
	return string(pem.EncodeToMemory(block))
}

// testKeyPKCS1PEM PKCS#1-PEM-encodes k, to exercise the "refused with
// guidance" path.
func testKeyPKCS1PEM(t testing.TB, k *rsa.PrivateKey) string {
	t.Helper()
	der := x509.MarshalPKCS1PrivateKey(k)
	block := &pem.Block{Type: "RSA PRIVATE KEY", Bytes: der}
	return string(pem.EncodeToMemory(block))
}

func TestPEMAndBase64DERParseToTheSameKey(t *testing.T) {
	want := testRSAKey(t)
	pemForm := testKeyPEM(t, want)

	der, err := x509.MarshalPKCS8PrivateKey(want)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	b64Form := base64.StdEncoding.EncodeToString(der)

	fromPEM, err := ParsePrivateKey(pemForm)
	if err != nil {
		t.Fatalf("ParsePrivateKey(PEM): %v", err)
	}
	fromB64, err := ParsePrivateKey(b64Form)
	if err != nil {
		t.Fatalf("ParsePrivateKey(base64 DER): %v", err)
	}

	if fromPEM.N.Cmp(want.N) != 0 || fromB64.N.Cmp(want.N) != 0 {
		t.Fatal("parsed keys do not match the source modulus")
	}
}

func TestNormalizeProducesOneLine(t *testing.T) {
	k := testRSAKey(t)
	pemForm := testKeyPEM(t, k)

	normalized, err := NormalizePrivateKey(pemForm)
	if err != nil {
		t.Fatalf("NormalizePrivateKey: %v", err)
	}
	if strings.Contains(normalized, "\n") {
		t.Fatalf("normalized form contains a newline: %q", normalized)
	}
	if strings.Contains(normalized, "BEGIN") {
		t.Fatalf("normalized form still carries PEM armor: %q", normalized)
	}
	if _, err := base64.StdEncoding.DecodeString(normalized); err != nil {
		t.Fatalf("normalized form is not std base64: %v", err)
	}

	reparsed, err := ParsePrivateKey(normalized)
	if err != nil {
		t.Fatalf("ParsePrivateKey(normalized): %v", err)
	}
	if reparsed.N.Cmp(k.N) != 0 {
		t.Fatal("normalized form does not round-trip to the same key")
	}
}

func TestKeyUnder2048AndPKCS1AreRefused(t *testing.T) {
	small, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatalf("generating undersized key: %v", err)
	}
	if _, err := ParsePrivateKey(testKeyPEM(t, small)); err == nil {
		t.Fatal("expected a 1024-bit key to be refused")
	}

	pkcs1 := testKeyPKCS1PEM(t, testRSAKey(t))
	_, err = ParsePrivateKey(pkcs1)
	if err == nil {
		t.Fatal("expected a PKCS#1 PEM to be refused")
	}
	if !strings.Contains(err.Error(), "PKCS#1") && !strings.Contains(err.Error(), "PKCS1") {
		t.Fatalf("PKCS#1 rejection should name the format for guidance, got: %v", err)
	}
}

func TestMalformedKeyErrorNamesTheFieldWithoutTheKey(t *testing.T) {
	// A syntactically-plausible but invalid base64 DER blob: valid base64,
	// invalid ASN.1, so it exercises the x509 parse-failure path rather than
	// the base64-decode-failure path.
	garbage := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("not a real der payload, just filler bytes ", 4)))

	_, err := ParsePrivateKey(garbage)
	if err == nil {
		t.Fatal("expected garbage input to be rejected")
	}
	msg := err.Error()
	if !strings.Contains(msg, "OPENBOX_WORKLOAD_PRIVATE_KEY") {
		t.Fatalf("error does not name the field: %v", err)
	}

	// No 16-char window of the input may appear in the error.
	for i := 0; i+16 <= len(garbage); i++ {
		window := garbage[i : i+16]
		if strings.Contains(msg, window) {
			t.Fatalf("error echoes input bytes (window %q): %v", window, err)
		}
	}
}

func TestGenerateKeyProducesRSA2048(t *testing.T) {
	k, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	if bits := k.N.BitLen(); bits < 2048 {
		t.Fatalf("generated key is %d bits, want >= 2048", bits)
	}
}

func TestEncodePrivateKeyRoundTrips(t *testing.T) {
	k := testRSAKey(t)
	encoded, err := EncodePrivateKey(k)
	if err != nil {
		t.Fatalf("EncodePrivateKey: %v", err)
	}
	if strings.Contains(encoded, "\n") {
		t.Fatalf("encoded form contains a newline")
	}
	reparsed, err := ParsePrivateKey(encoded)
	if err != nil {
		t.Fatalf("ParsePrivateKey(encoded): %v", err)
	}
	if reparsed.N.Cmp(k.N) != 0 {
		t.Fatal("encoded form does not round-trip")
	}
}
