package gitaction

import (
	"strconv"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/client"
)

// DeployMeta is the deploy-context the action stamps onto the Deploy event
// (beyond what it resolves from git).
type DeployMeta struct {
	Repo         string // e.g. "openbox-ai/openbox-shift-left" (GITHUB_REPOSITORY)
	Environment  string // e.g. "production"; "" => omitted
	DeveloperDID string // the git-action agent's REAL did:aip:<uuid> (signing identity)
}

// BuildDeployEvent maps a Resolution + deploy context onto the normalized
// DevEvent the client emits (contract event_type = Deploy).
//   - The signing identity (client.Config.DID) is the agent's real
//     did:aip:<uuid>; core validates it.
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
		// Dropping it here silently pins every link to verified:false, so it must
		// ride along whenever the resolver managed to attach one.
		if s.Attestation != nil {
			m["attestation"] = s.Attestation
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
