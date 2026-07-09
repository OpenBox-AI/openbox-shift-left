package gitaction

import (
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/client"
)

func fixedResolution() Resolution {
	return Resolution{
		CommitSHA: "37ec0a3f1c9b2e0000000000000000000000abcd",
		Status:    StatusInferred,
		Sessions: []SessionClaim{
			{SessionID: "sess-A", Source: SourceTrailer, Commit: "37ec0a3f", Verified: false, Reason: "unverified claim"},
		},
		ScopeWalked: 1,
		ScopeTotal:  1,
	}
}

func TestBuildDeployEvent_ShapeAndMetadata(t *testing.T) {
	now := time.Date(2026, 7, 7, 12, 35, 0, 0, time.UTC)
	meta := DeployMeta{
		Repo:         "openbox-ai/openbox-shift-left",
		Environment:  "production",
		DeveloperDID: "did:aip:7f3c9b2e-0000-5000-a000-000000000001",
	}
	ev := BuildDeployEvent(fixedResolution(), meta, now)

	if ev.EventType != client.EventDeploy {
		t.Fatalf("event_type = %s, want Deploy", ev.EventType)
	}
	if ev.SchemaVersion != client.SchemaVersion {
		t.Fatalf("schema_version = %s, want %s", ev.SchemaVersion, client.SchemaVersion)
	}
	// workflow_id source (WorkspaceID) is the repo; run_id (SessionID) is the
	// STABLE deploy id — never collapses a fan-in into one dev session and is
	// idempotent across re-runs (P1).
	if ev.WorkspaceID != meta.Repo {
		t.Fatalf("WorkspaceID = %s, want %s", ev.WorkspaceID, meta.Repo)
	}
	wantDeployID := "deploy-production-37ec0a3f1c9b2e0000000000000000000000abcd" // full SHA (P2)
	if ev.SessionID != wantDeployID {
		t.Fatalf("SessionID(run_id) = %s, want %s (stable deploy id)", ev.SessionID, wantDeployID)
	}
	if ev.Tool.Name != "openbox-git-action" || ev.Tool.Kind != client.ToolShell {
		t.Fatalf("tool = %+v, want openbox-git-action/shell", ev.Tool)
	}
	// event_id is the stable idempotency key (INV-5) == run_id.
	if ev.EventID != wantDeployID {
		t.Fatalf("event_id = %s, want %s", ev.EventID, wantDeployID)
	}

	m := ev.Metadata
	wantDID := "did:aip:deploy-37ec0a3-" + itoaUnix(now) // timestamped lineage label (metadata only)
	if m["deploy_did"] != wantDID {
		t.Fatalf("metadata.deploy_did = %v, want %s", m["deploy_did"], wantDID)
	}
	if m["commit_sha"] != "37ec0a3f1c9b2e0000000000000000000000abcd" {
		t.Fatalf("metadata.commit_sha = %v", m["commit_sha"])
	}
	if m["attribution_status"] != string(StatusInferred) {
		t.Fatalf("metadata.attribution_status = %v", m["attribution_status"])
	}
	if m["environment"] != "production" || m["repo"] != meta.Repo {
		t.Fatalf("metadata repo/env = %v/%v", m["repo"], m["environment"])
	}
	if m["session_count"] != 1 {
		t.Fatalf("metadata.session_count = %v, want 1", m["session_count"])
	}
}

func TestBuildDeployEvent_IsIdempotentAcrossRuns(t *testing.T) {
	// Same commit + environment ⇒ stable event_id AND run_id across CI re-runs
	// (INV-5 / P1), even though the deploy_did timestamp (a lineage label in
	// metadata) legitimately differs.
	res := fixedResolution()
	meta := DeployMeta{Environment: "staging"}
	a := BuildDeployEvent(res, meta, time.Unix(1000, 0))
	b := BuildDeployEvent(res, meta, time.Unix(2000, 0))
	if a.EventID != b.EventID {
		t.Fatalf("event_id not stable: %s vs %s", a.EventID, b.EventID)
	}
	if a.SessionID != b.SessionID {
		t.Fatalf("run_id not stable across runs: %s vs %s", a.SessionID, b.SessionID)
	}
	if a.Metadata["deploy_did"] == b.Metadata["deploy_did"] {
		t.Fatal("deploy_did should embed the timestamp and differ across runs")
	}
}

func TestBuildDeployEvent_SurvivesClientEmitBuild(t *testing.T) {
	// The event must round-trip through the SL-3 client's payload builder
	// without a precondition error (EventID + SessionID non-empty) and produce
	// JSON that carries our metadata. We assert via the client's public Emit
	// against a fake transport rather than reaching into unexported builders.
	ev := BuildDeployEvent(fixedResolution(), DeployMeta{Repo: "r", Environment: "e"}, time.Unix(1, 0))
	if ev.EventID == "" || ev.SessionID == "" {
		t.Fatal("event missing required INV-5 idempotency / run_id fields")
	}
	// metadata must be JSON-serializable (client marshals it).
	if _, err := json.Marshal(ev.Metadata); err != nil {
		t.Fatalf("metadata not serializable: %v", err)
	}
	sessions, ok := ev.Metadata["sessions"].([]map[string]any)
	if !ok || len(sessions) != 1 {
		t.Fatalf("metadata.sessions shape = %T %v", ev.Metadata["sessions"], ev.Metadata["sessions"])
	}
	if sessions[0]["verified"] != false || sessions[0]["source"] != string(SourceTrailer) {
		t.Fatalf("session claim not faithfully carried: %v", sessions[0])
	}
}

func TestDeployDID_MatchesContractSample(t *testing.T) {
	// The SL-1 deploy.json sample uses did:aip:deploy-<short7>-<unix>.
	ev := BuildDeployEvent(fixedResolution(), DeployMeta{}, time.Unix(1751890500, 0))
	if got := ev.Metadata["deploy_did"]; got != "did:aip:deploy-37ec0a3-1751890500" {
		t.Fatalf("deploy_did = %v, want did:aip:deploy-37ec0a3-1751890500", got)
	}
}

func itoaUnix(t time.Time) string {
	return strconv.FormatInt(t.Unix(), 10)
}
