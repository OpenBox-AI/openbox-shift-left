package gitaction

import (
	"strconv"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/client"
)

// DeployMeta is the deploy-context the action stamps onto the Deploy event
// (beyond what it resolves from git).
type DeployMeta struct {
	Repo        string // e.g. "openbox-ai/openbox-shift-left" (GITHUB_REPOSITORY)
	Environment string // e.g. "production"; "" => omitted
	// DeveloperDID is the git-action agent's attribution label,
	// devconfig.AttributionDIDFor(OPENBOX_AGENT_ID) -- NOT a signing identity.
	// A v3 workload agent authenticates with its API key and workload bearer
	// (internal/client's Authorization/X-OpenBox-Workload-Token headers); this
	// DID is a derived coordinate carried only as the event's workflow_id, for
	// attribution and grouping, and is never itself proof of anything.
	DeveloperDID string
}

// BuildDeployEvent maps a Resolution + deploy context onto the normalized
// DevEvent the client emits (contract event_type = Deploy).
//   - DeveloperDID is the derived attribution label, never a signing
//     identity: core validates the request via the workload bearer, not via
//     this value.
//   - Deploy_did is a synthetic lineage label
//     (`did:aip:deploy-<shortsha>-<unix>`) carried only in metadata; core has
//     no deploy-DID primitive, so it is never sent as the signing DID.
func BuildDeployEvent(res Resolution, meta DeployMeta, now time.Time) client.DevEvent {
	ts := now.UTC()
	deployDID := "did:aip:deploy-" + short(res.CommitSHA) + "-" + strconv.FormatInt(ts.Unix(), 10)
	deployID := deployIDFor(meta.Environment, res.CommitSHA)

	sessions := make([]map[string]any, 0, len(res.Sessions))
	verifiedIDs := make([]string, 0, len(res.Sessions))
	for _, s := range res.Sessions {
		m := map[string]any{
			"session_id": s.SessionID,
			"source":     string(s.Source),
			"commit":     s.Commit,
			"verified":   s.Verified,
		}
		if s.Reason != "" {
			m["reason"] = s.Reason
		}
		sessions = append(sessions, m)
		if s.Verified {
			verifiedIDs = append(verifiedIDs, s.SessionID)
		}
	}

	md := map[string]any{
		"deploy_id":          deployID,
		"deploy_did":         deployDID,
		"commit_sha":         res.CommitSHA,
		"attribution_status": string(res.Status),
		"session_count":      len(res.Sessions),
		// This is the authoritative session record; consumers must read `verified`
		// here.
		"sessions":             sessions,
		"verified_session_ids": verifiedIDs,
		"scope_walked":         res.ScopeWalked,
		"scope_total":          res.ScopeTotal,
	}
	if meta.Repo != "" {
		md["repo"] = meta.Repo
	}
	if meta.Environment != "" {
		md["environment"] = meta.Environment
	}
	if res.Reason != "" {
		md["attribution_reason"] = string(res.Reason)
	}
	// Deliberately NOT a contentMetadataKeys entry, and not an oversight: every
	// note is a sentence this repo wrote (resolve.go's appendNote call sites),
	// with only counts interpolated. No commit body, no file text, nothing the
	// developer typed. Derived evidence, like credential_fingerprint -- gating
	// it would drop the explanation of an unattributed deploy exactly when an
	// operator needs it. Keep it that way: never append user-supplied text here.
	// As of v1.9 the note also rides signal_args, so it is displayed, not merely
	// stored.
	if res.Note != "" {
		md["attribution_note"] = res.Note
	}

	return client.DevEvent{
		SchemaVersion: client.SchemaVersion,
		EventID:       deployID, // INV-5 idempotency key
		EventType:     client.EventDeploy,
		SessionID:     deployID,
		DeveloperDID:  meta.DeveloperDID,
		WorkspaceID:   meta.Repo, // workflow_id groups deploys by repo ("" => DID fallback)
		Timestamp:     ts.Format(time.RFC3339),
		Tool:          client.Tool{Name: "openbox-git-action", Kind: client.ToolShell},
		Metadata:      md,
	}
}

func deployIDFor(environment, sha string) string {
	env := environment
	if env == "" {
		env = "unspecified"
	}
	return "deploy-" + env + "-" + sha
}
