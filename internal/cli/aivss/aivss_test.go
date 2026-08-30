package aivss

import (
	"encoding/json"
	"testing"
)

func TestDefaultProfileWithinBounds(t *testing.T) {
	if field, ok := DefaultDeveloperProfile().Validate(); !ok {
		t.Fatalf("default profile field %s is out of the backend DTO bounds", field)
	}
}

func TestValidateCatchesOutOfRange(t *testing.T) {
	c := DefaultDeveloperProfile()
	c.BaseSecurity.AttackComplexity = 3 // DTO max is 2
	field, ok := c.Validate()
	if ok || field != "base_security.attack_complexity" {
		t.Fatalf("expected attack_complexity breach, got field=%q ok=%v", field, ok)
	}
}

func TestConfigMarshalsSnakeCase(t *testing.T) {
	b, err := json.Marshal(DefaultDeveloperProfile())
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]map[string]int
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, group := range []string{"base_security", "ai_specific", "impact"} {
		if _, ok := m[group]; !ok {
			t.Errorf("missing group %q in marshaled aivss_config", group)
		}
	}
	if m["ai_specific"]["data_sensitivity"] != 4 || m["impact"]["safety_impact"] != 1 {
		t.Errorf("unexpected posture values: %v", m)
	}
}
