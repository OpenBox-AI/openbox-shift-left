package snapshot

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestOmissionsPreserveEmptyArrays(t *testing.T) {
	snapshot := &Snapshot{omissions: []Omission{{Examples: []string{}}}}
	omissions := snapshot.Omissions()
	encoded, err := json.Marshal(omissions)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(encoded, []byte(`"examples":[]`)) || bytes.Contains(encoded, []byte(`"examples":null`)) {
		t.Fatalf("empty omission examples encoded as %s", encoded)
	}
}
