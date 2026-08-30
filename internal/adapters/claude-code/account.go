package claudecode

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
)

const accountStateFile = ".claude.json"

const maxAccountStateBytes = 16 << 20 // 16 MiB

// accountEvidence the struct IS the allowlist: a field that is not here cannot
// be egressed by this path, so adding one is a visible change rather than a
// silent widening.
type accountEvidence struct {
	Email   string
	OrgUUID string
}

// localAccount every failure is silent and returns the zero value: a session
// must never fail to report because an optional attribution field was
// unreadable.
func localAccount(homeDir string) accountEvidence {
	if homeDir == "" {
		return accountEvidence{}
	}
	f, err := os.Open(filepath.Join(homeDir, accountStateFile))
	if err != nil {
		return accountEvidence{}
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxAccountStateBytes))
	if err != nil {
		return accountEvidence{}
	}
	var doc struct {
		OAuthAccount struct {
			EmailAddress     string `json:"emailAddress"`
			OrganizationUUID string `json:"organizationUuid"`
		} `json:"oauthAccount"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return accountEvidence{}
	}
	return accountEvidence{
		Email:   capStr(doc.OAuthAccount.EmailAddress),
		OrgUUID: capStr(doc.OAuthAccount.OrganizationUUID),
	}
}

// accountMetadata metadata, never signal_args.
func accountMetadata(a accountEvidence) map[string]any {
	return compact(map[string]any{
		"account_email":    a.Email,
		"account_org_uuid": a.OrgUUID,
	})
}

func homeDir() string {
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		return h
	}
	return os.Getenv("HOME")
}
