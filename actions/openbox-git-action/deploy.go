package gitaction

import (
	"strconv"
	"time"

	"github.com/openbox-ai/openbox-shift-left/client"
)

// DeployMeta is the deploy-context the action stamps onto the Deploy event
// (beyond what it resolves from git).
type DeployMeta struct {
	Repo         string // e.g. "openbox-ai/openbox-shift-left" (GITHUB_REPOSITORY)
	Environment  string // e.g. "production"; "" => omitted
	DeveloperDID string // the git-action agent's REAL did:aip:<uuid> (signing identity)
}

// BuildDeployEvent maps a Resolution + deploy context onto the normalized
// DevEvent the client emits (contract event_type = Deploy). The resolved
// session set and the attribution outcome ride in metadata: no external
// schema is needed to write the link; a queryable session->commit->deploy
// JOIN is external/deferred.
//
// Identity/lineage:
//   - The signing identity (client.Config.DID) is the agent's real
//     did:aip:<uuid> — core validates it. DeveloperDID here is that same
//     value.
//   - deploy_did is a synthetic lineage label
//     (`did:aip:deploy-<shortsha>-<unix>`) carried only in metadata; core
//     has no deploy-DID primitive, so it is never sent as the signing DID.
//
// Idempotency (INV-5): event_id == deploy_id == `deploy-<env>-<full-sha>` is
// stable across CI re-runs of the same artifact (full SHA, so two commits
// with a shared short prefix never collide), and run_id == deploy_id is
// stable too, so a retried deploy of the same commit to the same
// environment resolves to the same core session rather than a new one and
// is not double-counted once core dedupes on metadata.event_id. deploy_did
// (metadata) carries the wall-clock timestamp as the lineage label and so
// legitimately varies per run.
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
		// Fully-qualified claims (each with verified/source/reason). This is the
		// authoritative session record — consumers MUST read `verified` here.
		"sessions": sessions,
		// Verified ids only: the flat convenience list a future lineage JOIN
		// would bind to must never carry an unverified/forged claim
		// stripped of its qualifier. Empty with the default NoopVerifier —
		// honestly reflecting that nothing is proven yet.
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
		// run_id: the stable deploy id (deploy-<env>-<full-sha>) — a Deploy is
		// not itself inside one developer session, and a multi-session
		// fan-in must not be collapsed into one run_id. Being stable across
		// re-runs (unlike the timestamped deploy_did) makes (workflow_id,
		// run_id) idempotent per artifact. The resolved developer sessions
		// live in metadata.
		SessionID:    deployID,
		DeveloperDID: meta.DeveloperDID,
		WorkspaceID:  meta.Repo, // workflow_id groups deploys by repo ("" => DID fallback)
		Timestamp:    ts.Format(time.RFC3339),
		Tool:         client.Tool{Name: "openbox-git-action", Kind: client.ToolShell},
		Metadata:     md,
	}
}

// deployIDFor builds the stable, idempotent, collision-free deploy id. It
// uses the full commit SHA: a 7-hex prefix would let two distinct commits
// share one event_id and be merged by a future event_id dedupe.
func deployIDFor(environment, sha string) string {
	env := environment
	if env == "" {
		env = "unspecified"
	}
	return "deploy-" + env + "-" + sha
}
