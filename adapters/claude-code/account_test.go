package claudecode

import (
	"os"
	"path/filepath"
	"testing"
)

// writeAccountFile lays down a .claude.json shaped like the real one, including
// the sibling keys that must NOT be egressed.
func writeAccountFile(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, accountStateFile), []byte(body), 0o600); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}
}

// TestLocalAccountBindsOnlyOrgUUIDAndEmail is the allowlist control. The real
// record exposes name, role, type, tiers and billing alongside the two bound
// fields; the decided evidence scope is org UUID + email and nothing more, so a
// widening has to fail here rather than ship quietly.
func TestLocalAccountBindsOnlyOrgUUIDAndEmail(t *testing.T) {
	dir := t.TempDir()
	writeAccountFile(t, dir, `{
	  "oauthAccount": {
	    "emailAddress": "dev@example.com",
	    "organizationUuid": "11111111-2222-3333-4444-555555555555",
	    "organizationName": "Example Corp",
	    "organizationRole": "admin",
	    "organizationType": "enterprise",
	    "seatTier": "premium",
	    "billingType": "invoice",
	    "userRateLimitTier": "high"
	  },
	  "projects": {"/some/path": {"history": ["a secret prompt"]}}
	}`)

	got := localAccount(dir)
	if got.Email != "dev@example.com" {
		t.Errorf("Email = %q", got.Email)
	}
	if got.OrgUUID != "11111111-2222-3333-4444-555555555555" {
		t.Errorf("OrgUUID = %q", got.OrgUUID)
	}

	// The rendered metadata is the egress surface: exactly two keys, and nothing
	// from the rest of a file that also holds the developer's prompt history.
	meta := accountMetadata(got)
	if len(meta) != 2 {
		t.Errorf("metadata has %d keys, want exactly 2: %v", len(meta), meta)
	}
	for _, banned := range []string{"Example Corp", "admin", "enterprise", "premium", "invoice", "high", "a secret prompt"} {
		for k, v := range meta {
			if s, ok := v.(string); ok && s == banned {
				t.Errorf("metadata[%s] egressed an unbound field: %q", k, banned)
			}
		}
	}
}

// TestLocalAccountFailsSilently keeps an optional attribution field from ever
// stopping a session from reporting. Absence is itself informative — it is what a
// machine that never signed in looks like.
func TestLocalAccountFailsSilently(t *testing.T) {
	cases := map[string]func(t *testing.T) string{
		"no home": func(*testing.T) string { return "" },
		"missing file": func(t *testing.T) string {
			return t.TempDir()
		},
		"unparseable json": func(t *testing.T) string {
			d := t.TempDir()
			writeAccountFile(t, d, `{not json`)
			return d
		},
		"no oauthAccount key": func(t *testing.T) string {
			d := t.TempDir()
			writeAccountFile(t, d, `{"projects":{}}`)
			return d
		},
		"empty values": func(t *testing.T) string {
			d := t.TempDir()
			writeAccountFile(t, d, `{"oauthAccount":{"emailAddress":"","organizationUuid":""}}`)
			return d
		},
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			got := localAccount(mk(t))
			if got.Email != "" || got.OrgUUID != "" {
				t.Errorf("expected zero evidence, got %+v", got)
			}
			// compact drops empties, so nothing is stamped at all rather than
			// two empty keys.
			if meta := accountMetadata(got); len(meta) != 0 {
				t.Errorf("expected no metadata keys, got %v", meta)
			}
		})
	}
}

// TestAccountMetadataKeysAreStable pins the two key names. They are what a core
// query joins on, so a rename is a contract change and should not be reachable by
// an incidental edit.
func TestAccountMetadataKeysAreStable(t *testing.T) {
	meta := accountMetadata(accountEvidence{Email: "a@b.c", OrgUUID: "org-1"})
	if meta["account_email"] != "a@b.c" {
		t.Errorf("account_email = %v", meta["account_email"])
	}
	if meta["account_org_uuid"] != "org-1" {
		t.Errorf("account_org_uuid = %v", meta["account_org_uuid"])
	}
	// Never signal_args-shaped: this is a flat metadata map, and core reads a
	// SignalReceived's signal_args as a NEW USER GOAL.
	if _, bad := meta["signal_args"]; bad {
		t.Error("account evidence must never render into signal_args")
	}
}
