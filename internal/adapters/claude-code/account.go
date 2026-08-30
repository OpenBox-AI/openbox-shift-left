package claudecode

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
)

// account.go stamps the developer's provider ACCOUNT onto a session, so a stored
// session can be attributed to an org even where the gateway is not running (that
// decision, phase 05 requirement 6).
//
// Two independent sources exist by design. The gateway attaches a credential
// fingerprint per model call; this attaches the account the local client is
// signed in as, once per session. Either alone leaves a gap — the fingerprint
// identifies a credential without naming who holds it, and this names an account
// without proving it made any particular call.
//
// The evidence scope is EXACTLY org UUID + email, decided 2026-08-25. The same
// local record also exposes organizationName, organizationRole, organizationType,
// seatTier and billingType; none of those are bound. That restraint is the same
// allowlist discipline INV-2 applies to content, and widening it is a decision
// rather than a convenience — an org's rate-limit tier is not governance
// evidence.
//
// Honest limit, stated because a governance product that overstates itself is the
// failure it exists to prevent: this file is written by the client this product
// governs and is readable and writable by anything running as the developer — the
// posture that decision already concedes for the signing key. So it is evidence
// of origin-of-config, not a tamper-resistant account claim.

// accountStateFile is where Claude Code keeps its local account record. Verified
// on 2026-08-25 against the installed 2.1.229 (probe P1 §3).
const accountStateFile = ".claude.json"

// maxAccountStateBytes bounds the read. Generous — the file legitimately grows
// with project history — but finite. A truncated read simply fails to unmarshal
// and yields no evidence, which is the same silent no-op as a missing file.
const maxAccountStateBytes = 16 << 20 // 16 MiB

// accountEvidence is the bound subset. The struct IS the allowlist: a field that
// is not here cannot be egressed by this path, so adding one is a visible change
// rather than a silent widening.
type accountEvidence struct {
	Email   string
	OrgUUID string
}

// localAccount reads the account evidence from the developer's home directory.
//
// Every failure is silent and returns the zero value: a session must never fail
// to report because an optional attribution field was unreadable. Absence of
// account evidence is itself informative — it is what a machine that has never
// signed in looks like.
func localAccount(homeDir string) accountEvidence {
	if homeDir == "" {
		return accountEvidence{}
	}
	// BOUNDED, like every other externally-controlled read in this repo
	// (maxHookPayload 32MiB, maxTranscriptBytes 64MiB, maxFindingsDelta 4MiB). This
	// file is not ours: it accumulates project history, cached experiment payloads
	// and MCP settings, so it grows without an upper bound we control, and this
	// runs on EVERY SessionStart.
	f, err := os.Open(filepath.Join(homeDir, accountStateFile))
	if err != nil {
		return accountEvidence{}
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxAccountStateBytes))
	if err != nil {
		return accountEvidence{}
	}
	// Only the one object is decoded. The file also holds project history,
	// cached experiment payloads and MCP settings, none of which this path may
	// carry, so binding a narrow shape is the control rather than a convenience.
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

// accountMetadata renders the evidence as session metadata keys.
//
// METADATA, never signal_args. Core's alignment engine reads any SignalReceived
// with non-empty signal_args as a NEW USER GOAL and overwrites the session's goal
// with it (openbox-core internal/services/age.go:112-137) — so an email routed
// there would replace the developer's prompt as the thing every later turn is
// scored against. SessionStarted maps to WorkflowStarted rather than
// SignalReceived, so signal_args is not even in play on this event; the rule is
// restated because the next person to add an account field may well put it on one
// that is.
//
// The email is PII and egresses as governance evidence, like the DID.
// docs/data-and-privacy.md carries the row.
func accountMetadata(a accountEvidence) map[string]any {
	return compact(map[string]any{
		"account_email":    a.Email,
		"account_org_uuid": a.OrgUUID,
	})
}

// homeDir resolves the developer's home the same way the installer already does,
// so the account record is looked for where Claude Code actually wrote it.
func homeDir() string {
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		return h
	}
	return os.Getenv("HOME")
}
