package muse

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/client/fakecore"
)

// Every gated event, against every verdict core can answer, renders an answer
// inside its own closed key set, never carries a shape Muse rejects or reads as
// a grant, and never leaks the tool's command into the local answer (INV-2).
func TestEveryGatedEventAgainstEveryVerdict(t *testing.T) {
	const command = "rm -rf /tmp/proj/build"
	events := []struct {
		name    string
		hook    string
		payload string
	}{
		{"UserPromptSubmit", "UserPromptSubmit", "user-prompt-submit"},
		{"PreToolUse", "PreToolUse", "pre-tool-use-bash"},
		{"PermissionRequest", "PermissionRequest", "permission-request"},
		{"PreLLMCall", "PreLLMCall", "pre-llm-call"},
	}
	byName := map[string]contractCase{}
	for _, c := range contractCases {
		byName[c.name] = c
	}
	for _, verdict := range []string{"allow", "block", "halt", "require_approval"} {
		for _, ev := range events {
			t.Run(verdict+"/"+ev.name, func(t *testing.T) {
				setHookEnv(t)
				t.Setenv(devconfig.EnvApprovalHold, "50")
				f := serveCore(t, fakecore.Script{Default: verdictJSON(verdict, "because")})
				f.Approval(func(fakecore.Received) (int, string) { return 404, "" })

				stdout, _ := runHook(t, ev.hook, strings.ReplaceAll(fixture(t, ev.payload, "s-"+verdict+"-"+ev.name), "go test ./...", command))
				if strings.Contains(stdout, command) {
					t.Errorf("the answer leaked the tool command: %q", stdout)
				}
				if verdict == "allow" {
					if strings.TrimSpace(stdout) != "" {
						t.Fatalf("an allow must write nothing (a proceed is silence), got %q", stdout)
					}
					return
				}
				c := byName[ev.name]
				m := decodeAnswer(t, []byte(stdout))
				if extra := subset(keysOf(m), c.topKeys); len(extra) > 0 {
					t.Errorf("top-level keys outside the contract: %v in %q", extra, stdout)
				}
				if hso, ok := m["hookSpecificOutput"].(map[string]any); ok {
					if extra := subset(keysOf(hso), c.hsoKeys); len(extra) > 0 {
						t.Errorf("hookSpecificOutput keys outside the contract: %v", extra)
					}
				}
				for _, s := range allStrings(m) {
					if rejected[s] {
						t.Errorf("rendered %q: %s", s, stdout)
					}
				}
				if strings.Count(strings.TrimRight(stdout, "\n"), "\n") != 0 {
					t.Errorf("more than one JSON document on stdout: %q", stdout)
				}
				if len(stdout) >= maxStdoutBytes {
					t.Errorf("stdout is %d bytes", len(stdout))
				}
				// A real HALT latches the run, and only a real HALT does.
				wantLatch := verdict == "halt"
				if got := len(haltLatches(t)) == 1; got != wantLatch {
					t.Errorf("latch written = %v, want %v", got, wantLatch)
				}
			})
		}
	}
}

// Redaction on the proceed path rewrites a write's body and nothing else, and
// rides alone: no permissionDecision, hence no allow.
func TestSecretInAWriteBodyIsRewrittenAsUpdatedInputAlone(t *testing.T) {
	setHookEnv(t)
	serveCore(t, fakecore.Script{Default: allowJSON})
	secret := "AKIA" + "IOSFODNN7" + "EXAMPLE"
	payload := `{"hook_event_name":"PreToolUse","session_id":"s-redact","cwd":"/tmp/proj","tool_name":"Write","tool_use_id":"toolu-9",` +
		`"tool_input":{"file_path":"/tmp/proj/cfg.env","content":"aws = ` + secret + `\nmode = on\n"}}`

	stdout, _ := runHook(t, "PreToolUse", payload)
	if strings.Contains(stdout, secret) {
		t.Fatalf("the rewrite still carries the secret: %q", stdout)
	}
	var got struct {
		HSO struct {
			Name    string          `json:"hookEventName"`
			Perm    string          `json:"permissionDecision"`
			Updated json.RawMessage `json:"updatedInput"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("stdout = %q: %v", stdout, err)
	}
	if got.HSO.Perm != "" {
		t.Errorf("permissionDecision %q beside an updatedInput; that would have to be an allow", got.HSO.Perm)
	}
	var in map[string]string
	if err := json.Unmarshal(got.HSO.Updated, &in); err != nil {
		t.Fatalf("updatedInput = %s: %v", got.HSO.Updated, err)
	}
	if in["file_path"] != "/tmp/proj/cfg.env" || !strings.Contains(in["content"], "mode = on") || strings.Contains(in["content"], secret) {
		t.Errorf("updatedInput = %v", in)
	}
}

// With no secret there is nothing to rewrite, so a proceed is silence.
func TestCleanWriteProceedsInSilence(t *testing.T) {
	setHookEnv(t)
	serveCore(t, fakecore.Script{Default: allowJSON})
	if out, _ := runHook(t, "PreToolUse", fixture(t, "pre-tool-use-write", "s-clean")); strings.TrimSpace(out) != "" {
		t.Fatalf("a clean, allowed write wrote %q", out)
	}
}
