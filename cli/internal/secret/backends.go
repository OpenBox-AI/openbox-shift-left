package secret

import (
	"bytes"
	"errors"
	"os/exec"
	"strings"
)

// --- Linux: libsecret via `secret-tool` -------------------------------------
//
// secret-tool reads the secret value from STDIN on store, so the value never
// appears on the process argv (INV-1). Lookup prints the value to stdout with
// no trailing newline; a missing item exits non-zero with empty stdout.

type secretToolStore struct{ bin string }

func detectSecretTool() (Store, bool) {
	if p, err := exec.LookPath("secret-tool"); err == nil {
		return &secretToolStore{bin: p}, true
	}
	return nil, false
}

func (s *secretToolStore) Name() string { return "libsecret (secret-tool)" }

func (s *secretToolStore) Set(service, account, value string) error {
	// --label is a human-facing description only; it carries no secret.
	cmd := exec.Command(s.bin, "store", "--label="+service+" / "+account,
		"service", service, "account", account)
	cmd.Stdin = strings.NewReader(value)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return errors.New("secret-tool store failed: " + strings.TrimSpace(stderr.String()))
	}
	return nil
}

func (s *secretToolStore) Get(service, account string) (string, error) {
	cmd := exec.Command(s.bin, "lookup", "service", service, "account", account)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		// secret-tool exits non-zero (no stdout) when the item is absent.
		if stdout.Len() == 0 {
			return "", ErrNotFound
		}
		return "", errors.New("secret-tool lookup failed")
	}
	return stdout.String(), nil
}

func (s *secretToolStore) Delete(service, account string) error {
	cmd := exec.Command(s.bin, "clear", "service", service, "account", account)
	// clear is a no-op (exit 0) when nothing matches; treat any error as soft.
	_ = cmd.Run()
	return nil
}

// --- macOS: keychain via `security` -----------------------------------------
//
// CAVEAT (routed to G_SEC / Sam): the `security add-generic-password` CLI takes
// the secret with `-w <value>` on argv, so the value is briefly visible via
// `ps` during the call — unlike the Linux stdin path. macOS has no no-cgo,
// no-argv path through the `security` binary. Options for the security review:
// (a) accept the transient local-only exposure, or (b) ship a small keychain
// helper. Phase-1 build implements (a) and flags it. INV-1's persistent
// guarantee (never at rest in repo/logs) still holds.

type keychainStore struct{ bin string }

func detectKeychain() (Store, bool) {
	if p, err := exec.LookPath("security"); err == nil {
		return &keychainStore{bin: p}, true
	}
	return nil, false
}

func (s *keychainStore) Name() string { return "macOS keychain (security)" }

func (s *keychainStore) Set(service, account, value string) error {
	// -U updates an existing item in place (idempotent re-init).
	cmd := exec.Command(s.bin, "add-generic-password", "-U",
		"-s", service, "-a", account, "-w", value)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return errors.New("security add-generic-password failed: " + strings.TrimSpace(stderr.String()))
	}
	return nil
}

func (s *keychainStore) Get(service, account string) (string, error) {
	cmd := exec.Command(s.bin, "find-generic-password", "-s", service, "-a", account, "-w")
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return "", ErrNotFound
	}
	// -w prints the password followed by a newline.
	return strings.TrimRight(stdout.String(), "\n"), nil
}

func (s *keychainStore) Delete(service, account string) error {
	cmd := exec.Command(s.bin, "delete-generic-password", "-s", service, "-a", account)
	_ = cmd.Run()
	return nil
}
