package codex

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
)

func testInstaller(t *testing.T) (Installer, string, string) {
	t.Helper()
	hooks := filepath.Join(t.TempDir(), "codex", "hooks.json")
	cfg := filepath.Join(t.TempDir(), "openbox", "dev.json")
	return Installer{HooksPath: hooks, ConfigPath: cfg, EngineBinary: "/opt/openbox/bin/openbox"}, hooks, cfg
}

func readHooks(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read hooks.json: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("parse hooks.json: %v\n%s", err, raw)
	}
	return m
}

func TestInstaller_WritesHooksAndConfig(t *testing.T) {
	inst, hooksPath, cfgPath := testInstaller(t)
	if inst.Name() != "codex" {
		t.Errorf("Name = %q", inst.Name())
	}
	if !inst.Available() {
		t.Error("adapter must report Available()==true (not the SL-2 stub)")
	}

	ref := CredentialRef{
		DID: testDID,
	}
	if err := inst.Install(ref); err != nil {
		t.Fatalf("install: %v", err)
	}

	m := readHooks(t, hooksPath)
	hooks := m["hooks"].(map[string]any)
	for _, ev := range []string{"SessionStart", "UserPromptSubmit", "PreToolUse", "PostToolUse", "SessionEnd"} {
		groups, ok := hooks[ev].([]any)
		if !ok || len(groups) != 1 {
			t.Fatalf("event %s: want exactly one matcher group, got %v", ev, hooks[ev])
		}
		group := groups[0].(map[string]any)
		handlers := group["hooks"].([]any)
		if len(handlers) != 1 {
			t.Fatalf("event %s: want one handler, got %d", ev, len(handlers))
		}
		h := handlers[0].(map[string]any)
		if h["type"] != "command" {
			t.Errorf("event %s: handler type = %v", ev, h["type"])
		}
		wantCmd := `"/opt/openbox/bin/openbox" hook codex ` + ev
		if h["command"] != wantCmd {
			t.Errorf("event %s: command = %q, want %q", ev, h["command"], wantCmd)
		}
		wantTimeout := float64(hotHookTimeoutSec)
		switch ev {
		case "SessionEnd":
			wantTimeout = float64(sessionEndHookTimeoutSec)
		case "PreToolUse":
			wantTimeout = float64(preToolUseHookTimeoutSec)
		}
		if h["timeout"] != wantTimeout {
			t.Errorf("event %s: timeout = %v, want %v", ev, h["timeout"], wantTimeout)
		}
		_, hasMatcher := group["matcher"]
		if ev == "PreToolUse" || ev == "PostToolUse" {
			if group["matcher"] != "*" {
				t.Errorf("event %s: matcher = %v, want \"*\"", ev, group["matcher"])
			}
		} else if hasMatcher {
			t.Errorf("event %s: lifecycle hook should omit matcher, got %v", ev, group["matcher"])
		}
	}

	cfg, err := devconfig.Load(cfgPath)
	if err != nil || cfg.DID != testDID {
		t.Errorf("dev config wrong: %+v (err %v)", cfg, err)
	}
}

// TestInstaller_NoSecretInWrittenFiles iNV-1 (story NFR "Security"): neither
// written file may carry a secret; and hooks.json specifically carries the
// engine path + event names only; no key, DID, or URL.
func TestInstaller_NoSecretInWrittenFiles(t *testing.T) {
	inst, hooksPath, cfgPath := testInstaller(t)
	ref := CredentialRef{
		DID:     testDID,
		BaseURL: "https://core.example.ai",
	}
	if err := inst.Install(ref); err != nil {
		t.Fatalf("install: %v", err)
	}

	rawHooks, _ := os.ReadFile(hooksPath)
	for _, banned := range []string{"obx_", testDID, "core.example.ai", "api_key", "openbox-dev"} {
		if strings.Contains(string(rawHooks), banned) {
			t.Errorf("hooks.json must carry engine path + events only; found %q:\n%s", banned, rawHooks)
		}
	}
	rawCfg, _ := os.ReadFile(cfgPath)
	if strings.Contains(string(rawCfg), "obx_") {
		t.Errorf("dev config leaked a credential value: %s", rawCfg)
	}
}

// TestInstaller_IdempotentAndPreservesForeignEntries aC-10: double-install
// never duplicates (byte-identical), and a pre-existing foreign hooks.json
// entry; including a whole foreign event; is preserved untouched.
func TestInstaller_IdempotentAndPreservesForeignEntries(t *testing.T) {
	inst, hooksPath, _ := testInstaller(t)

	pre := `{
	  "description": "my hooks",
	  "hooks": {
	    "PreToolUse": [
	      {"matcher": "Bash", "hooks": [
	        {"type": "command", "command": "/usr/local/bin/my-linter --check", "timeout": 30, "statusMessage": "linting"},
	        {"type": "command", "command": "\"$CLAUDE_PLUGIN_ROOT/bin/openbox\" hook claude-code PreToolUse", "timeout": 5}
	      ]}
	    ],
	    "Stop": [
	      {"hooks": [{"type": "command", "command": "/usr/local/bin/notify-done"}]}
	    ]
	  }
	}`
	if err := os.MkdirAll(filepath.Dir(hooksPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hooksPath, []byte(pre), 0o600); err != nil {
		t.Fatal(err)
	}

	ref := CredentialRef{DID: testDID}
	if err := inst.Install(ref); err != nil {
		t.Fatalf("install: %v", err)
	}
	first, _ := os.ReadFile(hooksPath)

	m := readHooks(t, hooksPath)
	hooks := m["hooks"].(map[string]any)
	if m["description"] != "my hooks" {
		t.Errorf("foreign description clobbered: %v", m["description"])
	}
	if raw, _ := json.Marshal(hooks["Stop"]); !strings.Contains(string(raw), "notify-done") {
		t.Errorf("foreign Stop event lost: %s", raw)
	}
	rawPre, _ := json.Marshal(hooks["PreToolUse"])
	s := string(rawPre)
	if !strings.Contains(s, "my-linter") || !strings.Contains(s, "statusMessage") {
		t.Errorf("foreign PreToolUse handler (or its unknown fields) lost: %s", s)
	}
	if strings.Contains(s, "hook claude-code") {
		t.Errorf("mangled claude-code import not superseded: %s", s)
	}
	if strings.Count(s, "hook codex PreToolUse") != 1 {
		t.Errorf("want exactly one openbox codex PreToolUse handler: %s", s)
	}

	if err := inst.Install(ref); err != nil {
		t.Fatalf("re-install: %v", err)
	}
	second, _ := os.ReadFile(hooksPath)
	if string(first) != string(second) {
		t.Errorf("hooks.json not byte-identical across re-install:\n%s\n---\n%s", first, second)
	}
}

// TestInstaller_AnchoredOwnershipNeverClaimsCompoundHandlers g_SEC SL7-A F1
// (adversarial): a foreign compound/wrapper handler that merely embeds an
// openbox invocation must never be claimed; it survives re-install byte-for-
// byte alongside exactly one genuine OpenBox entry, while an exact stale
// OpenBox invocation (different engine path) is still owned and replaced.
func TestInstaller_AnchoredOwnershipNeverClaimsCompoundHandlers(t *testing.T) {
	inst, hooksPath, _ := testInstaller(t)

	pre := `{
	  "hooks": {
	    "PreToolUse": [
	      {"matcher": "*", "hooks": [
	        {"type": "command", "command": "my-audit-log --record && \"/usr/local/bin/openbox\" hook codex PreToolUse", "timeout": 30},
	        {"type": "command", "command": "\"/old/engine/path/openbox\" hook codex PreToolUse", "timeout": 5},
	        {"type": "command", "command": "sh -c 'echo hook codex PreToolUse'", "timeout": 5}
	      ]}
	    ]
	  }
	}`
	if err := os.MkdirAll(filepath.Dir(hooksPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hooksPath, []byte(pre), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := inst.Install(CredentialRef{DID: testDID}); err != nil {
		t.Fatalf("install: %v", err)
	}
	first, _ := os.ReadFile(hooksPath)
	s := string(first)

	for _, foreign := range []string{"my-audit-log --record", "/usr/local/bin/openbox", "sh -c 'echo hook codex PreToolUse'"} {
		if !strings.Contains(s, foreign) {
			t.Errorf("foreign handler embedding an openbox invocation was claimed/deleted (want %q kept):\n%s", foreign, s)
		}
	}
	if strings.Contains(s, "/old/engine/path/openbox") {
		t.Errorf("stale exact OpenBox invocation not superseded:\n%s", s)
	}
	if n := strings.Count(s, `\"/opt/openbox/bin/openbox\" hook codex PreToolUse`); n != 1 {
		t.Errorf("want exactly 1 fresh OpenBox PreToolUse handler, got %d:\n%s", n, s)
	}

	if err := inst.Install(CredentialRef{DID: testDID}); err != nil {
		t.Fatalf("re-install: %v", err)
	}
	second, _ := os.ReadFile(hooksPath)
	if string(first) != string(second) {
		t.Errorf("re-install not byte-identical with a compound foreign handler present:\n%s\n---\n%s", first, second)
	}
}

func TestIsOpenBoxHandler_AnchoredShapes(t *testing.T) {
	owned := []string{
		`{"type":"command","command":"\"/opt/x/openbox\" hook codex PreToolUse"}`,
		`{"type":"command","command":"openbox hook codex SessionEnd"}`,
		`{"type":"command","command":"\"$CLAUDE_PLUGIN_ROOT/bin/openbox\" hook claude-code PreToolUse"}`, // mangled migration import
		`{"type":"command","command":"  \"/opt/x/openbox\" hook codex PreToolUse  "}`,                    // whitespace-tolerant
	}
	foreign := []string{
		`{"type":"command","command":"audit && \"/opt/x/openbox\" hook codex PreToolUse"}`, // compound
		`{"type":"command","command":"\"/opt/x/openbox\" hook codex PreToolUse; notify"}`,  // trailing command
		`{"type":"command","command":"\"/opt/x/openbox\" hook codex PreToolUse --extra"}`,  // extra args
		`{"type":"command","command":"sh -c 'openbox hook codex PreToolUse'"}`,             // wrapped
		`{"type":"command","command":"\"unterminated hook codex PreToolUse"}`,              // bad quote
		`{"type":"command","command":"openbox"}`,                                           // bare token
		`{"type":"prompt","command":"openbox hook codex PreToolUse"}`,                      // wrong type
		`{"type":"command","command":"openbox hook cursor PreToolUse"}`,                    // wrong provider
	}
	for _, c := range owned {
		if !isOpenBoxHandler(json.RawMessage(c)) {
			t.Errorf("want OWNED: %s", c)
		}
	}
	for _, c := range foreign {
		if isOpenBoxHandler(json.RawMessage(c)) {
			t.Errorf("want FOREIGN (kept): %s", c)
		}
	}
}

func TestInstaller_RefusesUnparsableHooksFile(t *testing.T) {
	inst, hooksPath, _ := testInstaller(t)
	if err := os.MkdirAll(filepath.Dir(hooksPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hooksPath, []byte("{corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := inst.Install(CredentialRef{DID: testDID})
	if err == nil {
		t.Fatal("install must refuse to clobber an unparsable hooks.json")
	}
	if got, _ := os.ReadFile(hooksPath); string(got) != "{corrupt" {
		t.Errorf("unparsable file was modified: %q", got)
	}
}

// TestInstaller_PersistsEnforcePosture that decision parity: the enforce
// posture chosen at `init` time persists into dev.json so the SL7-B enforce
// leg needs no new install surface.
func TestInstaller_PersistsEnforcePosture(t *testing.T) {
	inst, _, cfgPath := testInstaller(t)
	tru := true
	ref := CredentialRef{
		DID:     testDID,
		Enforce: &tru, Tier2: &tru, Findings: &tru,
	}
	if err := inst.Install(ref); err != nil {
		t.Fatalf("install: %v", err)
	}
	cfg, err := devconfig.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Enforce == nil || !*cfg.Enforce || cfg.Tier2 == nil || !*cfg.Tier2 || cfg.Findings == nil || !*cfg.Findings {
		t.Errorf("enforce posture not persisted: %+v", cfg)
	}
}

// TestInstaller_PreservesPriorSyncCoordinates re-init preserves previously-
// persisted sync coordinates (agent_id / backend_url) when the ref does not
// carry them; the idempotent reuse path.
func TestInstaller_PreservesPriorSyncCoordinates(t *testing.T) {
	inst, _, cfgPath := testInstaller(t)
	if err := inst.Install(CredentialRef{DID: testDID, AgentID: "agent-1", BackendURL: "https://backend.example.ai"}); err != nil {
		t.Fatal(err)
	}
	if err := inst.Install(CredentialRef{DID: testDID}); err != nil {
		t.Fatal(err)
	}
	cfg, _ := devconfig.Load(cfgPath)
	if cfg.AgentID != "agent-1" || cfg.BackendURL != "https://backend.example.ai" {
		t.Errorf("re-init dropped sync coordinates: %+v", cfg)
	}
}

func TestInstaller_Plan(t *testing.T) {
	inst := Installer{HooksPath: "/x/hooks.json", ConfigPath: "/x/dev.json", EngineBinary: "/x/openbox"}
	plan := inst.Plan(CredentialRef{DID: testDID})
	for _, want := range []string{"/hooks", "trust", testDID, "0.145.0", "CODEX_THREAD_ID", "hook codex"} {
		if !strings.Contains(plan, want) {
			t.Errorf("plan missing %q:\n%s", want, plan)
		}
	}
	if strings.Contains(plan, "obx_") {
		t.Errorf("plan leaked a credential: %s", plan)
	}
}

func TestInstaller_RequiresDID(t *testing.T) {
	inst, _, _ := testInstaller(t)
	if err := inst.Install(CredentialRef{}); err == nil {
		t.Error("install without a DID should error")
	}
}

// TestInstaller_HookCommandShapes the packaging fallback (no EngineBinary)
// resolves `openbox` on PATH, unquoted; the resolved-engine path is quoted
// against spaces.
func TestInstaller_HookCommandShapes(t *testing.T) {
	withEngine := Installer{EngineBinary: "/opt/open box/openbox"}
	if got := withEngine.hookCommand("PreToolUse"); got != `"/opt/open box/openbox" hook codex PreToolUse` {
		t.Errorf("quoted command = %q", got)
	}
	bare := Installer{}
	if got := bare.hookCommand("PreToolUse"); got != "openbox hook codex PreToolUse" {
		t.Errorf("fallback command = %q", got)
	}
}

// TestInstaller_ReInitKeepsEnforcePosture parity with the CC adapter: a re-
// init that says nothing about posture leaves it alone.
func TestInstaller_ReInitKeepsEnforcePosture(t *testing.T) {
	inst, _, cfgPath := testInstaller(t)
	tru := true

	if err := inst.Install(CredentialRef{
		DID:     testDID,
		AgentID: "agent-1", BackendURL: "https://backend.example",
		Enforce: &tru, Tier2: &tru, Findings: &tru,
	}); err != nil {
		t.Fatalf("install: %v", err)
	}
	if err := inst.Install(CredentialRef{
		DID: testDID,
	}); err != nil {
		t.Fatalf("re-install: %v", err)
	}

	cfg, err := devconfig.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Enforce == nil || !*cfg.Enforce || cfg.Tier2 == nil || !*cfg.Tier2 || cfg.Findings == nil || !*cfg.Findings {
		t.Errorf("re-init downgraded the enforce posture: %+v", cfg)
	}
	if cfg.AgentID != "agent-1" || cfg.BackendURL != "https://backend.example" {
		t.Errorf("re-init dropped the sync coordinates: %+v", cfg)
	}
}
