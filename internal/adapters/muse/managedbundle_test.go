package muse

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

const managedBundle = "../../../deployments/managed/muse/"

// The managed hooks file an org deploys and the handlers `init` writes are one
// contract: drift between them would leave a fleet governed differently from a
// developer who ran init.
func TestManagedHooksFileMatchesWhatInstallWrites(t *testing.T) {
	raw, err := os.ReadFile(managedBundle + "hooks.json")
	if err != nil {
		t.Fatal(err)
	}
	managed := strings.ReplaceAll(string(raw), "OPENBOX_BIN", testEngine)
	if strings.Contains(managed, "OPENBOX_BIN") {
		t.Fatal("placeholder survived")
	}
	if _, err := ValidateSettings([]byte(managed)); err != nil {
		t.Fatalf("the managed hooks file does not validate: %v", err)
	}
	doc, err := parseSettings([]byte(managed))
	if err != nil {
		t.Fatal(err)
	}
	if a := auditDoc(doc, ""); len(a.Problems()) != 0 || len(a.Owned) != ExpectedHandlers() {
		t.Errorf("the managed file is not a complete install: %d handlers, problems %v", len(a.Owned), a.Problems())
	}

	i, path := newTestInstaller(t)
	if err := i.Install(testRef); err != nil {
		t.Fatal(err)
	}
	installed, err := parseSettings([]byte(readFile(t, path)))
	if err != nil {
		t.Fatal(err)
	}
	type key struct {
		event, command, successor string
		timeout                   int
	}
	shape := func(d settingsDoc) map[key]bool {
		out := map[key]bool{}
		for _, h := range d.Handlers {
			k := key{event: h.Event, command: h.Command, timeout: h.Timeout}
			if h.OnFailure != nil {
				k.successor = h.OnFailure.Command
			}
			out[k] = true
		}
		return out
	}
	got, want := shape(doc), shape(installed)
	for k := range want {
		if !got[k] {
			t.Errorf("init writes %+v, which the managed file lacks", k)
		}
	}
	if len(got) != len(want) {
		t.Errorf("the managed file has %d handlers, init writes %d", len(got), len(want))
	}
}

func TestManagedPolicyIsJSONThatNamesItsOwnIntent(t *testing.T) {
	raw, err := os.ReadFile(managedBundle + "policy.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"allow_user_approval_override": false`, `"approval_modes"`, `"require_managed_lane": true`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("policy.json lacks %s", want)
		}
	}
	// The envelope the real binary demands of an enterprise plane document: a
	// schema version and the settings under their own key.
	var doc struct {
		SchemaVersion int            `json:"schema_version"`
		Settings      map[string]any `json:"settings"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("policy.json does not decode: %v", err)
	}
	if doc.SchemaVersion != 1 || len(doc.Settings) == 0 {
		t.Errorf("policy.json must be {\"schema_version\":1,\"settings\":{...}}, got version %d with %d settings", doc.SchemaVersion, len(doc.Settings))
	}
	if _, ok := doc.Settings["extensions"]; !ok {
		t.Error("policy.json keeps its guessed keys under settings")
	}
	if readme, err := os.ReadFile(managedBundle + "README-mdm.md"); err != nil {
		t.Fatal(err)
	} else if !strings.Contains(string(readme), "muse config validate --plane policy --file policy.json") {
		t.Error("README-mdm.md does not tell the operator to validate the policy before deploying")
	}
	for _, credential := range []string{"KEY", "TOKEN", "SECRET"} {
		if strings.Contains(strings.ToUpper(string(raw)), credential) {
			t.Errorf("policy.json mentions %s; no credential belongs in a managed file", credential)
		}
	}
	if _, err := ValidateSettings(raw); err != nil {
		t.Errorf("policy.json is not a JSON object: %v", err)
	}
}
