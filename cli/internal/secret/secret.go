// Package secret is the OpenBox CLI's OS-backed secret store (INV-1).
// Developer-agent credentials — the obx_ API key and the Ed25519 signing
// key — are written here, never to plaintext files in a repo, to shell
// history, or onto a process argv (visible via `ps`).
//
// The store is a minimal (service, account) -> opaque-string map. Callers keep
// only a reference (service+account) in config; the value itself lives in the
// platform keychain.
//
// Backends (selected at runtime by GOOS + tool availability):
//   - linux:  libsecret via `secret-tool` — secret passed on STDIN (INV-1 clean)
//   - darwin: macOS keychain via `security` — see the argv caveat on keychainStore
//   - other:  none; Detect returns ErrNoStore so the caller HALTs (INV-1)
package secret

import (
	"errors"
	"fmt"
	"runtime"
)

// Service is the keychain service namespace under which the CLI stores every
// developer-agent credential. Accounts within it are of the form
// "<organization_id>/<provider>/<field>".
const Service = "ai.openbox.dev"

// ErrNotFound is returned by Get when no secret exists for (service, account).
var ErrNotFound = errors.New("secret: not found")

// ErrNoStore is returned by Detect when the platform has no usable OS secret
// store. Per INV-1 the CLI HALTs rather than fall back to plaintext.
var ErrNoStore = errors.New("secret: no OS secret store available on this platform")

// Store is an OS-backed secret store. Implementations MUST NOT place a secret
// value on a process argv or emit it to logs (INV-1).
type Store interface {
	// Name identifies the backend for diagnostics. It never returns a secret.
	Name() string
	// Set stores (or replaces) the secret for (service, account).
	Set(service, account, value string) error
	// Get returns the stored secret, or ErrNotFound if absent.
	Get(service, account string) (string, error)
	// Delete removes the secret for (service, account); absent is not an error.
	Delete(service, account string) error
}

// Detect returns the OS secret store for the current platform, or ErrNoStore
// if none is available (a HALT condition for credential-writing commands).
func Detect() (Store, error) {
	switch runtime.GOOS {
	case "linux":
		if s, ok := detectSecretTool(); ok {
			return s, nil
		}
	case "darwin":
		if s, ok := detectKeychain(); ok {
			return s, nil
		}
	}
	return nil, ErrNoStore
}

// Open selects a secret backend by name:
//
//	"" | "auto" | "os" — the OS keychain (Detect); ErrNoStore if none. Safe default.
//	"file"             — the opt-in 0600 file backend (plaintext at rest). The
//	                     caller must warn the user before using it.
//
// Note "auto" never silently falls back to the file backend: falling back would
// store plaintext without consent, which is exactly what the HALT prevents.
func Open(kind string) (Store, error) {
	switch kind {
	case "", "auto", "os":
		return Detect()
	case "file":
		return NewFileStore(DefaultFilePath()), nil
	default:
		return nil, fmt.Errorf("%w %q (use os|file)", ErrUnknownBackend, kind)
	}
}
