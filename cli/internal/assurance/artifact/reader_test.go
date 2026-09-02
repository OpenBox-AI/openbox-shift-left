package artifact

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestParseManifestIndexVerifiesAssembledObjects(t *testing.T) {
	pack, err := AssemblePack(testManifestInput(t, "run-reader", "1.2.3", false))
	if err != nil {
		t.Fatal(err)
	}
	index, err := ParseManifestIndex(pack.Manifest())
	if err != nil {
		t.Fatal(err)
	}
	if index.Digest() != pack.Digest() || !bytes.Equal(index.Manifest(), pack.Manifest()) {
		t.Fatal("parsed manifest identity changed")
	}
	objects := make(map[Role]Object)
	for _, object := range pack.Objects() {
		objects[object.Role()] = object
	}
	for _, reference := range index.References() {
		if err := index.VerifyObject(reference, objects[reference.Role()].Bytes()); err != nil {
			t.Fatalf("verify %s: %v", reference.Role(), err)
		}
	}
	if len(index.References()) != len(pack.Objects()) {
		t.Fatalf("references = %d, want %d", len(index.References()), len(pack.Objects()))
	}
}

func TestParseManifestIndexRejectsStructuralAndBindingMutations(t *testing.T) {
	pack, err := AssemblePack(testManifestInput(t, "run-reader", "1.2.3", false))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseManifestIndex(append(pack.Manifest(), '\n')); err == nil {
		t.Fatal("accepted noncanonical manifest")
	}
	for _, test := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "unknown root", mutate: func(root map[string]any) { root["unknown"] = true }},
		{name: "missing role", mutate: func(root map[string]any) { delete(root["objects"].(map[string]any), string(RoleEffectEvents)) }},
		{name: "wrong media", mutate: func(root map[string]any) {
			root["objects"].(map[string]any)[string(RoleProjectModel)].(map[string]any)["mediaType"] = "text/plain"
		}},
		{name: "judgment mismatch", mutate: func(root map[string]any) { root["judgments"] = []any{} }},
		{name: "unretained payload", mutate: func(root map[string]any) {
			root["objects"].(map[string]any)[string(RoleSDKEvents)].(map[string]any)["retention"] = "digest_only"
		}},
		{name: "missing schema digest", mutate: func(root map[string]any) {
			delete(root["schemas"].([]any)[0].(map[string]any), "digest")
		}},
		{name: "missing explicit schema", mutate: func(root map[string]any) {
			delete(root["objects"].(map[string]any)[string(RoleSDKEvents)].(map[string]any), "schema")
		}},
		{name: "missing bytes", mutate: func(root map[string]any) {
			delete(root["objects"].(map[string]any)[string(RoleSDKEvents)].(map[string]any), "bytes")
		}},
		{name: "missing retention flag", mutate: func(root map[string]any) {
			delete(root["retention"].(map[string]any), "rawContentPersisted")
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var root map[string]any
			if err := json.Unmarshal(pack.Manifest(), &root); err != nil {
				t.Fatal(err)
			}
			test.mutate(root)
			mutated, err := CanonicalJSON(root)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := ParseManifestIndex(mutated); err == nil {
				t.Fatal("accepted mutated manifest")
			}
		})
	}
}

func TestManifestIndexRejectsChangedObjectBytes(t *testing.T) {
	pack, err := AssemblePack(testManifestInput(t, "run-reader", "1.2.3", false))
	if err != nil {
		t.Fatal(err)
	}
	index, err := ParseManifestIndex(pack.Manifest())
	if err != nil {
		t.Fatal(err)
	}
	reference := index.References()[0]
	if err := index.VerifyObject(reference, []byte(`{}`)); err == nil {
		t.Fatal("accepted changed object bytes")
	}
}
