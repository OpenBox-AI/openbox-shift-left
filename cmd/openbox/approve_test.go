package main

import (
	"bytes"
	"encoding/json"
	"net/http"

	"github.com/openbox-ai/openbox-shift-left/internal/client/memhttptest"
	"os"
	"strings"
	"testing"
	"time"
)

const testOrgKey = "obx_key_secretorgkey"

func approveBackend(t *testing.T, pending []map[string]any) (*memhttptest.Server, *[]string) {
	t.Helper()
	var decided []string
	srv := memhttptest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-API-Key"); got != testOrgKey {
			t.Errorf("X-API-Key = %q — the approver acts under its own org credential", got)
		}
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/approvals"):
			if got := r.URL.Query().Get("status"); got != "pending" {
				t.Errorf("status filter = %q, want pending", got)
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{
					"approvals": map[string]any{"data": pending, "total": len(pending)},
				},
			})
		case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/decide"):
			decided = append(decided, r.URL.Path+"?"+r.URL.RawQuery)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":{}}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &decided
}

func approveApp(t *testing.T, srv *memhttptest.Server) (*app, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	t.Setenv("OPENBOX_CONFIG", t.TempDir()+"/none.json")
	t.Setenv("OPENBOX_CONTROL_TOKEN", testOrgKey)
	t.Setenv("OPENBOX_BACKEND_URL", srv.URL)
	var out, errb bytes.Buffer
	return &app{stdout: &out, stderr: &errb, getenv: os.Getenv}, &out, &errb
}

func pendingItem(id, agentID, agentName, reason string, expiresIn time.Duration) map[string]any {
	return map[string]any{
		"id":            id,
		"agent_id":      agentID,
		"agent":         map[string]any{"id": agentID, "agent_name": agentName},
		"activity_type": "Bash",
		"input":         map[string]any{"kind": "shell", "tool_name": "Bash", "command": "rm -rf /tmp/x"},
		"reason":        reason,

		"approval_expired_at": time.Now().Add(expiresIn).Format(time.RFC3339),
		"created_at":          time.Now().Add(-time.Minute).Format(time.RFC3339),
	}
}

func TestApproveList(t *testing.T) {
	srv, _ := approveBackend(t, []map[string]any{
		pendingItem("ge-1", "agent-1", "dev-brian", "production shell command", 25*time.Minute),
	})
	a, out, _ := approveApp(t, srv)

	if code := a.run([]string{"approve", "list", "--org", "org-1"}); code != exitOK {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, out.String())
	}
	for _, want := range []string{
		"ge-1", "dev-brian", "production shell command", "openbox approve allow ge-1",
		"Bash",          // the tool — nested in the wire, absent from the DTO
		"kind=shell",    // structural context
		"rm -rf /tmp/x", // THE thing the approval is decided on
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("listing missing %q:\n%s", want, out.String())
		}
	}
	if !strings.Contains(out.String(), "request  rm -rf /tmp/x") {
		t.Errorf("the decisive field is not surfaced on its own line:\n%s", out.String())
	}
}

// TestApproveList_SaysWhenARequestIsUndecidable with no command or arguments
// there is nothing to judge, and the listing must say so.
func TestApproveList_SaysWhenARequestIsUndecidable(t *testing.T) {
	item := pendingItem("ge-1", "agent-1", "dev-brian", "shell needs approval", 25*time.Minute)
	item["input"] = map[string]any{"kind": "shell", "tool_name": "Bash"} // no command
	srv, _ := approveBackend(t, []map[string]any{item})
	a, out, _ := approveApp(t, srv)

	if code := a.run([]string{"approve", "list", "--org", "org-1"}); code != exitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "not captured") {
		t.Errorf("a request with no command must be marked as such:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "rubber stamp") {
		t.Errorf("the listing must name the consequence, not just the gap:\n%s", out.String())
	}
}

func TestApproveListEmpty(t *testing.T) {
	srv, _ := approveBackend(t, nil)
	a, out, _ := approveApp(t, srv)

	if code := a.run([]string{"approve", "list", "--org", "org-1"}); code != exitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "No pending approvals") {
		t.Errorf("empty queue should say so, got %q", out.String())
	}
}

// TestApproveAllowResolvesTheAgent the approver types only the id they read
// off the list; the CLI resolves the owning agent, because the decide route is
// per-agent.
func TestApproveAllowResolvesTheAgent(t *testing.T) {
	srv, decided := approveBackend(t, []map[string]any{
		pendingItem("ge-1", "agent-77", "dev-brian", "production shell command", 25*time.Minute),
	})
	a, out, errb := approveApp(t, srv)

	if code := a.run([]string{"approve", "allow", "ge-1", "--org", "org-1"}); code != exitOK {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, errb.String())
	}
	if len(*decided) != 1 {
		t.Fatalf("decide calls = %v, want exactly one", *decided)
	}
	got := (*decided)[0]
	if !strings.Contains(got, "/agent/agent-77/approvals/ge-1/decide") || !strings.Contains(got, "action=approve") {
		t.Errorf("decide call = %q, want the owning agent and action=approve", got)
	}
	if !strings.Contains(out.String(), "Approved ge-1") {
		t.Errorf("output should confirm the decision, got %q", out.String())
	}
}

func TestApproveDenySendsReject(t *testing.T) {
	srv, decided := approveBackend(t, []map[string]any{
		pendingItem("ge-1", "agent-77", "dev-brian", "reason", 25*time.Minute),
	})
	a, _, errb := approveApp(t, srv)

	if code := a.run([]string{"approve", "deny", "ge-1", "--org", "org-1"}); code != exitOK {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, errb.String())
	}
	if len(*decided) != 1 || !strings.Contains((*decided)[0], "action=reject") {
		t.Errorf("decide calls = %v, want one action=reject", *decided)
	}
}

// TestApproveRefusesAnExpiredRequest an expired request cannot be decided; the
// server would refuse it, and pretending otherwise would tell an approver they
// had answered something they had not.
func TestApproveRefusesAnExpiredRequest(t *testing.T) {
	srv, decided := approveBackend(t, []map[string]any{
		pendingItem("ge-old", "agent-77", "dev-brian", "reason", -time.Minute),
	})
	a, _, errb := approveApp(t, srv)

	if code := a.run([]string{"approve", "allow", "ge-old", "--org", "org-1"}); code == exitOK {
		t.Error("deciding an expired request must fail")
	}
	if len(*decided) != 0 {
		t.Errorf("sent %v — an expired request must not reach the decide route", *decided)
	}
	if !strings.Contains(errb.String(), "expired") {
		t.Errorf("error should say the window closed, got %q", errb.String())
	}
}

func TestApproveRejectsAnUnknownEvent(t *testing.T) {
	srv, decided := approveBackend(t, nil)
	a, _, _ := approveApp(t, srv)

	if code := a.run([]string{"approve", "allow", "ge-missing", "--org", "org-1"}); code == exitOK {
		t.Error("deciding an event that is not pending must fail")
	}
	if len(*decided) != 0 {
		t.Errorf("sent %v for an unknown event", *decided)
	}
}

// TestApproveRequiresTheControlTokenFromTheEnvironment iNV-1: the approver's
// credential comes from the environment, never a flag, so it cannot leak
// through argv or shell history.
func TestApproveRequiresTheControlTokenFromTheEnvironment(t *testing.T) {
	t.Setenv("OPENBOX_CONFIG", t.TempDir()+"/none.json")
	t.Setenv("OPENBOX_CONTROL_TOKEN", "")
	t.Setenv("OPENBOX_BACKEND_URL", "https://backend.example")
	var out, errb bytes.Buffer
	a := &app{stdout: &out, stderr: &errb, getenv: os.Getenv}

	if code := a.run([]string{"approve", "list", "--org", "org-1"}); code == exitOK {
		t.Error("approve must refuse to run without a control credential")
	}
	if !strings.Contains(errb.String(), "OPENBOX_CONTROL_TOKEN") {
		t.Errorf("error should name the env var, got %q", errb.String())
	}
	if strings.Contains(errb.String(), "--token") {
		t.Error("the credential must never be a flag (INV-1)")
	}
}
