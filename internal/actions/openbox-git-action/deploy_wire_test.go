package gitaction

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/client/fakecore"
)

// richResolution is what BuildDeployEvent actually produces in the field, as
// opposed to the three-key fixture the wire golden uses: an attributed deploy
// with a verified session claim, so the derived note is present.
func richResolution() Resolution {
	return Resolution{
		CommitSHA: "37ec0a3f1c9b2e0000000000000000000000abcd",
		Status:    StatusInferred,
		Reason:    ReasonTrailerStripped,
		Note:      "recovered from the git-notes mirror; commit trailer absent (likely a history rewrite)",
		Sessions: []SessionClaim{
			{SessionID: "sess-A", Source: SourceTrailer, Commit: "37ec0a3f", Verified: true, Reason: "owned by pusher"},
			{SessionID: "sess-B", Source: SourceNote, Commit: "37ec0a3f", Verified: false, Reason: "unverified claim"},
		},
		ScopeWalked: 2,
		ScopeTotal:  2,
	}
}

// TestDeployProjectsItsWholeMetadataIntoSignalArgs is the case the wire golden
// cannot be: `signal_deploy_lineage.json` is built from a hand-written
// three-key metadata map that happens to be a subset of the allowlist v1.9
// deleted, so it is byte-identical either way and proves nothing about the
// projection.
//
// The real producer emits far more — status, counts, the derived note, and
// nested per-session objects. All of it is now signal_args, which is what OPA
// and Guardrails read, so this asserts on the bytes that actually reach
// /evaluate rather than on the struct.
func TestDeployProjectsItsWholeMetadataIntoSignalArgs(t *testing.T) {
	fc := fakecore.New(t, fakecore.Script{})

	cl, err := client.New(client.Config{
		BaseURL:            fc.URL(),
		APIKey:             fakecore.APIKey(),
		WorkloadPrivateKey: fakecore.WorkloadPrivateKey(),
	})
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}

	ev := BuildDeployEvent(richResolution(), DeployMeta{
		Repo:         "openbox-ai/openbox-shift-left",
		Environment:  "production",
		DeveloperDID: "did:aip:7f3c9b2e-0000-5000-a000-000000000001",
	}, time.Date(2026, 7, 7, 12, 35, 0, 0, time.UTC))

	if _, err := cl.Emit(context.Background(), ev); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	inbox := fc.Inbox()
	if len(inbox) != 1 {
		t.Fatalf("expected 1 wire body, got %d", len(inbox))
	}

	var p struct {
		SignalName string         `json:"signal_name"`
		SignalArgs map[string]any `json:"signal_args"`
		Metadata   map[string]any `json:"metadata"`
	}
	if err := json.Unmarshal(inbox[0].Raw, &p); err != nil {
		t.Fatalf("decode wire body: %v\n%s", err, inbox[0].Raw)
	}
	if p.SignalName != "deploy" {
		t.Fatalf("signal_name = %q, want deploy", p.SignalName)
	}

	// The keys the deleted allowlist named must still be there — that is the
	// actual R5 evidence, and the golden could not supply it.
	for _, k := range []string{"deploy_id", "commit_sha", "repo", "environment", "deploy_did"} {
		if _, ok := p.SignalArgs[k]; !ok {
			t.Errorf("signal_args lost allowlisted lineage key %q: %v", k, p.SignalArgs)
		}
	}
	// And the keys it did NOT name, which is the point of deleting it: a deploy
	// policy can now match attribution, not just identity.
	for _, k := range []string{"attribution_status", "attribution_reason", "attribution_note",
		"session_count", "scope_walked", "scope_total", "sessions", "verified_session_ids"} {
		if _, ok := p.SignalArgs[k]; !ok {
			t.Errorf("signal_args is missing %q, which the allowlist used to drop: %v", k, p.SignalArgs)
		}
	}

	// Projected, not moved: metadata is untouched (R4).
	for k := range p.SignalArgs {
		if _, kept := p.Metadata[k]; !kept {
			t.Errorf("signal_args[%q] is not in metadata; the projection moved a key instead of "+
				"copying it, and the SQL forensics path reads metadata", k)
		}
	}

	// Nested objects survive as objects. Worth pinning because core's Guardrails
	// extract every string leaf under input.**, so a deploy row's per-session
	// claims are now scanned by whatever guards an org has configured.
	sessions, ok := p.SignalArgs["sessions"].([]any)
	if !ok || len(sessions) != 2 {
		t.Fatalf("signal_args.sessions = %T %v, want 2 nested objects",
			p.SignalArgs["sessions"], p.SignalArgs["sessions"])
	}
	first, ok := sessions[0].(map[string]any)
	if !ok || first["session_id"] != "sess-A" {
		t.Errorf("nested session claim did not survive the projection: %v", sessions[0])
	}

	// R3: the client signs nothing, so no sessions[] entry carries an
	// attestation key, verified or not.
	for _, raw := range sessions {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if _, has := entry["attestation"]; has {
			t.Errorf("sessions[] entry carries an attestation key, which the client no longer produces: %v", entry)
		}
	}
}

// TestDeployMetadataIsNotContentGated. A deploy carries no Content, so the
// content gate has nothing to strip — its whole payload rides regardless of
// content_capture, exactly as it did in metadata before v1.9. Pinned because
// the projection made these keys UI-visible, and "does capture off hide them?"
// is the first question a reader will have. It does not, and that is not a
// regression: attribution evidence is structural by design.
func TestDeployMetadataIsNotContentGated(t *testing.T) {
	ev := BuildDeployEvent(richResolution(), DeployMeta{Repo: "r", Environment: "e"}, time.Unix(1, 0))
	if ev.Content != nil {
		t.Fatalf("a deploy event carries Content = %+v; it never has, and the gate assumptions here "+
			"assume it does not", ev.Content)
	}
	note, _ := ev.Metadata["attribution_note"].(string)
	if !strings.Contains(note, "git-notes mirror") {
		t.Errorf("attribution_note = %q, want the derived explanation", note)
	}
}
