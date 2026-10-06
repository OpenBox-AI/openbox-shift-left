package acceptancetest

import (
	"sort"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/conformance"
)

// TestSchemaEnumMatchesClientConstants the event vocabulary is declared in
// several places by necessity: the JSON Schema is the published contract,
// client.AllEventTypes is what the code emits, the conformance module keeps
// its own copy so it can stay dependency-free, and this module orders the
// types into a coherent session.
func TestSchemaEnumMatchesClientConstants(t *testing.T) {
	schema, err := conformance.LoadSchema()
	if err != nil {
		t.Fatalf("load schema: %v", err)
	}
	props, _ := schema["properties"].(map[string]any)
	et, _ := props["event_type"].(map[string]any)
	enum, _ := et["enum"].([]any)
	if len(enum) == 0 {
		t.Fatal("schema declares no event_type enum")
	}

	fromSchema := make([]string, 0, len(enum))
	for _, e := range enum {
		s, ok := e.(string)
		if !ok {
			t.Fatalf("schema enum holds a non-string value %v", e)
		}
		fromSchema = append(fromSchema, s)
	}

	fromClient := make([]string, 0, len(client.AllEventTypes))
	for _, t := range client.AllEventTypes {
		fromClient = append(fromClient, string(t))
	}

	assertSameSet(t, "schema event_type enum", fromSchema, "client.AllEventTypes", fromClient)
}

// TestSessionOrderingCoversWholeVocabulary devEventTypes deliberately orders
// the lifecycle for a coherent session, so it is not simply
// client.AllEventTypes.
func TestSessionOrderingCoversWholeVocabulary(t *testing.T) {
	ordered := make([]string, 0, len(devEventTypes))
	for _, t := range devEventTypes {
		ordered = append(ordered, string(t))
	}
	all := make([]string, 0, len(client.AllEventTypes))
	for _, t := range client.AllEventTypes {
		all = append(all, string(t))
	}
	assertSameSet(t, "acceptance devEventTypes", ordered, "client.AllEventTypes", all)
}

func assertSameSet(t *testing.T, aName string, a []string, bName string, b []string) {
	t.Helper()
	index := func(list []string) map[string]bool {
		m := make(map[string]bool, len(list))
		for _, s := range list {
			m[s] = true
		}
		return m
	}
	inA, inB := index(a), index(b)
	var onlyA, onlyB []string
	for _, s := range a {
		if !inB[s] {
			onlyA = append(onlyA, s)
		}
	}
	for _, s := range b {
		if !inA[s] {
			onlyB = append(onlyB, s)
		}
	}
	sort.Strings(onlyA)
	sort.Strings(onlyB)
	if len(onlyA) > 0 {
		t.Errorf("in %s but not %s: %v", aName, bName, onlyA)
	}
	if len(onlyB) > 0 {
		t.Errorf("in %s but not %s: %v", bName, aName, onlyB)
	}
}
