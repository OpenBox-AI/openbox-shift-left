package secret

import (
	"os/exec"
	"testing"
)

// storeContract exercises the Store interface against any backend.
func storeContract(t *testing.T, s Store) {
	t.Helper()
	const svc, acct = "ai.openbox.dev.test", "org/claude-code/api_key"
	_ = s.Delete(svc, acct)

	if _, err := s.Get(svc, acct); err != ErrNotFound {
		t.Fatalf("Get on absent key = %v, want ErrNotFound", err)
	}
	const secretVal = "obx_test_deadbeef"
	if err := s.Set(svc, acct, secretVal); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := s.Get(svc, acct)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != secretVal {
		t.Fatalf("Get = %q, want %q", got, secretVal)
	}
	// Overwrite (idempotent re-init).
	if err := s.Set(svc, acct, "obx_test_v2"); err != nil {
		t.Fatalf("Set overwrite: %v", err)
	}
	if got, _ := s.Get(svc, acct); got != "obx_test_v2" {
		t.Fatalf("after overwrite Get = %q", got)
	}
	if err := s.Delete(svc, acct); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Get(svc, acct); err != ErrNotFound {
		t.Fatalf("Get after Delete = %v, want ErrNotFound", err)
	}
}

func TestMemStoreContract(t *testing.T) {
	storeContract(t, NewMemStore())
}

// TestSecretToolContract runs the interface contract against the real libsecret
// backend when secret-tool is available AND a session keyring is reachable.
// Skipped otherwise so `go test ./...` stays green in headless CI.
func TestSecretToolContract(t *testing.T) {
	if _, err := exec.LookPath("secret-tool"); err != nil {
		t.Skip("secret-tool not installed")
	}
	s := &secretToolStore{bin: "secret-tool"}
	// Probe: a store to a throwaway item; if the keyring is locked/absent this
	// errors and we skip rather than fail.
	if err := s.Set("ai.openbox.dev.probe", "probe", "x"); err != nil {
		t.Skipf("no reachable keyring: %v", err)
	}
	_ = s.Delete("ai.openbox.dev.probe", "probe")
	storeContract(t, s)
}

func TestMemStoreAccounts(t *testing.T) {
	m := NewMemStore()
	_ = m.Set(Service, "org/claude-code/api_key", "a")
	_ = m.Set(Service, "org/claude-code/did", "b")
	if got := m.Accounts(Service); len(got) != 2 {
		t.Fatalf("Accounts = %v, want 2 entries", got)
	}
}
