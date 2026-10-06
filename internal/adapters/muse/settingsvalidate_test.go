package muse

import (
	"strings"
	"testing"
)

func TestValidateSettingsAcceptsWhatMuseReads(t *testing.T) {
	for name, doc := range map[string]string{
		"minimal":         `{}`,
		"with schema":     `{"schema_version":1}`,
		"foreign members": `{"schema_version":1,"theme":"dark","model":{"a":[1]},"hooks":{}}`,
		"full handler":    `{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"x","timeout":30,"onFailure":{"type":"command","command":"y","timeout":5}}]}]}}`,
		"async":           `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"x","async":true}]}]}}`,
		"empty groups":    `{"hooks":{"Stop":[]}}`,
	} {
		rep, err := ValidateSettings([]byte(doc))
		if err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if len(rep.Warnings) != 0 {
			t.Errorf("%s: warnings %v", name, rep.Warnings)
		}
	}
}

func TestValidateSettingsReportsTheSchemaVersion(t *testing.T) {
	rep, err := ValidateSettings([]byte(`{"schema_version":1}`))
	if err != nil || rep.SchemaVersion == nil || *rep.SchemaVersion != 1 {
		t.Fatalf("= %+v, %v", rep, err)
	}
	if rep, _ := ValidateSettings([]byte(`{}`)); rep.SchemaVersion != nil {
		t.Error("an absent schema_version read as present")
	}
}

// A key outside the closed schema is a warning, not an error: a newer Muse may
// accept it, and refusing to install over it would be refusing on a guess.
func TestValidateSettingsWarnsOnUnknownsWithoutRefusing(t *testing.T) {
	rep, err := ValidateSettings([]byte(`{"hooks":{"SomeNewEvent":[{"extra":1,"hooks":[{"type":"command","command":"x","surprise":true},{"type":"script","command":"y"}]}]}}`))
	if err != nil {
		t.Fatalf("unknowns refused: %v", err)
	}
	joined := strings.Join(rep.Warnings, "\n")
	for _, want := range []string{"SomeNewEvent", `"extra"`, `"surprise"`, `"script"`} {
		if !strings.Contains(joined, want) {
			t.Errorf("no warning naming %s:\n%s", want, joined)
		}
	}
}

func TestValidateSettingsErrorsNameTheirLocation(t *testing.T) {
	_, err := ValidateSettings([]byte(`{"hooks":{"PreToolUse":[{"hooks":[{"type":"command","command":"x","timeout":0}]}]}}`))
	if err == nil || !strings.Contains(err.Error(), "hooks.PreToolUse[0].hooks[0].timeout") {
		t.Errorf("err = %v", err)
	}
}
