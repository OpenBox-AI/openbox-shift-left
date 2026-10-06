package workloadauth

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"strings"
)

// keyEnvName is the field every private-key error names -- never the key
// bytes themselves, per SDK behaviour 4.
const keyEnvName = "OPENBOX_WORKLOAD_PRIVATE_KEY"

// minRSAModulusBits is the floor this package enforces on every parsed or
// generated key.
const minRSAModulusBits = 2048

// generatedRSABits is the size GenerateKey produces.
const generatedRSABits = 2048

// ParsePrivateKey parses s as an RSA private key: a PKCS#8 PEM block (type
// "PRIVATE KEY"), or a single-line, standard-base64-encoded PKCS#8 DER
// document -- the form NormalizePrivateKey and EncodePrivateKey produce. A
// PKCS#1 PEM ("RSA PRIVATE KEY") is refused with conversion guidance rather
// than silently accepted. Every error names keyEnvName and never the input:
// on the base64 path there is nothing to parse into a structural error in the
// first place, and on the PEM/DER path the failures below are all
// package-authored strings, never a wrapped x509 error that could quote
// offending bytes back.
func ParsePrivateKey(s string) (*rsa.PrivateKey, error) {
	der, err := privateKeyDER(s)
	if err != nil {
		return nil, err
	}
	key, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, fmt.Errorf("%s: not a valid PKCS#8 document", keyEnvName)
	}
	rsaKey, ok := key.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("%s: must be an RSA key", keyEnvName)
	}
	if bits := rsaKey.N.BitLen(); bits < minRSAModulusBits {
		return nil, fmt.Errorf("%s: RSA modulus is %d bits, must be at least %d", keyEnvName, bits, minRSAModulusBits)
	}
	return rsaKey, nil
}

// privateKeyDER resolves s to raw PKCS#8 DER bytes, or a keyEnvName-labelled
// error that never echoes s.
func privateKeyDER(s string) ([]byte, error) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return nil, fmt.Errorf("%s: empty", keyEnvName)
	}
	if block, _ := pem.Decode([]byte(trimmed)); block != nil {
		switch block.Type {
		case "PRIVATE KEY":
			return block.Bytes, nil
		case "RSA PRIVATE KEY":
			return nil, fmt.Errorf(
				"%s: PEM block is PKCS#1 (\"RSA PRIVATE KEY\"), which this client does not accept; "+
					"convert to PKCS#8 first, e.g. `openssl pkcs8 -topk8 -nocrypt -in key.pem -out key8.pem`",
				keyEnvName)
		default:
			return nil, fmt.Errorf("%s: unsupported PEM block type %q; expected PKCS#8 (\"PRIVATE KEY\")", keyEnvName, block.Type)
		}
	}
	der, err := base64.StdEncoding.DecodeString(trimmed)
	if err != nil {
		return nil, fmt.Errorf("%s: not a PEM block or standard-base64 PKCS#8 DER", keyEnvName)
	}
	return der, nil
}

// EncodePrivateKey returns k as a single-line, standard-base64-encoded PKCS#8
// DER document -- the canonical form this package's cache and config layers
// store.
func EncodePrivateKey(k *rsa.PrivateKey) (string, error) {
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		return "", fmt.Errorf("%s: marshal PKCS#8: %w", keyEnvName, err)
	}
	return base64.StdEncoding.EncodeToString(der), nil
}

// NormalizePrivateKey parses s in either accepted form and re-encodes it as
// the canonical single-line base64 PKCS#8 DER form, so a PEM the operator
// pastes in and the form this package caches are always the same shape.
func NormalizePrivateKey(s string) (string, error) {
	key, err := ParsePrivateKey(s)
	if err != nil {
		return "", err
	}
	return EncodePrivateKey(key)
}

// GenerateKey generates a fresh 2048-bit RSA key.
func GenerateKey() (*rsa.PrivateKey, error) {
	key, err := rsa.GenerateKey(rand.Reader, generatedRSABits)
	if err != nil {
		return nil, fmt.Errorf("generating RSA key: %w", err)
	}
	return key, nil
}
