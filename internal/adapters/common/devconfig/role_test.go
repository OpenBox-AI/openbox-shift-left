package devconfig_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
)

func TestParseRoleDefaultsToDev(t *testing.T) {
	for _, in := range []string{"", "dev"} {
		got, err := devconfig.ParseRole(in)
		if err != nil || got != devconfig.RoleDev {
			t.Fatalf("ParseRole(%q) = %q, %v; want dev, nil", in, got, err)
		}
	}
	if got, err := devconfig.ParseRole("approver"); err != nil || got != devconfig.RoleApprover {
		t.Fatalf("ParseRole(approver) = %q, %v", got, err)
	}
	if _, err := devconfig.ParseRole("aprover"); err == nil {
		t.Fatal("ParseRole(aprover) = nil error; want a rejection")
	}
}

func TestConfigPathForKeepsTheRolesApart(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv(devconfig.EnvConfigPath, "")
	t.Setenv(devconfig.EnvApproverConfigPath, "")

	dev := devconfig.ConfigPathFor(devconfig.RoleDev)
	approver := devconfig.ConfigPathFor(devconfig.RoleApprover)
	if dev == approver {
		t.Fatalf("both roles resolve to %s — one principal's config would overwrite the other's", dev)
	}
	if got, want := filepath.Base(dev), "dev.json"; got != want {
		t.Errorf("dev config = %s, want %s", got, want)
	}
	if got, want := filepath.Base(approver), "approver.json"; got != want {
		t.Errorf("approver config = %s, want %s", got, want)
	}
}

func TestApproverConfigRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "approver.json")
	want := devconfig.ApproverConfig{
		BackendURL:     "http://localhost:3000",
		OrgID:          "acme.example",
		Host:           "claude-code",
		Shadow:         true,
		PollIntervalMS: 1000,
	}
	if err := devconfig.WriteApprover(path, want); err != nil {
		t.Fatalf("WriteApprover: %v", err)
	}
	got, err := devconfig.LoadApprover(path)
	if err != nil {
		t.Fatalf("LoadApprover: %v", err)
	}
	if got != want {
		t.Errorf("round trip changed the config:\n got %+v\nwant %+v", got, want)
	}

	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Errorf("approver config mode = %v, want 0600", perm)
	}

	if _, err := devconfig.LoadApprover(filepath.Join(t.TempDir(), "absent.json")); err != nil {
		t.Errorf("LoadApprover(absent) = %v; want no error", err)
	}
}
