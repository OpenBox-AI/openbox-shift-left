package acceptancetest

import (
	"sort"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/conformance"
)

// The event vocabulary is declared in several places by necessity: the JSON
// Schema is the published contract, client.AllEventTypes is what the code emits,
// the conformance module keeps its own copy so it can stay dependency-free, and
// this module orders the types into a coherent session. Nothing bound them
// together — the one cross-check compared list *lengths*, so renaming a type on
// one side left every test green.
//
// This module is the only one that can see both the schema and the client, so
// the binding lives here. conformance binds its own list to the schema, so the
// three declarations are pinned transitively.
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

// devEventTypes deliberately orders the lifecycle for a coherent session, so it
// is not simply client.AllEventTypes. It must still cover exactly the same set —
// a type added to the vocabulary but missing here would go un-exercised against
// a stock core, which is the whole point of this module.
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

// assertSameSet reports each side's extras, so a rename reads as one added and
// one removed name rather than an opaque mismatch.
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
