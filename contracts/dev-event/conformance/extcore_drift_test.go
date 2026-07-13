package conformance

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// STORY-SL-13 drift guard. Keeps the EXT-core patch artifact
// (../ext-core/) honest against the SL-1 contract: the developer event types the
// patch teaches openbox-core to accept-list MUST be exactly the types the SL-1
// contract emits, and the patch must actually mention each of them. A type can
// therefore never appear in what shift-left emits without core being taught to
// accept it (INV-8, strictly additive). These tests are dependency-free and run
// offline under `cd contracts/dev-event/conformance && go test ./...`.

const (
	extCoreTypesRelPath = "../ext-core/dev-event-types.json"
	extCorePatchRelPath = "../ext-core/openbox-core-dev-event-types.patch"
)

// extCoreType mirrors one entry of dev-event-types.json's `types` array.
type extCoreType struct {
	EventType string `json:"event_type"`
	GoConst   string `json:"go_const"`
	Lifecycle string `json:"lifecycle"`
}

type extCoreArtifact struct {
	Patch string        `json:"patch"`
	Types []extCoreType `json:"types"`
}

// resolveRel resolves a path relative to this source file (CWD-independent).
func resolveRel(rel string) string {
	_, thisFile, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(thisFile), rel)
}

func loadExtCoreArtifact(t *testing.T) extCoreArtifact {
	t.Helper()
	raw, err := os.ReadFile(resolveRel(extCoreTypesRelPath))
	if err != nil {
		t.Fatalf("read ext-core artifact: %v", err)
	}
	var a extCoreArtifact
	if err := json.Unmarshal(raw, &a); err != nil {
		t.Fatalf("parse ext-core artifact: %v", err)
	}
	if len(a.Types) == 0 {
		t.Fatal("ext-core artifact declares no types")
	}
	return a
}

// schemaEventTypeEnum extracts the SL-1 contract's event_type enum — the source
// of truth the artifact is checked against.
func schemaEventTypeEnum(t *testing.T) []string {
	t.Helper()
	doc, err := LoadSchema()
	if err != nil {
		t.Fatalf("load SL-1 schema: %v", err)
	}
	props, ok := doc["properties"].(map[string]any)
	if !ok {
		t.Fatal("schema has no properties object")
	}
	et, ok := props["event_type"].(map[string]any)
	if !ok {
		t.Fatal("schema has no event_type property")
	}
	rawEnum, ok := et["enum"].([]any)
	if !ok {
		t.Fatal("schema event_type has no enum")
	}
	out := make([]string, 0, len(rawEnum))
	for _, v := range rawEnum {
		s, ok := v.(string)
		if !ok {
			t.Fatalf("non-string event_type enum value: %v", v)
		}
		out = append(out, s)
	}
	return out
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// TestExtCoreArtifactMatchesSL1Enum is the drift guard: the artifact's type list
// must equal the SL-1 contract's event_type enum, set-for-set. Adding a type in
// one place and not the other fails here.
func TestExtCoreArtifactMatchesSL1Enum(t *testing.T) {
	art := loadExtCoreArtifact(t)

	artifactTypes := make([]string, len(art.Types))
	for i, ty := range art.Types {
		if ty.EventType == "" {
			t.Fatalf("artifact type %d has empty event_type", i)
		}
		artifactTypes[i] = ty.EventType
	}

	schemaTypes := schemaEventTypeEnum(t)

	got, want := sortedCopy(artifactTypes), sortedCopy(schemaTypes)
	if len(got) != len(want) {
		t.Fatalf("drift: artifact has %d types %v, SL-1 enum has %d %v — the EXT-core patch and the contract disagree; reconcile ext-core/dev-event-types.json with schema/dev-event.schema.json",
			len(got), got, len(want), want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("drift: artifact types %v != SL-1 enum %v — a type shift-left emits would not be accept-listed (or vice versa); reconcile ext-core/dev-event-types.json with schema/dev-event.schema.json (INV-8)",
				got, want)
		}
	}
}

// TestExtCorePatchCoversEveryType asserts each declared type literally appears in
// the patch, so the patch can never silently omit a type the artifact promises
// core will accept.
func TestExtCorePatchCoversEveryType(t *testing.T) {
	art := loadExtCoreArtifact(t)

	patchRaw, err := os.ReadFile(resolveRel(extCorePatchRelPath))
	if err != nil {
		t.Fatalf("read ext-core patch: %v", err)
	}
	patch := string(patchRaw)

	for _, ty := range art.Types {
		// The constant name and the wire string both must be present on an added
		// (`+`) line — the patch adds `EventTypeX = "X"` and lists it in the switch.
		if !strings.Contains(patch, ty.GoConst) {
			t.Errorf("patch is missing the constant %q for event_type %q — regenerate openbox-core-dev-event-types.patch (STORY-SL-13 stop condition)", ty.GoConst, ty.EventType)
		}
		if !strings.Contains(patch, `"`+ty.EventType+`"`) {
			t.Errorf("patch is missing the wire string %q — regenerate openbox-core-dev-event-types.patch", ty.EventType)
		}
	}
}
